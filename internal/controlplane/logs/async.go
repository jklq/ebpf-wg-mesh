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
	defaultIngestQueueFlushes  = 512
	defaultIngestQueueBytes    = 64 << 20
	ingestRowOverheadBytes     = 256
	defaultIngestRatePerSec    = 2000.0
	defaultIngestBurst         = 10000
	maxIngestOwedGaps          = 4096
	maxIngestCoalescedLines    = 5000
	defaultIngestShutdownGrace = 15 * time.Second
	reporterControlPlane       = "controlplane"
)

// AsyncIngesterConfig bounds the control-plane log ingest path.
type AsyncIngesterConfig struct {
	// QueueFlushes caps queued agent batches. Past the cap whole
	// batches shed with owed gap rows. Defaults to 512.
	QueueFlushes int
	// QueueBytes caps the retained size of queued batches, so many
	// maximum-size batches can never hold a memory-limited control
	// plane hostage during a backend outage. Past the budget whole
	// batches shed with owed gap rows. Defaults to 64 MiB.
	QueueBytes int64
	// RatePerSec and Burst bound the per-allocation ingest guard,
	// which sits above agent-side limits and protects ClickHouse
	// from abusive or buggy agents. Defaults to 2000/s and 10000.
	RatePerSec float64
	Burst      int
	// ShutdownGrace bounds how long shutdown drains the accepted
	// backlog into the store before giving up. Defaults to 15s.
	ShutdownGrace time.Duration
}

// IngesterStats reports ingest health and lifetime counters.
type IngesterStats struct {
	QueuedFlushes int
	QueuedBytes   int64
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
	store          flushStore
	queue          chan pendingFlush
	queueBytes     atomic.Int64
	queueByteLimit int64
	limiter        *logpipeline.Limiter
	backoff        *logpipeline.Backoff

	acceptedLines atomic.Uint64
	shedLines     atomic.Uint64
	flushedLines  atomic.Uint64
	gapsLost      atomic.Uint64

	mu          sync.Mutex
	owed        map[owedGapKey]*owedGap
	owedFold    map[owedGapKey]*owedGap
	drainDone   bool
	lastFlush   time.Time
	lastError   string
	lastErrorAt time.Time

	shutdownGrace time.Duration
}

type pendingFlush struct {
	lines []LogLineInput
	gaps  []GapInput
	// bytes is the admitted retained size, subtracted from the
	// queue budget when the flush is pulled.
	bytes int64
}

type owedGapKey struct {
	serviceID    string
	allocationID string
	buildID      string
	logType      string
	stream       string
	reason       string
	reporter     string
	// summaryID is the producer's stable summary identity. Shed
	// producer gaps keep it so a replay replaces the same gap row
	// instead of double-counting the loss.
	summaryID string
}

type owedGap struct {
	key          owedGapKey
	droppedCount uint64
	windowStart  time.Time
	windowEnd    time.Time
}

// add folds one shed window into the entry.
func (o *owedGap) add(count uint64, start, end time.Time) {
	if o.droppedCount == 0 {
		o.windowStart, o.windowEnd = start, end
		if o.windowStart.IsZero() {
			o.windowStart = end
		}
	}
	o.droppedCount += count
	if !start.IsZero() && start.Before(o.windowStart) {
		o.windowStart = start
	}
	if end.After(o.windowEnd) {
		o.windowEnd = end
	}
}

// noteOwedLocked records one shed window under its detailed key.
// Once the detailed map is full, the window folds into a
// service-level aggregate gap (no allocation or build detail) so the
// loss still surfaces in reads instead of vanishing past the key
// cap. Only when the aggregate map is exhausted too does the count
// stay in the GapsLost counter.
func (a *AsyncIngester) noteOwedLocked(key owedGapKey, count uint64, start, end time.Time) {
	if owed, ok := a.owed[key]; ok {
		owed.add(count, start, end)
		return
	}
	if len(a.owed) < maxIngestOwedGaps {
		owed := &owedGap{key: key}
		owed.add(count, start, end)
		a.owed[key] = owed
		return
	}
	fold := key
	fold.allocationID = ""
	fold.buildID = ""
	fold.summaryID = ""
	if owed, ok := a.owedFold[fold]; ok {
		owed.add(count, start, end)
		return
	}
	if len(a.owedFold) >= maxIngestOwedGaps {
		a.gapsLost.Add(count)
		return
	}
	owed := &owedGap{key: fold}
	owed.add(count, start, end)
	a.owedFold[fold] = owed
}

