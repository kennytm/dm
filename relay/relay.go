package relay

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/tidb-enterprise-tools/dm/config"
	"github.com/pingcap/tidb-enterprise-tools/dm/pb"
	"github.com/pingcap/tidb-enterprise-tools/dm/unit"
	pkgstreamer "github.com/pingcap/tidb-enterprise-tools/pkg/streamer"
	"github.com/pingcap/tidb-enterprise-tools/pkg/utils"
	"github.com/siddontang/go-mysql/mysql"
	"github.com/siddontang/go-mysql/replication"
	"github.com/siddontang/go/sync2"
	"golang.org/x/net/context"
)

// errors used by relay
var (
	ErrBinlogPosGreaterThanFileSize = errors.New("the specific position is greater than the local binlog file size")
	// for MariaDB, UUID set as `gtid_domain_id` + domainServerIDSeparator + `server_id`
	domainServerIDSeparator = "-"
)

const (
	eventTimeout                = 1 * time.Hour
	flushMetaInterval           = 30 * time.Second
	binlogHeaderSize            = 4
	showStatusConnectionTimeout = "1m"
)

// Relay relays mysql binlog to local file.
type Relay struct {
	db                    *sql.DB
	cfg                   *Config
	syncer                *replication.BinlogSyncer
	syncerCfg             replication.BinlogSyncerConfig
	meta                  Meta
	lastSlaveConnectionID uint32
	fd                    *os.File
	closed                sync2.AtomicBool
	sync.RWMutex
}

// NewRelay creates an instance of Relay.
func NewRelay(cfg *Config) *Relay {
	syncerCfg := replication.BinlogSyncerConfig{
		ServerID:       uint32(cfg.ServerID),
		Flavor:         cfg.Flavor,
		Host:           cfg.From.Host,
		Port:           uint16(cfg.From.Port),
		User:           cfg.From.User,
		Password:       cfg.From.Password,
		Charset:        cfg.Charset,
		UseDecimal:     true, // must set true. ref: https://github.com/pingcap/tidb-enterprise-tools/pull/272
		VerifyChecksum: true,
	}
	if !cfg.EnableGTID {
		// for rawMode(true), we only parse FormatDescriptionEvent and RotateEvent
		// if not need to support GTID mode, we can enable rawMode
		syncerCfg.RawModeEnabled = true
	}
	return &Relay{
		cfg:       cfg,
		syncer:    replication.NewBinlogSyncer(syncerCfg),
		syncerCfg: syncerCfg,
		meta:      NewLocalMeta(cfg.Flavor, cfg.RelayDir),
	}
}

// Init implements the dm.Unit interface.
func (r *Relay) Init() error {
	cfg := r.cfg.From
	dbDSN := fmt.Sprintf("%s:%s@tcp(%s:%d)/?charset=utf8mb4&interpolateParams=true&readTimeout=%s", cfg.User, cfg.Password, cfg.Host, cfg.Port, showStatusConnectionTimeout)
	db, err := sql.Open("mysql", dbDSN)
	if err != nil {
		return errors.Trace(err)
	}
	r.db = db

	if err := os.MkdirAll(r.cfg.RelayDir, 0755); err != nil {
		return errors.Trace(err)
	}

	err = r.meta.Load()
	if err != nil {
		return errors.Trace(err)
	}

	if err := reportRelayLogSpaceInBackground(r.cfg.RelayDir); err != nil {
		return errors.Trace(err)
	}

	return nil
}

// Process implements the dm.Unit interface.
func (r *Relay) Process(ctx context.Context, pr chan pb.ProcessResult) {
	errs := make([]*pb.ProcessError, 0, 1)
	err := r.process(ctx)
	if err != nil && errors.Cause(err) != replication.ErrSyncClosed {
		relayExitWithErrorCounter.Inc()
		log.Errorf("[relay] process exit with error %v", errors.ErrorStack(err))
		// TODO: add specified error type instead of pb.ErrorType_UnknownError
		errs = append(errs, unit.NewProcessError(pb.ErrorType_UnknownError, errors.ErrorStack(err)))
	}

	isCanceled := false
	if len(errs) == 0 {
		select {
		case <-ctx.Done():
			isCanceled = true
		default:
		}
	}
	pr <- pb.ProcessResult{
		IsCanceled: isCanceled,
		Errors:     errs,
	}
}

