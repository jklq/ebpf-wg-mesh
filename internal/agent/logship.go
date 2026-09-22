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
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	defaultShipBatchSize     = 100
	defaultShipFlushInterval = time.Second
	defaultShipReplayWindow  = 5 * time.Minute
)

// logShipConfig bounds one agent log shipper. Zero values select
// defaults.
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
// window replays so a reconnect loses nothing the spool still holds.
// Retried lines reuse their stable IDs and collapse server-side.
// Every shed line is counted per allocation and reported as an
// explicit read gap.
type logShipper struct {
	agentID string
	spool   *logpipeline.Spool
	limiter *logpipeline.Limiter

	batchSize     int
	flushInterval time.Duration
	replayWindow  time.Duration

	mu       sync.Mutex
	send     func(*agentv1.AgentClientMessage) error
	meta     map[string]allocMeta
	overflow map[string]uint64
	pending  []*platformv1.LogDropSummary

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
	})
	if err != nil {
		return nil, err
	}
	return &logShipper{
		agentID:       agentID,
		spool:         spool,
		limiter:       logpipeline.NewLimiter(cfg.RatePerSec, cfg.Burst),
		batchSize:     cfg.BatchSize,
		flushInterval: cfg.FlushInterval,
		replayWindow:  cfg.ReplayWindow,
		meta:          make(map[string]allocMeta),
		overflow:      make(map[string]uint64),
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
// replays the recent window for at-least-once delivery across
// reconnects.
func (s *logShipper) Attach(send func(*agentv1.AgentClientMessage) error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.send = send
	s.mu.Unlock()
	if err := s.spool.RewindToTime(time.Now().Add(-s.replayWindow)); err != nil {
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

// Close closes the underlying spool.
func (s *logShipper) Close() error {
	if s == nil {
		return nil
	}
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
	send := s.currentSend()
	if send == nil {
		return
	}
	now := time.Now().UTC()
	s.collectDrops(now)
	records, cursor, err := s.spool.Read(s.batchSize)
	if err != nil {
		slog.Warn("read log spool", "agent_id", s.agentID, "error", err)
		return
	}
	s.mu.Lock()
	pending := s.pending
	s.mu.Unlock()
	if len(records) == 0 && len(pending) == 0 {
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
	var dropped uint64
	for _, summary := range pending {
		dropped += summary.GetDroppedCount()
	}
	err = send(&agentv1.AgentClientMessage{
		Payload: &agentv1.AgentClientMessage_LogBatch{
			LogBatch: &agentv1.LogBatch{
				AgentId:      s.agentID,
				Entries:      entries,
				DroppedLines: dropped,
				Drops:        pending,
			},
		},
	})
	if err != nil {
		slog.Warn("send container log batch failed", "agent_id", s.agentID, "error", err)
		return
	}
	if err := s.spool.Commit(cursor); err != nil {
		slog.Warn("commit log spool", "agent_id", s.agentID, "error", err)
		return
	}
	s.mu.Lock()
	if len(pending) > 0 && len(s.pending) >= len(pending) {
		s.pending = append([]*platformv1.LogDropSummary(nil), s.pending[len(pending):]...)
	}
	s.mu.Unlock()
}

// collectDrops drains limiter, spool, and overflow counters into the
// pending gap summaries reported with the next batch.
func (s *logShipper) collectDrops(now time.Time) {
	windowStart := now.Add(-s.flushInterval)
	limited := s.limiter.DrainDrops()
	spooled := s.spool.DrainDrops()
	s.mu.Lock()
	overflow := s.overflow
	s.overflow = make(map[string]uint64)
	s.mu.Unlock()
	if len(limited) == 0 && len(spooled) == 0 && len(overflow) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, count := range limited {
		if summary := s.summaryLocked(key, count, logpipeline.ReasonRateLimited, windowStart, now); summary != nil {
			s.pending = append(s.pending, summary)
		}
	}
	for key, count := range spooled {
		if summary := s.summaryLocked(key, count, logpipeline.ReasonSpoolOverflow, windowStart, now); summary != nil {
			s.pending = append(s.pending, summary)
		}
	}
	for key, count := range overflow {
		if summary := s.summaryLocked(key, count, logpipeline.ReasonSpoolOverflow, windowStart, now); summary != nil {
			s.pending = append(s.pending, summary)
		}
	}
}

// summaryLocked attributes one drop count to its service. Counts
// without a known service cannot surface in reads and stay in the
// shipper counters only.
func (s *logShipper) summaryLocked(key string, count uint64, reason string, windowStart, windowEnd time.Time) *platformv1.LogDropSummary {
	meta, ok := s.meta[key]
	if !ok || meta.serviceID == "" {
		return nil
	}
	return &platformv1.LogDropSummary{
		ServiceId:    meta.serviceID,
		AllocationId: key,
		LogType:      meta.logType,
		DroppedCount: count,
		Reason:       reason,
		WindowStart:  timestamppb.New(windowStart),
		WindowEnd:    timestamppb.New(windowEnd),
	}
}