// NewAsyncIngester builds the ingest queue. A nil or disabled store
// makes Enqueue a no-op and Run return immediately.
func NewAsyncIngester(store flushStore, cfg AsyncIngesterConfig) *AsyncIngester {
	queueFlushes := cfg.QueueFlushes
	if queueFlushes <= 0 {
		queueFlushes = defaultIngestQueueFlushes
	}
	queueBytes := cfg.QueueBytes
	if queueBytes <= 0 {
		queueBytes = defaultIngestQueueBytes
	}
	rate := cfg.RatePerSec
	if rate <= 0 {
		rate = defaultIngestRatePerSec
	}
	burst := cfg.Burst
	if burst <= 0 {
		burst = defaultIngestBurst
	}
	grace := cfg.ShutdownGrace
	if grace <= 0 {
		grace = defaultIngestShutdownGrace
	}
	return &AsyncIngester{
		store:          store,
		queue:          make(chan pendingFlush, queueFlushes),
		queueByteLimit: queueBytes,
		limiter:        logpipeline.NewLimiter(rate, burst),
		backoff:        &logpipeline.Backoff{},
		owed:           make(map[owedGapKey]*owedGap),
		owedFold:       make(map[owedGapKey]*owedGap),
		shutdownGrace:  grace,
	}
}

// flushEstimateBytes approximates one flush's retained size: line
// text plus a fixed per-row allowance for decoded structure
// overhead. It bounds the queue's memory footprint, not the wire
// size (which producers cap at logpipeline.MaxBatchBytes).
func flushEstimateBytes(flush pendingFlush) int64 {
	size := int64(0)
	for _, in := range flush.lines {
		size += int64(len(in.Line)) + ingestRowOverheadBytes
	}
	for range flush.gaps {
		size += ingestRowOverheadBytes
	}
	return size
}

// EnqueueAgentBatch converts one validated batch and queues it for
// durable write. It never blocks and never fails: overload sheds
// with gap accounting. It returns false only once the shutdown drain
// finished, when nothing can flush anymore and the caller should
// fall back to a synchronous write or rely on producer replay.
func (a *AsyncIngester) EnqueueAgentBatch(agentID string, batch *agentv1.LogBatch) bool {
	if a == nil || a.store == nil || !a.store.Enabled() || batch == nil {
		return true
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
	// The ingest guard surfaces its rate-limited lines as gaps above,
	// so the limiter's own denied map is redundant here: drain it so
	// allocation churn cannot grow it without a bound.
	a.limiter.DrainDrops()
	if !a.enqueue(pendingFlush{lines: kept, gaps: gaps}) {
		a.rejectFlush(kept, gaps)
		return false
	}
	a.acceptedLines.Add(uint64(len(kept)))
	return true
}

// EnqueueLines queues platform-emitted lines for durable write under
// the same bounded contract as agent batches: never blocks, sheds
// with gap accounting past the queue cap, retries across outages.
// It returns false only once the shutdown drain finished.
func (a *AsyncIngester) EnqueueLines(lines []LogLineInput) bool {
	if a == nil || a.store == nil || !a.store.Enabled() || len(lines) == 0 {
		return true
	}
	if !a.enqueue(pendingFlush{lines: lines}) {
		a.rejectFlush(lines, nil)
		return false
	}
	a.acceptedLines.Add(uint64(len(lines)))
	return true
}

// rejectFlush accounts a flush that arrived after the shutdown
// drain: nothing can persist it as rows anymore, so the loss lands
// in the loud accounting — producers replay their retained spool
// window on reconnect and synchronous fallbacks may still write
// while the store closes.
func (a *AsyncIngester) rejectFlush(lines []LogLineInput, gaps []GapInput) {
	count := uint64(len(lines))
	for _, gap := range gaps {
		count += gap.DroppedCount
	}
	a.gapsLost.Add(count)
	slog.Warn("log ingest rejected after shutdown drain",
		"lines", len(lines), "gaps", len(gaps),
		"accepted_lines", a.acceptedLines.Load(),
		"flushed_lines", a.flushedLines.Load(),
		"gaps_lost", a.gapsLost.Load())
}

// enqueue admits one flush under the admission lock so sealing the
// queue at drain end is atomic with admission: a flush is either
// visible to the drain or rejected, never lost in between.
func (a *AsyncIngester) enqueue(flush pendingFlush) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.drainDone {
		return false
	}
	flush.bytes = flushEstimateBytes(flush)
	if a.queueBytes.Load()+flush.bytes > a.queueByteLimit {
		a.shedFlushLocked(flush)
		return true
	}
	select {
	case a.queue <- flush:
		a.queueBytes.Add(flush.bytes)
	default:
		a.shedFlushLocked(flush)
	}
	return true
}

