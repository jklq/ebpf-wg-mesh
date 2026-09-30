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
	defaultShipBatchSize     = 256
	defaultShipFlushInterval = time.Second
	defaultShipReplayWindow  = 5 * time.Minute
)

// shipAdmitRate caps one allocation at the shipper's drain rate. Non-positive rates disable limiting.
func shipAdmitRate(ratePerSec float64, batch int, interval time.Duration) float64 {
	if ratePerSec <= 0 {
		return 0
	}
	drain := float64(batch) / interval.Seconds()
	if ratePerSec > drain {
		slog.Warn("lowering producer log rate to the shipper drain rate",
			"requested_per_sec", ratePerSec, "drain_per_sec", drain)
		return drain
	}
	return ratePerSec
}

// aggregateAdmitRate caps every allocation together at one batch per flush, so
// separate buckets cannot overfill the spool.
func aggregateAdmitRate(ratePerSec float64, batch int, interval time.Duration) float64 {
	if ratePerSec <= 0 || batch <= 0 || interval <= 0 {
		return 0
	}
	return float64(batch) / interval.Seconds()
}

// logShipConfig bounds one agent log shipper. Zero values select defaults, except a
// non-positive RatePerSec disables producer limiting.
type logShipConfig struct {
	SpoolDir      string
	SpoolMaxBytes int64
	RatePerSec    float64
	Burst         int
	BatchSize     int
	FlushInterval time.Duration
	ReplayWindow  time.Duration
}

type logShipStats struct {
	Accepted     uint64
	Limited      uint64
	SpoolDropped uint64
	Unshipped    int64
}

// logShipper is the agent's durable log pipeline: rate-limited container output
// funnels into a bounded disk spool, and a ship loop forwards batches over the
// Sync stream with retry. It outlives any session: while detached the spool
// absorbs output, and on attach the recent window replays. Shed lines are
// counted per allocation and reported as explicit read gaps.
type logShipper struct {
	agentID          string
	spool            *logpipeline.Spool
	limiter          *logpipeline.Limiter
	aggregateLimiter *logpipeline.Limiter

	batchSize     int
	flushInterval time.Duration
	replayWindow  time.Duration

	// shipMu serializes flush cycles with attach rewinds. It never guards
	// AppendLog, so logging never blocks on the network.
	shipMu sync.Mutex

	mu   sync.Mutex
	send func(*agentv1.AgentClientMessage) error

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
		Dir:      cfg.SpoolDir,
		MaxBytes: cfg.SpoolMaxBytes,
		// Acknowledgement is durable journal admission, so committed records stay for the
		// replay window: a reconnect re-sends what was acknowledged but not ingested.
		Retention: cfg.ReplayWindow,
	})
	if err != nil {
		return nil, err
	}
	return &logShipper{
		agentID:          agentID,
		spool:            spool,
		limiter:          logpipeline.NewLimiter(shipAdmitRate(cfg.RatePerSec, cfg.BatchSize, cfg.FlushInterval), cfg.Burst),
		aggregateLimiter: logpipeline.NewLimiter(aggregateAdmitRate(cfg.RatePerSec, cfg.BatchSize, cfg.FlushInterval), cfg.BatchSize),
		batchSize:        cfg.BatchSize,
		flushInterval:    cfg.FlushInterval,
		replayWindow:     cfg.ReplayWindow,
	}, nil
}

// AppendLog rate-limits and spools one container line. It never blocks on the network.
func (s *logShipper) AppendLog(entry *agentv1.LogEntry) {
	if s == nil || entry == nil {
		return
	}
	key := entry.GetAllocationId()
	if !s.limiter.Allow(key) {
		s.noteDrop(entry, logpipeline.ReasonRateLimited)
		s.limited.Add(1)
		return
	}
	if !s.aggregateLimiter.Allow("agent") {
		s.noteDrop(entry, logpipeline.ReasonRateLimited)
		s.limited.Add(1)
		return
	}
	payload, err := proto.Marshal(entry)
	if err != nil {
		slog.Warn("marshal log entry", "agent_id", s.agentID, "error", err)
		s.noteDrop(entry, logpipeline.ReasonSpoolOverflow)
		return
	}
	observedAt := entry.GetObservedAt().AsTime()
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	if err := s.spool.Append(logpipeline.Record{DropKey: agentLogDropKey(entry), ObservedAt: observedAt, Payload: payload}); err != nil {
		slog.Warn("spool log entry", "agent_id", s.agentID, "error", err)
		s.noteDrop(entry, logpipeline.ReasonSpoolOverflow)
		return
	}
	s.accepted.Add(1)
}

func agentLogDropKey(entry *agentv1.LogEntry) logpipeline.DropKey {
	return logpipeline.DropKey{ServiceID: entry.GetServiceId(), AllocationID: entry.GetAllocationId(), LogType: entry.GetLogType(), Stream: entry.GetStream()}
}

func (s *logShipper) noteDrop(entry *agentv1.LogEntry, reason string) {
	key := agentLogDropKey(entry)
	key.Reason = reason
	now := entry.GetObservedAt().AsTime()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if err := s.spool.AddDrops(logpipeline.Drop{Key: key, Count: 1, Start: now, End: now}); err != nil {
		slog.Error("persist producer log loss", "agent_id", s.agentID, "allocation_id", key.AllocationID, "error", err)
	}
}

// Attach connects the ship loop to a session's send function and replays the
// recent window plus everything unshipped. The send function returns once the
// server accepted the batch; on error the batch stays unshipped and retries.
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

// Close waits for an in-flight flush and closes the durable queue.
func (s *logShipper) Close() error {
	if s == nil {
		return nil
	}
	s.shipMu.Lock()
	defer s.shipMu.Unlock()
	return s.spool.Close()
}

func (s *logShipper) Stats() logShipStats {
	if s == nil {
		return logShipStats{}
	}
	spoolStats, err := s.spool.Stats()
	if err != nil {
		slog.Error("read log spool health", "agent_id", s.agentID, "error", err)
	}
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
	s.limiter.DrainDrops()
	s.aggregateLimiter.DrainDrops()
	send := s.currentSend()
	if send == nil {
		return
	}
	records, cursor, err := s.spool.Read(s.batchSize)
	if err != nil {
		slog.Warn("read log spool", "agent_id", s.agentID, "error", err)
		return
	}
	taken, err := s.spool.PendingDrops()
	if err != nil {
		s.spool.Release()
		slog.Error("read producer log losses", "agent_id", s.agentID, "error", err)
		return
	}
	var dropped uint64
	for _, summary := range taken {
		dropped += summary.GetDroppedCount()
	}
	if len(records) == 0 && len(taken) == 0 {
		s.spool.Release()
		return
	}
	entries := make([]*agentv1.LogEntry, 0, len(records))
	for _, record := range records {
		var entry agentv1.LogEntry
		if err := proto.Unmarshal(record.Payload, &entry); err != nil {
			slog.Warn("decode spooled log entry", "agent_id", s.agentID, "error", err)
			if err := s.spool.Discard(record); err != nil {
				s.spool.Release()
				slog.Error("record corrupt producer log loss", "error", err)
				return
			}
			continue
		}
		entries = append(entries, &entry)
	}
	// Every message stays within the wire budget. Drop summaries ride their own
	// bounded messages and must never block line delivery.
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
			s.spool.Release()
			return
		}
	}
	if err := s.spool.Commit(cursor, taken); err != nil {
		slog.Warn("commit log spool", "agent_id", s.agentID, "error", err)
		return
	}
}