// SwitchMaster switches relay's master server
// before call this from dmctl, you must ensure that relay catches up previous master
// we can not check this automatically in this func because master already changed
// switch master server steps:
//   1. use dmctl to pause relay, TODO zxc
//   2. ensure relay catching up current master server (use `query-status`)
//   3. switch master server for upstream
//      * change relay's master config, TODO
//      * change master behind VIP
//   4. use dmctl to switch relay's master server (use `switch-relay-master`)
//   5. use dmctl to resume relay, TODO zxc
func (r *Relay) SwitchMaster(ctx context.Context, req *pb.SwitchRelayMasterRequest) error {
	// TODO zxc: check relay's stage when Pause / Resume supported
	if !r.cfg.EnableGTID {
		return errors.New("can only switch relay's master server when GTID enabled")
	}
	err := r.reSetupMeta()
	return errors.Trace(err)
}

func (r *Relay) process(parentCtx context.Context) error {
	if len(r.meta.UUID()) == 0 {
		// no current UUID set, try set one (re-setup meta)
		err := r.reSetupMeta()
		if err != nil {
			return errors.Trace(err)
		}
	}

	streamer, err := r.getBinlogStreamer()
	if err != nil {
		return errors.Trace(err)
	}

	var (
		_, lastPos  = r.meta.Pos()
		_, lastGTID = r.meta.GTID()
		masterNode  = r.masterNode()
		masterUUID  = r.meta.UUID() // only change after switch
		tryReSync   = true          // used to handle master-slave switch
	)
	defer func() {
		if r.fd != nil {
			r.fd.Close()
		}
	}()

	go r.flushMetaAtIntervals(parentCtx)

	for {
		ctx, cancel := context.WithTimeout(parentCtx, eventTimeout)
		readTimer := time.Now()
		e, err := streamer.GetEvent(ctx)
		cancel()
		binlogReadDurationHistogram.Observe(time.Since(readTimer).Seconds())

		if err != nil {
			switch errors.Cause(err) {
			case context.Canceled:
				return nil
			case context.DeadlineExceeded:
				log.Infof("[relay] deadline %s exceeded, no binlog event received", eventTimeout)
				continue
			case replication.ErrChecksumMismatch:
				relayLogDataCorruptionCounter.Inc()
			case replication.ErrSyncClosed, replication.ErrNeedSyncAgain:
				// do nothing
			default:
				if utils.IsErrBinlogPurged(err) {
					if tryReSync && r.cfg.EnableGTID && r.cfg.AutoFixGTID {
						streamer, err = r.reSyncBinlog(r.syncerCfg)
						if err != nil {
							return errors.Annotatef(err, "try auto switch with GTID")
						}
						tryReSync = false // do not support repeat try re-sync
						continue
					}
				}
				binlogReadErrorCounter.Inc()
			}
			return errors.Trace(err)
		}
		tryReSync = true

		log.Debugf("[relay] receive binlog event with header %v", e.Header)
		switch ev := e.Event.(type) {
		case *replication.FormatDescriptionEvent:
			// FormatDescriptionEvent is the first event in binlog, we will close old one and create a new
			exist, err := r.handleFormatDescriptionEvent(lastPos.Name)
			if err != nil {
				return errors.Trace(err)
			}
			if exist {
				// exists previously, skip
				continue
			}
		case *replication.RotateEvent:
			// for RotateEvent, update binlog name
			currentPos := mysql.Position{
				Name: string(ev.NextLogName),
				Pos:  uint32(ev.Position),
			}
			if currentPos.Compare(lastPos) == 1 {
				lastPos = currentPos
			}
			log.Infof("[relay] rotate to %s", lastPos.String())
			if e.Header.Timestamp == 0 || e.Header.LogPos == 0 {
				// skip fake rotate event
				continue
			}
		case *replication.QueryEvent:
			// when RawModeEnabled not true, QueryEvent will be parsed
			// even for `BEGIN`, we still update pos / GTID
			lastPos.Pos = e.Header.LogPos
			lastGTID.Set(ev.GSet) // in order to call `ev.GSet`, can not combine QueryEvent and XIDEvent
		case *replication.XIDEvent:
			// when RawModeEnabled not true, XIDEvent will be parsed
			lastPos.Pos = e.Header.LogPos
			lastGTID.Set(ev.GSet)
		}

		if !r.cfg.EnableGTID {
			// not need support GTID mode (rawMode enabled), update pos for all events
			lastPos.Pos = e.Header.LogPos
		}

		writeTimer := time.Now()
		log.Debugf("[relay] writing binlog event with header %v", e.Header)
		if n, err2 := r.fd.Write(e.RawData); err2 != nil {
			relayLogWriteErrorCounter.Inc()
			return errors.Trace(err2)
		} else if n != len(e.RawData) {
			relayLogWriteErrorCounter.Inc()
			// FIXME: should we panic here? it seems unreachable
			return errors.Trace(io.ErrShortWrite)
		}

		relayLogWriteDurationHistogram.Observe(time.Since(writeTimer).Seconds())
		relayLogWriteSizeHistogram.Observe(float64(e.Header.EventSize))
		relayLogPosGauge.WithLabelValues(masterNode, masterUUID).Set(float64(lastPos.Pos))
		if index, err := pkgstreamer.GetBinlogFileIndex(lastPos.Name); err != nil {
			log.Errorf("[relay] parse binlog file name %s err %v", lastPos.Name, err)
		} else {
			relayLogFileGauge.WithLabelValues(masterNode, masterUUID).Set(index)
		}

		err = r.meta.Save(lastPos, lastGTID)
		if err != nil {
			return errors.Trace(err)
		}
	}
}