// sealAdmission closes the queue against further admissions and
// returns every flush that raced in since the drain last found the
// queue empty, oldest first. The seal and the pull share the
// admission lock, so a racing batch either lands in the returned set
// or is rejected and accounted by enqueue — never lost in between.
func (a *AsyncIngester) sealAdmission() []pendingFlush {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.drainDone = true
	var rest []pendingFlush
	for {
		select {
		case next := <-a.queue:
			a.queueBytes.Add(-next.bytes)
			rest = append(rest, next)
		default:
			return rest
		}
	}
}

// Run flushes queued batches until ctx ends, then drains the
// accepted backlog under a grace deadline so shutdown does not
// discard data the Sync loop already acknowledged. Flushes retry with
// backoff across a ClickHouse outage; only a hard kill or a grace
// expiry drops the backlog, which agents then replay from their
// recent spool window or which surfaces in the loud shutdown
// accounting. Run always blocks until ctx ends, even when disabled,
// so hosting
// servers never observe an early clean return as a shutdown signal.
func (a *AsyncIngester) Run(ctx context.Context) error {
	if a == nil || a.store == nil || !a.store.Enabled() {
		<-ctx.Done()
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			a.drainShutdown(ctx, pendingFlush{})
			return nil
		case flush := <-a.queue:
			a.queueBytes.Add(-flush.bytes)
			a.coalesce(&flush)
			a.attachOwed(&flush)
			if len(flush.lines) == 0 && len(flush.gaps) == 0 {
				continue
			}
			if err := a.flushWithRetry(ctx, flush); err != nil {
				if ctx.Err() != nil {
					// The dequeued batch was never accepted and its
					// attached gaps already left the owed maps: hand
					// the in-flight flush to the drain so shutdown
					// does not drop accepted data mid-retry.
					a.drainShutdown(ctx, flush)
					return nil
				}
				return err
			}
		}
	}
}

// drainShutdown flushes everything already accepted — the in-flight
// batch handed over from Run, queued batches, owed gap windows, and
// anything arriving during the drain — under a grace deadline
// decoupled from the canceled run context. It ends by sealing
// admission atomically with the final pull, so a batch racing the
// seal either flushes here or is rejected and accounted, never
// silently dropped. If the grace expires first, every line and gap
// count still held lands in the loud shutdown accounting instead of
// vanishing.
func (a *AsyncIngester) drainShutdown(ctx context.Context, inFlight pendingFlush) {
	graceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.shutdownGrace)
	defer cancel()
	remaining := []pendingFlush{inFlight}
	sealed := false
	for {
		if len(remaining) == 0 {
			if sealed {
				return
			}
			remaining = a.sealAdmission()
			sealed = true
			continue
		}
		flush := remaining[0]
		remaining = remaining[1:]
		a.coalesce(&flush)
		a.attachOwed(&flush)
		if len(flush.lines) == 0 && len(flush.gaps) == 0 {
			continue
		}
		if err := a.flushWithRetry(graceCtx, flush); err != nil {
			if !sealed {
				remaining = append(remaining, a.sealAdmission()...)
			}
			// Gap counts shed while the drain was retrying are still
			// owed; fold them into the accounted loss so they surface
			// as gaps instead of vanishing.
			a.attachOwed(&flush)
			a.accountDrainedLoss(append([]pendingFlush{flush}, remaining...))
			return
		}
	}
}

