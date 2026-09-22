package logs

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/logpipeline"
)

const (
	defaultIngestQueueFlushes = 512
	defaultIngestRatePerSec   = 2000.0
	defaultIngestBurst        = 10000
	maxIngestOwedGaps         = 4096
	maxIngestCoalescedLines   = 5000
	reporterControlPlane      = "controlplane"
)

// AsyncIngesterConfig bounds the control-plane log ingest path.
type AsyncIngesterConfig struct {
	// QueueFlushes caps queued agent batches. Past the cap whole
	// batches shed with owed gap rows. Defaults to 512.
	QueueFlushes int
	// RatePerSec and Burst bound the per-allocation ingest guard,
	// which sits above agent-side limits and protects ClickHouse
	// from abusive or buggy agents. Defaults to 2000/s and 10000.
	RatePerSec float64
	Burst      int
}

// IngesterStats reports ingest health and lifetime counters.
type IngesterStats struct {
	QueuedFlushes int
	AcceptedLines uint64
	ShedLines     uint64
	OwedGaps      int
	GapsLost      uint64
	FlushedLines  uint64
	LastFlush     time.Time
	LastError     string
	LastErrorAt   time.Time
}

// flushStore is the durable sink behind the async ingester. LogStore
// is the production implementation; tests inject fakes.
type flushStore interface {
	Enabled() bool
	WriteLogLines(context.Context, []LogLineInput) error
	WriteGaps(context.Context, []GapInput) error
}

// AsyncIngester decouples agent log batches from ClickHouse writes so
// a backend outage cannot stall the agent Sync loop and block
// workload reconciliation. Enqueue never blocks: it rate-limits,
// converts, and queues, shedding whole batches with explicit gap
// accounting past the queue cap. The Run loop flushes with unbounded
// retry, so queued lines wait out an outage instead of dropping.
type AsyncIngester struct {
	store   flushStore
	queue   chan pendingFlush
	limiter *logpipeline.Limiter
	backoff *logpipeline.Backoff

	acceptedLines atomic.Uint64
	shedLines     atomic.Uint64
	flushedLines  atomic.Uint64
	gapsLost      atomic.Uint64

	mu          sync.Mutex
	owed        map[owedGapKey]*owedGap
	lastFlush   time.Time
	lastError   string
	lastErrorAt time.Time
}

type pendingFlush struct {
	lines []LogLineInput
	gaps  []GapInput
}

type owedGapKey struct {
	serviceID    string
	allocationID string
	buildID      string
	logType      string
	stream       string
	reason       string
	reporter     string
}

type owedGap struct {
	key          owedGapKey
	droppedCount uint64
	windowStart  time.Time
	windowEnd    time.Time
}

// NewAsyncIngester builds the ingest queue. A nil or disabled store
// makes Enqueue a no-op and Run return immediately.
func NewAsyncIngester(store flushStore, cfg AsyncIngesterConfig) *AsyncIngester {
	queueFlushes := cfg.QueueFlushes
	if queueFlushes <= 0 {
		queueFlushes = defaultIngestQueueFlushes
	}
	rate := cfg.RatePerSec
	if rate <= 0 {
		rate = defaultIngestRatePerSec
	}
	burst := cfg.Burst
	if burst <= 0 {
		burst = defaultIngestBurst
	}
	return &AsyncIngester{
		store:   store,
		queue:   make(chan pendingFlush, queueFlushes),
		limiter: logpipeline.NewLimiter(rate, burst),
		backoff: &logpipeline.Backoff{},
		owed:    make(map[owedGapKey]*owedGap),
	}
}

// EnqueueAgentBatch converts one validated batch and queues it for
// durable write. It never blocks and never fails: overload sheds
// with gap accounting.
func (a *AsyncIngester) EnqueueAgentBatch(agentID string, batch *agentv1.LogBatch) {
	if a == nil || a.store == nil || !a.store.Enabled() || batch == nil {
		return
	}
	inputs, gaps := convertAgentBatch(agentID, batch)
	now := time.Now().UTC()
	kept := inputs[:0]
	limited := make(map[owedGapKey]uint64)
	for _, in := range inputs {
		key := in.AllocationID
		if key == "" {
			key = in.BuildID
		}
		if key == "" || a.limiter.Allow("alloc:"+key) {
			kept = append(kept, in)
			continue
		}
		limited[owedGapKey{
			serviceID:    in.ServiceID,
			allocationID: in.AllocationID,
			buildID:      in.BuildID,
			logType:      string(normalizeLogType(in.LogType)),
			stream:       normalizeLogStream(in.Stream),
			reason:       logpipeline.ReasonRateLimited,
			reporter:     reporterControlPlane,
		}]++
	}
	for key, count := range limited {
		gaps = append(gaps, GapInput{
			ServiceID:    key.serviceID,
			AllocationID: key.allocationID,
			BuildID:      key.buildID,
			LogType:      LogType(key.logType),
			Stream:       key.stream,
			WindowStart:  now,
			WindowEnd:    now,
			DroppedCount: count,
			Reason:       key.reason,
			Reporter:     key.reporter,
		})
		a.shedLines.Add(count)
	}
	a.acceptedLines.Add(uint64(len(kept)))
	flush := pendingFlush{lines: kept, gaps: gaps}
	select {
	case a.queue <- flush:
	default:
		a.shedFlush(flush)
	}
}

