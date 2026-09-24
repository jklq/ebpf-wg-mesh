package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/logpipeline"

	"google.golang.org/protobuf/proto"
)

const (
	defaultShipBatchSize     = 100
	defaultShipFlushInterval = time.Second
	defaultShipReplayWindow  = 5 * time.Minute
	// pendingDropsFile durably holds unreported drop summaries next
	// to the spool so drop accounting survives a restart.
	pendingDropsFile = "pending-drops.json"
)

// logShipConfig bounds one agent log shipper. Zero values select
// defaults, except RatePerSec: a non-positive rate disables
// producer limiting.
type logShipConfig struct {
	SpoolDir      string
	SpoolMaxBytes int64
	RatePerSec    float64
	Burst         int
	BatchSize     int
	FlushInterval time.Duration
	ReplayWindow  time.Duration
}

type allocMeta struct {
	serviceID string
	envID     string
	logType   platformv1.ServiceLogType
}

type logShipStats struct {
	Accepted     uint64
	Limited      uint64
	SpoolDropped uint64
	Unshipped    int64
}

// logShipper is the agent's durable log pipeline. Container output
// funnels through per-allocation rate limiting into a bounded
// disk-backed spool, and a ship loop forwards batches over the
// current Sync stream with retry. It outlives any single session:
// while detached the spool absorbs output, and on attach the recent
// window replays alongside everything unshipped so a reconnect
// loses nothing the spool still holds. Retried lines reuse their
// stable IDs and collapse server-side. Every shed line is counted
// per allocation and reported as an explicit read gap; pending
// drop summaries persist next to the spool so shutdown and restart
// keep the accounting, and coalesce by identity so a sustained
// outage holds one entry per identity instead of one per flush
// interval.
type logShipper struct {
	agentID  string
	spoolDir string
	spool    *logpipeline.Spool
	limiter  *logpipeline.Limiter

	batchSize     int
	flushInterval time.Duration
	replayWindow  time.Duration

	// shipMu serializes flush cycles with attach rewinds: a batch
	// read before the rewind must not commit past it and swallow the
	// replay window. It never guards AppendLog, so logging never
	// blocks on the network.
	shipMu sync.Mutex

	mu       sync.Mutex
	send     func(*agentv1.AgentClientMessage) error
	meta     map[string]allocMeta
	overflow map[string]uint64
	pending  *logpipeline.DropSet

	accepted atomic.Uint64
	limited  atomic.Uint64
}

func newLogShipper(agentID string, cfg logShipConfig) (*logShipper, error) {
	if agentID == "" {
		return nil, errors.New("agent id is required")
	}
	if cfg.SpoolDir == "" {
		return nil, errors.New("log spool directory is required")
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultShipBatchSize
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = defaultShipFlushInterval
	}
	if cfg.ReplayWindow <= 0 {
		cfg.ReplayWindow = defaultShipReplayWindow
	}
	spool, err := logpipeline.OpenSpool(logpipeline.SpoolConfig{
		Dir:        cfg.SpoolDir,
		MaxBytes:   cfg.SpoolMaxBytes,
		SyncWrites: true,
		// Acknowledgement is queue admission at the control plane, so
		// committed sealed segments stay for the replay window: a
		// reconnect after a control-plane crash re-sends what the
		// backend acknowledged but did not durably ingest.
		Retention: cfg.ReplayWindow,
	})
	if err != nil {
		return nil, err
	}
	pending, err := loadPendingDrops(cfg.SpoolDir)
	if err != nil {
		slog.Warn("load pending log drop summaries", "agent_id", agentID, "error", err)
		pending = logpipeline.NewDropSet()
	}
	return &logShipper{
		agentID:       agentID,
		spoolDir:      cfg.SpoolDir,
		spool:         spool,
		limiter:       logpipeline.NewLimiter(cfg.RatePerSec, cfg.Burst),
		batchSize:     cfg.BatchSize,
		flushInterval: cfg.FlushInterval,
		replayWindow:  cfg.ReplayWindow,
		meta:          make(map[string]allocMeta),
		overflow:      make(map[string]uint64),
		pending:       pending,
	}, nil
}