// accountDrainedLoss lands a drain that expired its grace in the
// loud accounting: every held line and gap count is added to
// GapsLost and logged in full, so abandoning the accepted backlog is
// observable instead of silent.
func (a *AsyncIngester) accountDrainedLoss(flushes []pendingFlush) {
	var lines, gapRows, gapLines uint64
	for _, flush := range flushes {
		lines += uint64(len(flush.lines))
		gapRows += uint64(len(flush.gaps))
		for _, gap := range flush.gaps {
			gapLines += gap.DroppedCount
		}
	}
	a.gapsLost.Add(lines + gapLines)
	slog.Error("log ingest shutdown drain dropped accepted data",
		"lines", lines,
		"gaps", gapRows,
		"gap_lines", gapLines,
		"accepted_lines", a.acceptedLines.Load(),
		"flushed_lines", a.flushedLines.Load(),
		"shed_lines", a.shedLines.Load(),
		"gaps_lost", a.gapsLost.Load())
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
		QueuedBytes:   a.queueBytes.Load(),
		AcceptedLines: a.acceptedLines.Load(),
		ShedLines:     a.shedLines.Load(),
		OwedGaps:      len(a.owed) + len(a.owedFold),
		GapsLost:      a.gapsLost.Load(),
		FlushedLines:  a.flushedLines.Load(),
		LastFlush:     a.lastFlush,
		LastError:     a.lastError,
		LastErrorAt:   a.lastErrorAt,
	}
}

// shedFlushLocked converts a queue-overflowed flush into owed gap
// rows so the loss still surfaces in reads. The admission lock must
// be held.
func (a *AsyncIngester) shedFlushLocked(flush pendingFlush) {
	now := time.Now().UTC()
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
		a.noteOwedLocked(key, count, now, now)
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
			summaryID:    gap.SummaryID,
		}
		a.noteOwedLocked(key, gap.DroppedCount, gap.WindowStart, gap.WindowEnd)
	}
}

// coalesce drains additional queued flushes into one ClickHouse
// write, bounded so a single insert stays small.
// coalesce merges queued flushes into the in-flight one while it
// retries, up to the row and byte budgets. Pulled flushes return
// their queue budget. The merged buffer stays under one budget plus
// one admitted flush, so backlog drainage can never accumulate
// maximum-size lines without bound.
func (a *AsyncIngester) coalesce(flush *pendingFlush) {
	for len(flush.lines) < maxIngestCoalescedLines && flush.bytes < a.queueByteLimit {
		select {
		case next := <-a.queue:
			a.queueBytes.Add(-next.bytes)
			flush.lines = append(flush.lines, next.lines...)
			flush.gaps = append(flush.gaps, next.gaps...)
			flush.bytes += next.bytes
		default:
			return
		}
	}
}

// attachOwed pops owed gap rows into the outgoing flush.
func (a *AsyncIngester) attachOwed(flush *pendingFlush) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.attachOwedLocked(flush)
}

func (a *AsyncIngester) attachOwedLocked(flush *pendingFlush) {
	if len(a.owed) == 0 && len(a.owedFold) == 0 {
		return
	}
	for _, owedMap := range []map[owedGapKey]*owedGap{a.owed, a.owedFold} {
		for key, owed := range owedMap {
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
				SummaryID:    key.summaryID,
			})
			delete(owedMap, key)
		}
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