// Run flushes queued batches until ctx ends. Flushes retry with
// backoff across a ClickHouse outage; only shutdown drops the
// backlog, and agents replay their recent window on reconnect. Run
// always blocks until ctx ends, even when disabled, so hosting
// servers never observe an early clean return as a shutdown signal.
func (a *AsyncIngester) Run(ctx context.Context) error {
	if a == nil || a.store == nil || !a.store.Enabled() {
		<-ctx.Done()
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case flush := <-a.queue:
			a.coalesce(&flush)
			a.attachOwed(&flush)
			if len(flush.lines) == 0 && len(flush.gaps) == 0 {
				continue
			}
			if err := a.flushWithRetry(ctx, flush); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

// Stats reports a point-in-time snapshot.
func (a *AsyncIngester) Stats() IngesterStats {
	if a == nil {
		return IngesterStats{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return IngesterStats{
		QueuedFlushes: len(a.queue),
		AcceptedLines: a.acceptedLines.Load(),
		ShedLines:     a.shedLines.Load(),
		OwedGaps:      len(a.owed),
		GapsLost:      a.gapsLost.Load(),
		FlushedLines:  a.flushedLines.Load(),
		LastFlush:     a.lastFlush,
		LastError:     a.lastError,
		LastErrorAt:   a.lastErrorAt,
	}
}

// shedFlush converts a queue-overflowed flush into owed gap rows so
// the loss still surfaces in reads.
func (a *AsyncIngester) shedFlush(flush pendingFlush) {
	now := time.Now().UTC()
	a.mu.Lock()
	defer a.mu.Unlock()
	counts := make(map[owedGapKey]uint64)
	for _, in := range flush.lines {
		counts[owedGapKey{
			serviceID:    in.ServiceID,
			allocationID: in.AllocationID,
			buildID:      in.BuildID,
			logType:      string(normalizeLogType(in.LogType)),
			stream:       normalizeLogStream(in.Stream),
			reason:       logpipeline.ReasonIngestOverflow,
			reporter:     reporterControlPlane,
		}]++
	}
	for key, count := range counts {
		a.shedLines.Add(count)
		owed, ok := a.owed[key]
		if !ok {
			if len(a.owed) >= maxIngestOwedGaps {
				a.gapsLost.Add(count)
				continue
			}
			owed = &owedGap{key: key, windowStart: now, windowEnd: now}
			a.owed[key] = owed
		}
		owed.droppedCount += count
		owed.windowEnd = now
	}
	// Producer gap reports inside a shed flush are owed too; without
	// them the producer's own drops would vanish silently.
	for _, gap := range flush.gaps {
		if gap.ServiceID == "" || gap.DroppedCount == 0 {
			continue
		}
		key := owedGapKey{
			serviceID:    gap.ServiceID,
			allocationID: gap.AllocationID,
			buildID:      gap.BuildID,
			logType:      string(normalizeLogType(gap.LogType)),
			stream:       normalizeLogStream(gap.Stream),
			reason:       logpipeline.NormalizeDropReason(gap.Reason),
			reporter:     normalizeReporter(gap.Reporter),
		}
		owed, ok := a.owed[key]
		if !ok {
			if len(a.owed) >= maxIngestOwedGaps {
				a.gapsLost.Add(gap.DroppedCount)
				continue
			}
			owed = &owedGap{key: key, windowStart: gap.WindowStart, windowEnd: gap.WindowEnd}
			a.owed[key] = owed
		}
		owed.droppedCount += gap.DroppedCount
		if !gap.WindowStart.IsZero() && gap.WindowStart.Before(owed.windowStart) {
			owed.windowStart = gap.WindowStart
		}
		if gap.WindowEnd.After(owed.windowEnd) {
			owed.windowEnd = gap.WindowEnd
		}
	}
}

// coalesce drains additional queued flushes into one ClickHouse
// write, bounded so a single insert stays small.
func (a *AsyncIngester) coalesce(flush *pendingFlush) {
	for len(flush.lines) < maxIngestCoalescedLines {
		select {
		case next := <-a.queue:
			flush.lines = append(flush.lines, next.lines...)
			flush.gaps = append(flush.gaps, next.gaps...)
		default:
			return
		}
	}
}

// attachOwed pops owed gap rows into the outgoing flush.
func (a *AsyncIngester) attachOwed(flush *pendingFlush) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.owed) == 0 {
		return
	}
	for key, owed := range a.owed {
		flush.gaps = append(flush.gaps, GapInput{
			ServiceID:    key.serviceID,
			AllocationID: key.allocationID,
			BuildID:      key.buildID,
			LogType:      LogType(key.logType),
			Stream:       key.stream,
			WindowStart:  owed.windowStart,
			WindowEnd:    owed.windowEnd,
			DroppedCount: owed.droppedCount,
			Reason:       key.reason,
			Reporter:     key.reporter,
		})
		delete(a.owed, key)
	}
}

func (a *AsyncIngester) flushWithRetry(ctx context.Context, flush pendingFlush) error {
	for {
		linesErr := a.store.WriteLogLines(ctx, flush.lines)
		var gapsErr error
		if linesErr == nil {
			gapsErr = a.store.WriteGaps(ctx, flush.gaps)
		}
		if linesErr == nil && gapsErr == nil {
			a.backoff.Reset()
			a.flushedLines.Add(uint64(len(flush.lines)))
			a.mu.Lock()
			a.lastFlush = time.Now().UTC()
			a.lastError = ""
			a.mu.Unlock()
			return nil
		}
		err := linesErr
		if err == nil {
			err = gapsErr
		}
		a.mu.Lock()
		a.lastError = err.Error()
		a.lastErrorAt = time.Now().UTC()
		a.mu.Unlock()
		slog.WarnContext(ctx, "log ingest flush failed, retrying",
			"error", err, "lines", len(flush.lines), "gaps", len(flush.gaps))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.backoff.Next()):
		}
	}
}