// AppendLog rate-limits and spools one container line. It never
// blocks on the network: the ship loop forwards asynchronously.
func (s *logShipper) AppendLog(entry *agentv1.LogEntry) {
	if s == nil || entry == nil {
		return
	}
	key := entry.GetAllocationId()
	if !s.limiter.Allow(key) {
		s.recordMeta(key, entry)
		s.limited.Add(1)
		return
	}
	s.recordMeta(key, entry)
	payload, err := proto.Marshal(entry)
	if err != nil {
		slog.Warn("marshal log entry", "agent_id", s.agentID, "error", err)
		s.countOverflow(key, 1)
		return
	}
	observedAt := entry.GetObservedAt().AsTime()
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	if err := s.spool.Append(key, entry.GetLineId(), observedAt, payload); err != nil {
		slog.Warn("spool log entry", "agent_id", s.agentID, "error", err)
		s.countOverflow(key, 1)
		return
	}
	s.accepted.Add(1)
}

func (s *logShipper) recordMeta(key string, entry *agentv1.LogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.meta[key]; ok {
		return
	}
	s.meta[key] = allocMeta{
		serviceID: entry.GetServiceId(),
		envID:     entry.GetEnvironmentId(),
		logType:   entry.GetLogType(),
	}
}

func (s *logShipper) countOverflow(key string, count uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overflow[key] += count
}

// Attach connects the ship loop to a session's send function and
// replays the recent window plus everything unshipped for
// at-least-once delivery across reconnects. The rewind is serialized
// against in-flight flushes and takes effect before the next batch
// is read, so no batch can commit past it.
func (s *logShipper) Attach(send func(*agentv1.AgentClientMessage) error) {
	if s == nil {
		return
	}
	s.shipMu.Lock()
	defer s.shipMu.Unlock()
	s.mu.Lock()
	s.send = send
	s.mu.Unlock()
	if err := s.spool.RewindForReplay(time.Now().Add(-s.replayWindow)); err != nil {
		slog.Warn("rewind log spool", "agent_id", s.agentID, "error", err)
	}
}

// Detach parks the ship loop; output keeps spooling to disk.
func (s *logShipper) Detach() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.send = nil
}

// Run ships spooled batches until ctx ends.
func (s *logShipper) Run(ctx context.Context) error {
	if s == nil {
		return nil
	}
	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.flush()
		}
	}
}

// Close drains drop counters into durable pending summaries and
// closes the spool. Unshipped records and pending drop summaries
// survive the restart: records replay from the durable cursor and
// summaries ship with the first batch.
func (s *logShipper) Close() error {
	if s == nil {
		return nil
	}
	s.collectDrops(time.Now().UTC())
	s.mu.Lock()
	s.persistPendingLocked()
	s.mu.Unlock()
	return s.spool.Close()
}

// Stats reports shipper counters and unshipped depth.
func (s *logShipper) Stats() logShipStats {
	if s == nil {
		return logShipStats{}
	}
	spoolStats := s.spool.Stats()
	return logShipStats{
		Accepted:     s.accepted.Load(),
		Limited:      s.limited.Load(),
		SpoolDropped: spoolStats.DroppedRecords,
		Unshipped:    spoolStats.PendingRecords,
	}
}

func (s *logShipper) currentSend() func(*agentv1.AgentClientMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.send
}

func (s *logShipper) flush() {
	s.shipMu.Lock()
	defer s.shipMu.Unlock()
	now := time.Now().UTC()
	// Drop accounting is collected and persisted even while detached:
	// a crash before the next attach must not lose counted losses that
	// never reached a summary.
	s.collectDrops(now)
	send := s.currentSend()
	if send == nil {
		return
	}
	records, cursor, err := s.spool.Read(s.batchSize)
	if err != nil {
		slog.Warn("read log spool", "agent_id", s.agentID, "error", err)
		return
	}
	s.mu.Lock()
	taken := s.pending.Take()
	s.mu.Unlock()
	var dropped uint64
	for _, summary := range taken {
		dropped += summary.GetDroppedCount()
	}
	if len(records) == 0 && len(taken) == 0 {
		return
	}
	entries := make([]*agentv1.LogEntry, 0, len(records))
	for _, record := range records {
		var entry agentv1.LogEntry
		if err := proto.Unmarshal(record.Payload, &entry); err != nil {
			slog.Warn("decode spooled log entry", "agent_id", s.agentID, "error", err)
			s.countOverflow(record.Key, 1)
			continue
		}
		entries = append(entries, &entry)
	}
	// Every message stays within the wire budget: a batch of
	// maximum-size lines must never exceed the transport's receive
	// limit, which would wedge delivery permanently. Drop summaries
	// ride their own bounded messages — an accumulated set can
	// exceed the budget too and must never block line delivery.
	dropChunks := logpipeline.ChunkByBytes(taken, func(d *platformv1.LogDropSummary) int { return proto.Size(d) }, logpipeline.MaxBatchBytes)
	entryChunks := logpipeline.ChunkByBytes(entries, func(e *agentv1.LogEntry) int { return proto.Size(e) }, logpipeline.MaxBatchBytes)
	for i, chunk := range dropChunks {
		batch := &agentv1.LogBatch{AgentId: s.agentID, Drops: chunk}
		if i == 0 {
			batch.DroppedLines = dropped
		}
		err = send(&agentv1.AgentClientMessage{
			Payload: &agentv1.AgentClientMessage_LogBatch{LogBatch: batch},
		})
		if err != nil {
			slog.Warn("send container log batch failed", "agent_id", s.agentID, "error", err)
			s.restorePending(taken)
			s.spool.Release()
			return
		}
	}
	for _, chunk := range entryChunks {
		err = send(&agentv1.AgentClientMessage{
			Payload: &agentv1.AgentClientMessage_LogBatch{LogBatch: &agentv1.LogBatch{AgentId: s.agentID, Entries: chunk}},
		})
		if err != nil {
			slog.Warn("send container log batch failed", "agent_id", s.agentID, "error", err)
			s.restorePending(taken)
			s.spool.Release()
			return
		}
	}
	if err := s.spool.Commit(cursor); err != nil {
		slog.Warn("commit log spool", "agent_id", s.agentID, "error", err)
		s.restorePending(taken)
		return
	}
	s.mu.Lock()
	s.persistPendingLocked()
	s.mu.Unlock()
}