// handleFormatDescriptionEvent tries to create new binlog file and write binlog header
func (r *Relay) handleFormatDescriptionEvent(filename string) (exist bool, err error) {
	if r.fd != nil {
		// close the previous binlog log
		r.fd.Close()
		r.fd = nil
	}

	if len(filename) == 0 {
		binlogReadErrorCounter.Inc()
		return false, errors.NotValidf("write FormatDescriptionEvent with empty binlog filename")
	}

	fullPath := path.Join(r.meta.Dir(), filename)
	fd, err := os.OpenFile(fullPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return false, errors.Annotatef(err, "file full path %s", fullPath)
	}
	r.fd = fd

	err = r.writeBinlogHeaderIfNotExists()
	if err != nil {
		return false, errors.Annotatef(err, "file full path %s", fullPath)
	}

	exist, err = r.checkFormatDescriptionEventExists(filename)
	if err != nil {
		relayLogDataCorruptionCounter.Inc()
		return false, errors.Annotatef(err, "file full path %s", fullPath)
	}

	ret, err := r.fd.Seek(0, io.SeekEnd)
	if err != nil {
		return false, errors.Annotatef(err, "file full path %s", fullPath)
	}
	log.Infof("[relay] %s seek to end (%d)", filename, ret)

	return exist, nil
}

func (r *Relay) reSetupMeta() error {
	uuid, err := r.getServerUUID()
	if err != nil {
		return errors.Trace(err)
	}
	err = r.meta.AddDir(uuid, nil, nil)
	if err != nil {
		return errors.Trace(err)
	}
	err = r.meta.Load()
	if err != nil {
		return errors.Trace(err)
	}

	return nil
}

// getServerUUID gets master server's UUID
// for MySQL，UUID is `server_uuid` system variable
// for MariaDB, UUID is `gtid_domain_id` joined `server_id` with domainServerIDSeparator
func (r *Relay) getServerUUID() (string, error) {
	if r.cfg.Flavor == mysql.MariaDBFlavor {
		domainID, err := utils.GetMariaDBGtidDomainID(r.db)
		if err != nil {
			return "", errors.Trace(err)
		}
		serverID, err := utils.GetServerID(r.db)
		if err != nil {
			return "", errors.Trace(err)
		}
		return fmt.Sprintf("%d%s%d", domainID, domainServerIDSeparator, serverID), nil
	}
	return utils.GetServerUUID(r.db)
}