// restorePending merges unsent summaries back into the pending set
// so a failed send keeps its accounting; coalescing folds them into
// newer windows for the same identity.
func (s *logShipper) restorePending(taken []*platformv1.LogDropSummary) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending.Restore(taken)
	s.persistPendingLocked()
}

// loadPendingDrops reads drop summaries persisted by an earlier
// process so shutdown-time accounting reports after the restart.
func loadPendingDrops(dir string) (*logpipeline.DropSet, error) {
	rows, err := logpipeline.LoadDrops(dir)
	if err != nil {
		return nil, err
	}
	pending := logpipeline.NewDropSet()
	pending.Restore(rows)
	return pending, nil
}

// persistPendingLocked snapshots the pending drop summaries next to
// the spool. The snapshot is advisory: retried summaries collapse
// server-side by gap identity, so a stale copy only re-reports.
func (s *logShipper) persistPendingLocked() {
	if err := logpipeline.SaveDrops(s.spoolDir, s.pending.Summaries()); err != nil {
		slog.Warn("persist pending log drop summaries", "agent_id", s.agentID, "error", err)
	}
}

// collectDrops drains limiter, spool, and overflow counters into the
// pending gap summaries reported with the next batch, coalescing by
// identity so repeated collections during an outage cannot grow the
// pending set, then durably snapshots them so shutdown or crash
// keeps the accounting.
func (s *logShipper) collectDrops(now time.Time) {
	windowStart := now.Add(-s.flushInterval)
	limited := s.limiter.DrainDrops()
	evicted, corrupt := s.spool.DrainDrops()
	s.mu.Lock()
	overflow := s.overflow
	s.overflow = make(map[string]uint64)
	s.mu.Unlock()
	if len(limited) == 0 && len(evicted) == 0 && len(corrupt) == 0 && len(overflow) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, count := range limited {
		s.notePendingLocked(key, count, logpipeline.ReasonRateLimited, windowStart, now)
	}
	for key, count := range evicted {
		s.notePendingLocked(key, count, logpipeline.ReasonSpoolOverflow, windowStart, now)
	}
	for key, count := range corrupt {
		s.notePendingLocked(key, count, logpipeline.ReasonCorruptSpool, windowStart, now)
	}
	for key, count := range overflow {
		s.notePendingLocked(key, count, logpipeline.ReasonSpoolOverflow, windowStart, now)
	}
	s.persistPendingLocked()
}

// notePendingLocked attributes one drop count to its allocation and
// coalesces it into the pending entry for that identity. The service
// ID is advisory: the control plane derives authoritative service
// attribution from the allocation owner, so counts surface even when
// local metadata is gone after a restart. Counts without a
// recoverable key cannot appear in reads and stay in the shipper
// counters only.
func (s *logShipper) notePendingLocked(key string, count uint64, reason string, windowStart, windowEnd time.Time) {
	if key == "" {
		return
	}
	meta := s.meta[key]
	s.pending.Add(logpipeline.DropKey{
		ServiceID:    meta.serviceID,
		AllocationID: key,
		LogType:      meta.logType,
		Reason:       reason,
	}, count, windowStart, windowEnd)
}