func (r *Relay) flushMetaAtIntervals(ctx context.Context) {
	ticker := time.NewTicker(flushMetaInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if r.meta.Dirty() {
				err := r.meta.Flush()
				if err != nil {
					log.Errorf("[relay] flush meta error %v", errors.ErrorStack(err))
				} else {
					log.Infof("[relay] flush meta finished, %s", r.meta.String())
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func (r *Relay) writeBinlogHeaderIfNotExists() error {
	b := make([]byte, binlogHeaderSize)
	_, err := r.fd.Read(b)
	log.Debugf("[relay] the first 4 bytes are %v", b)
	if err == io.EOF || !bytes.Equal(b, replication.BinLogFileHeader) {
		_, err = r.fd.Seek(0, io.SeekStart)
		if err != nil {
			return errors.Trace(err)
		}
		log.Info("[relay] write binlog header")
		// write binlog header fe'bin'
		if _, err = r.fd.Write(replication.BinLogFileHeader); err != nil {
			return errors.Trace(err)
		}
		// Note: it's trival to monitor the writing duration and size here. so ignore it.
	} else if err != nil {
		relayLogDataCorruptionCounter.Inc()
		return errors.Trace(err)
	}
	return nil
}

func (r *Relay) checkFormatDescriptionEventExists(filename string) (exists bool, err error) {
	eof, err2 := replication.NewBinlogParser().ParseSingleEvent(r.fd, func(e *replication.BinlogEvent) error {
		return nil
	})
	if err2 != nil {
		return false, errors.Trace(err2)
	}
	// FormatDescriptionEvent is the first event and only one FormatDescriptionEvent in a file.
	if !eof {
		log.Infof("[relay] binlog file %s already has Format_desc event, so ignore it", filename)
		return true, nil
	}
	return false, nil
}

// NOTE: now, no online master-slave switching supported
// when switching, user must Pause relay, update config, then Resume
// so, will call `getBinlogStreamer` again on new master
func (r *Relay) getBinlogStreamer() (*replication.BinlogStreamer, error) {
	defer func() {
		r.lastSlaveConnectionID = r.syncer.LastConnectionID()
		log.Infof("[relay] last slave connection id %d", r.lastSlaveConnectionID)
	}()
	if r.cfg.EnableGTID {
		return r.startSyncByGTID()
	}
	return r.startSyncByPos()
}

func (r *Relay) startSyncByGTID() (*replication.BinlogStreamer, error) {
	uuid, gs := r.meta.GTID()
	log.Infof("[relay] start sync for master(%s, %s) from GTID set %s", r.masterNode(), uuid, gs)

	streamer, err := r.syncer.StartSyncGTID(gs.Origin())
	if err != nil {
		log.Errorf("[relay] start sync in GTID mode from %s error %v", gs.String(), err)
		return r.startSyncByPos()
	}

	return streamer, errors.Trace(err)
}

// TODO: exception handling.
// e.g.
// 1.relay connects to a difference MySQL
// 2. upstream MySQL does a pure restart (removes all its' data, and then restart)

func (r *Relay) startSyncByPos() (*replication.BinlogStreamer, error) {
	// if the first binlog not exists in local, we should fetch from the first position, whatever the specific position is.
	uuid, pos := r.meta.Pos()
	log.Infof("[relay] start sync for master (%s, %s) from %s", r.masterNode(), uuid, pos.String())
	if pos.Name == "" {
		// let mysql decides
		return r.syncer.StartSync(pos)
	}
	if stat, err := os.Stat(filepath.Join(r.meta.Dir(), pos.Name)); os.IsNotExist(err) {
		log.Infof("[relay] should sync from %s:4 instead of %s:%d because the binlog file not exists in local before and should sync from the very beginning", pos.Name, pos.Name, pos.Pos)
		pos.Pos = 4
	} else if err != nil {
		return nil, errors.Trace(err)
	} else {
		if stat.Size() > int64(pos.Pos) {
			// it means binlog file already exists, and the local binlog file already contains the specific position
			//  so we can just fetch from the biggest position, that's the stat.Size()
			//
			// NOTE: is it possible the data from pos.Pos to stat.Size corrupt
			log.Infof("[relay] the binlog file %s already contains position %d, so we should sync from %d", pos.Name, pos.Pos, stat.Size())
			pos.Pos = uint32(stat.Size())
			err := r.meta.Save(pos, nil)
			if err != nil {
				return nil, errors.Trace(err)
			}
		} else if stat.Size() < int64(pos.Pos) {
			// in such case, we should stop immediately and check
			return nil, errors.Annotatef(ErrBinlogPosGreaterThanFileSize, "%s size=%d, specific pos=%d", pos.Name, stat.Size(), pos.Pos)
		}
	}

	streamer, err := r.syncer.StartSync(pos)
	return streamer, errors.Trace(err)
}

// reSyncBinlog re-tries sync binlog when master-slave switched
func (r *Relay) reSyncBinlog(cfg replication.BinlogSyncerConfig) (*replication.BinlogStreamer, error) {
	err := r.retrySyncGTIDs()
	if err != nil {
		return nil, errors.Trace(err)
	}
	return r.reopenStreamer(cfg)
}

// retrySyncGTIDs try to auto fix GTID set
// assume that reset master before switching to new master, and only the new master would write
// it's a weak function to try best to fix GTID set while switching master/slave
func (r *Relay) retrySyncGTIDs() error {
	// TODO: now we don't implement quering GTID from MariaDB, implement it later
	if r.cfg.Flavor != mysql.MySQLFlavor {
		return nil
	}
	_, oldGTIDSet := r.meta.GTID()
	log.Infof("[relay] start retry sync with old GTID %s", oldGTIDSet.String())

	_, newGTIDSet, err := utils.GetMasterStatus(r.db, r.cfg.Flavor)
	if err != nil {
		return errors.Annotatef(err, "get master status")
	}
	log.Infof("[relay] new master GTID set %v", newGTIDSet)

	masterUUID, err := r.getServerUUID()
	if err != nil {
		return errors.Annotatef(err, "get master UUID")
	}
	log.Infof("master UUID %s", masterUUID)

	oldGTIDSet.Replace(newGTIDSet, []interface{}{masterUUID})

	// add sub relay dir for new master server
	// save and flush meta for new master server
	err = r.meta.AddDir(masterUUID, nil, oldGTIDSet)
	if err != nil {
		return errors.Annotatef(err, "add sub relay directory for master server %s", masterUUID)
	}

	return nil
}

// reopenStreamer reopen a new streamer
func (r *Relay) reopenStreamer(cfg replication.BinlogSyncerConfig) (*replication.BinlogStreamer, error) {
	if r.syncer != nil {
		err := r.closeBinlogSyncer(r.syncer)
		r.syncer = nil
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	r.syncer = replication.NewBinlogSyncer(cfg)
	return r.getBinlogStreamer()
}

func (r *Relay) masterNode() string {
	return fmt.Sprintf("%s:%d", r.cfg.From.Host, r.cfg.From.Port)
}

// IsClosed tells whether Relay unit is closed or not.
func (r *Relay) IsClosed() bool {
	return r.closed.Get()
}

// Close implements the dm.Unit interface.
func (r *Relay) Close() {
	r.Lock()
	defer r.Unlock()
	if r.closed.Get() {
		return
	}
	log.Info("[relay] relay unit is closing")
	if r.syncer != nil {
		r.closeBinlogSyncer(r.syncer)
		r.syncer = nil
	}
	if r.fd != nil {
		r.fd.Close()
	}
	if r.db != nil {
		r.db.Close()
	}
	if err := r.meta.Flush(); err != nil {
		log.Errorf("[relay] flush checkpoint error %v", errors.ErrorStack(err))
	}
	r.closed.Set(true)
	log.Info("[relay] relay unit closed")
}

func (r *Relay) closeBinlogSyncer(syncer *replication.BinlogSyncer) error {
	if syncer == nil {
		return nil
	}

	defer syncer.Close()
	lastSlaveConnectionID := syncer.LastConnectionID()
	if lastSlaveConnectionID > 0 {
		err := utils.KillConn(r.db, lastSlaveConnectionID)
		if err != nil {
			if !utils.IsNoSuchThreadError(err) {
				return errors.Annotatef(err, "connection ID %d", lastSlaveConnectionID)
			}
		}
	}
	return nil
}

// Status implements the dm.Unit interface.
func (r *Relay) Status() interface{} {
	masterPos, masterGTID, err := utils.GetMasterStatus(r.db, r.cfg.Flavor)
	if err != nil {
		log.Warnf("[relay] get master status %v", errors.ErrorStack(err))
	}

	uuid, relayPos := r.meta.Pos()
	_, relayGTIDSet := r.meta.GTID()
	rs := &pb.RelayStatus{
		MasterBinlog: masterPos.String(),
		RelaySubDir:  uuid,
		RelayBinlog:  relayPos.String(),
	}
	if masterGTID != nil { // masterGTID maybe a nil interface
		rs.MasterBinlogGtid = masterGTID.String()
	}
	if relayGTIDSet != nil {
		rs.RelayBinlogGtid = relayGTIDSet.String()
	}
	if r.cfg.EnableGTID {
		if rs.MasterBinlogGtid == rs.RelayBinlogGtid {
			rs.RelayCatchUpMaster = true
		}
	} else {
		rs.RelayCatchUpMaster = masterPos.Compare(relayPos) == 0
	}
	return rs
}

// Type implements the dm.Unit interface.
func (r *Relay) Type() pb.UnitType {
	return pb.UnitType_Relay
}

// IsFreshTask implements Unit.IsFreshTask
func (r *Relay) IsFreshTask() (bool, error) {
	return true, nil
}

// Pause pauses the process, it can be resumed later
func (r *Relay) Pause() {
	// Note: will not implemented
}

// Resume resumes the paused process
func (r *Relay) Resume(ctx context.Context, pr chan pb.ProcessResult) {
	// Note: will not implementted
}

// Update implements Unit.Update
func (r *Relay) Update(cfg *config.SubTaskConfig) error {
	// not support update configuration now
	return nil
}
