package logs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/logpipeline"
)

const (
	defaultIngestQueueBytes      = 64 << 20
	ingestJournalRecordBytes     = 2 << 20
	ingestFlushRecords           = 8
	ingestRowOverheadBytes       = 256
	ingestAttributeOverheadBytes = 48
	defaultIngestRatePerSec      = 2000.0
	defaultIngestBurst           = 10000
	maxIngestCoalescedLines      = 5000
	defaultIngestShutdownGrace   = 15 * time.Second
	reporterControlPlane         = "controlplane"
)

// AsyncIngesterConfig bounds the control-plane log ingest path.
type AsyncIngesterConfig struct {
	// SpoolDir holds the durable ingest journal. Required when the store is enabled.
	SpoolDir string
	// QueueBytes caps the journal's retained size; past it whole batches
	// shed with owed gap rows. Defaults to 64 MiB.
	QueueBytes int64
	// RatePerSec and Burst bound the per-allocation ingest guard above
	// agent-side limits. Defaults to 2000/s and 10000.
	RatePerSec float64
	Burst      int
	// ShutdownGrace bounds the shutdown drain. Defaults to 15s.
	ShutdownGrace time.Duration
}

// IngesterStats reports ingest health and lifetime counters.
type IngesterStats struct {
	QueuedFlushes int
	QueuedBytes   int64
	AcceptedLines uint64
	ShedLines     uint64
	GapsLost      uint64
	FlushedLines  uint64
	LastFlush     time.Time
	LastError     string
	LastErrorAt   time.Time
}

// flushStore is the durable sink behind the async ingester.
type flushStore interface {
	Enabled() bool
	WriteLogLines(context.Context, []LogLineInput) error
	WriteGaps(context.Context, []GapInput) error
}

// AsyncIngester decouples agent log batches from ClickHouse writes behind a
// durable journal: acceptance follows the durable append, so a crash or
// outage never loses acknowledged batches. Overload sheds whole batches
// with gap accounting; Run flushes with unbounded retry.
type AsyncIngester struct {
	store   flushStore
	backlog *logpipeline.Spool
	wake    chan struct{}
	limiter *logpipeline.Limiter
	backoff *logpipeline.Backoff

	acceptedLines atomic.Uint64
	shedLines     atomic.Uint64
	flushedLines  atomic.Uint64
	gapsLost      atomic.Uint64

	mu          sync.Mutex
	drainDone   bool
	lastFlush   time.Time
	lastError   string
	lastErrorAt time.Time

	shutdownGrace time.Duration
}

type pendingFlush struct {
	lines []LogLineInput
	gaps  []GapInput
}

// journalRecord is one durable queue entry.
type journalRecord struct {
	Lines []LogLineInput `json:"lines"`
	Gaps  []GapInput     `json:"gaps"`
}

type gapKey struct {
	serviceID, allocationID, buildID, logType, stream, reason, reporter string
}

// NewAsyncIngester builds the ingest queue. A nil or disabled store makes
// Enqueue a no-op; otherwise a durable journal is required.
func NewAsyncIngester(store flushStore, cfg AsyncIngesterConfig) (*AsyncIngester, error) {
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
	var backlog *logpipeline.Spool
	if store != nil && store.Enabled() {
		var err error
		backlog, err = logpipeline.OpenSpool(logpipeline.SpoolConfig{
			Dir:          cfg.SpoolDir,
			MaxBytes:     queueBytes,
			RejectOnFull: true,
		})
		if err != nil {
			return nil, fmt.Errorf("open log ingest journal: %w", err)
		}
	}
	return &AsyncIngester{
		store:         store,
		backlog:       backlog,
		wake:          make(chan struct{}, 1),
		limiter:       logpipeline.NewLimiter(rate, burst),
		backoff:       &logpipeline.Backoff{},
		shutdownGrace: grace,
	}, nil
}

// flushEstimateBytes approximates one flush's retained size to bound the
// queue's memory footprint.
func flushEstimateBytes(flush pendingFlush) int64 {
	size := int64(0)
	for _, in := range flush.lines {
		row := int64(len(in.Line)) + ingestRowOverheadBytes
		row += int64(len(in.ID) + len(in.ProjectID) + len(in.EnvironmentID) +
			len(in.ServiceID) + len(in.AllocationID) + len(in.AgentID) +
			len(in.Stream) + len(in.BuildID) + len(in.Stage) + len(in.Event))
		for k, v := range in.Attributes {
			row += int64(len(k)+len(v)) + ingestAttributeOverheadBytes
		}
		size += row
	}
	for _, gap := range flush.gaps {
		row := int64(ingestRowOverheadBytes)
		row += int64(len(gap.ProjectID) + len(gap.ServiceID) + len(gap.AllocationID) +
			len(gap.BuildID) + len(gap.Stream) + len(gap.Reason) +
			len(gap.Reporter) + len(gap.SummaryID))
		size += row
	}
	return size
}

// Admit is the durable decision for one offered batch.
type Admit int

const (
	// AdmitAccepted means the batch is journaled, or its loss is a durable
	// gap. The caller may acknowledge it.
	AdmitAccepted Admit = iota + 1
	// AdmitRetry means neither the batch nor a gap fit; the producer still
	// holds the lines, so the caller must not acknowledge.
	AdmitRetry
	// AdmitClosed means the shutdown drain has finished.
	AdmitClosed
)

// EnqueueAgentBatch converts one validated batch and queues it for durable
// write. It never blocks; overload sheds with gap accounting.
func (a *AsyncIngester) EnqueueAgentBatch(agentID string, batch *agentv1.LogBatch) Admit {
	if a == nil || a.store == nil || !a.store.Enabled() || batch == nil {
		return AdmitAccepted
	}
	inputs, gaps := convertAgentBatch(agentID, batch)
	now := time.Now().UTC()
	kept := inputs[:0]
	limited := make(map[gapKey]uint64)
	for _, in := range inputs {
		key := in.AllocationID
		if key == "" {
			key = in.BuildID
		}
		if key == "" || a.limiter.Allow("alloc:"+key) {
			kept = append(kept, in)
			continue
		}
		limited[gapKey{
			serviceID:    in.ServiceID,
			allocationID: in.AllocationID,
			buildID:      in.BuildID,
			logType:      string(normalizeLogType(in.LogType)),
			stream:       normalizeLogStream(in.Stream),
			reason:       logpipeline.ReasonRateLimited,
			reporter:     reporterControlPlane,
		}]++
	}
	// Producer drop reports consume the same guard; gap rows are retained
	// writes too.
	keptGaps := gaps[:0]
	deniedGaps := make(map[gapKey]GapInput)
	for _, gap := range gaps {
		key := gap.AllocationID
		if key == "" {
			key = gap.BuildID
		}
		if key == "" || a.limiter.Allow("alloc:"+key) {
			keptGaps = append(keptGaps, gap)
			continue
		}
		// A denied report is coalesced, never erased; anonymous reports merge
		// per window here while summary identities keep replace semantics.
		if gap.SummaryID != "" {
			keptGaps = append(keptGaps, gap)
			continue
		}
		mergeKey := gapKey{
			serviceID:    gap.ServiceID,
			allocationID: gap.AllocationID,
			buildID:      gap.BuildID,
			logType:      string(normalizeLogType(gap.LogType)),
			stream:       normalizeLogStream(gap.Stream),
			reason:       gap.Reason,
			reporter:     gap.Reporter,
		}
		merged := deniedGaps[mergeKey]
		merged.ServiceID = gap.ServiceID
		merged.AllocationID = gap.AllocationID
		merged.BuildID = gap.BuildID
		merged.LogType = LogType(mergeKey.logType)
		merged.Stream = mergeKey.stream
		merged.Reason = gap.Reason
		merged.Reporter = gap.Reporter
		if merged.DroppedCount == 0 || gap.WindowStart.Before(merged.WindowStart) {
			merged.WindowStart = gap.WindowStart
		}
		if gap.WindowEnd.After(merged.WindowEnd) {
			merged.WindowEnd = gap.WindowEnd
		}
		merged.DroppedCount += gap.DroppedCount
		deniedGaps[mergeKey] = merged
	}
	for _, merged := range deniedGaps {
		keptGaps = append(keptGaps, merged)
	}
	gaps = keptGaps
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
	// The limiter's denied map is redundant here; drain it to bound churn growth.
	a.limiter.DrainDrops()
	if len(kept) == 0 && len(gaps) == 0 {
		return AdmitAccepted
	}
	switch result := a.enqueue(pendingFlush{lines: kept, gaps: gaps}); result {
	case AdmitAccepted:
		return AdmitAccepted
	default:
		// Count the refused batch so it is never silent.
		a.rejectFlush(kept, gaps, result)
		return result
	}
}

// EnqueueLines queues platform-emitted lines under the same contract as
// agent batches: never blocks, sheds with gap accounting, retries across outages.
func (a *AsyncIngester) EnqueueLines(lines []LogLineInput) Admit {
	if a == nil || a.store == nil || !a.store.Enabled() || len(lines) == 0 {
		return AdmitAccepted
	}
	switch result := a.enqueue(pendingFlush{lines: lines}); result {
	case AdmitAccepted:
		return AdmitAccepted
	default:
		a.rejectFlush(lines, nil, result)
		return result
	}
}

// rejectFlush accounts a flush the journal refused: the loss lands in the
// loud accounting while the producer still holds the lines.
func (a *AsyncIngester) rejectFlush(lines []LogLineInput, gaps []GapInput, why Admit) {
	count := uint64(len(lines))
	for _, gap := range gaps {
		count += gap.DroppedCount
	}
	a.gapsLost.Add(count)
	reason := "shutdown drain"
	if why == AdmitRetry {
		reason = "journal full"
	}
	slog.Warn("log ingest rejected batch",
		"reason", reason,
		"lines", len(lines), "gaps", len(gaps),
		"accepted_lines", a.acceptedLines.Load(),
		"flushed_lines", a.flushedLines.Load(),
		"gaps_lost", a.gapsLost.Load())
}

// enqueue appends one flush to the journal in bounded records and wakes the
// flusher. Admission holds the lock so sealing at drain end is atomic with
// it: a flush is either visible to the drain or rejected, never lost.
func (a *AsyncIngester) enqueue(flush pendingFlush) Admit {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.drainDone {
		return AdmitClosed
	}
	records := splitJournalRecords(flush)
	queued := make([]logpipeline.Record, 0, len(records))
	for _, record := range records {
		payload, err := json.Marshal(record)
		if err != nil {
			slog.Error("encode log ingest journal", "error", err)
			return AdmitRetry
		}
		queued = append(queued, logpipeline.Record{ObservedAt: time.Now().UTC(), Payload: payload})
	}
	if err := a.backlog.Append(queued...); err != nil {
		if !a.journalGaps(a.shedToGaps(flush)) {
			// No acceptance is acknowledged unless either the entire batch
			// or its exact loss accounting commits durably.
			return AdmitRetry
		}
		a.shedLines.Add(uint64(len(flush.lines)))
		if !errors.Is(err, logpipeline.ErrSpoolFull) && !errors.Is(err, logpipeline.ErrRecordTooLarge) {
			slog.Warn("append log ingest journal", "error", err)
		}
	} else {
		a.acceptedLines.Add(uint64(len(flush.lines)))
	}
	select {
	case a.wake <- struct{}{}:
	default:
	}
	return AdmitAccepted
}

// splitJournalRecords cuts one flush into journal records bounded by rows
// and estimated bytes.
func splitJournalRecords(flush pendingFlush) []journalRecord {
	var records []journalRecord
	cur := journalRecord{}
	curBytes := int64(0)
	flushRow := func(record *journalRecord, bytes *int64, lines []LogLineInput, gaps []GapInput) {
		slice := pendingFlush{lines: lines, gaps: gaps}
		est := flushEstimateBytes(slice)
		if len(record.Lines)+len(record.Gaps) > 0 &&
			(len(record.Lines)+len(record.Gaps) >= maxIngestCoalescedLines || *bytes+est > ingestJournalRecordBytes) {
			records = append(records, *record)
			*record = journalRecord{}
			*bytes = 0
		}
		record.Lines = append(record.Lines, lines...)
		record.Gaps = append(record.Gaps, gaps...)
		*bytes += est
	}
	for _, in := range flush.lines {
		flushRow(&cur, &curBytes, []LogLineInput{in}, nil)
	}
	for _, gap := range flush.gaps {
		flushRow(&cur, &curBytes, nil, []GapInput{gap})
	}
	if len(cur.Lines)+len(cur.Gaps) > 0 {
		records = append(records, cur)
	}
	return records
}

// seal stops admission once the shutdown drain finished.
func (a *AsyncIngester) seal() {
	a.mu.Lock()
	a.drainDone = true
	a.mu.Unlock()
}

// Run flushes the journal until ctx ends, then drains the backlog under a
// grace deadline. Undrained lines stay journaled for the next boot. Run
// blocks until ctx ends even when disabled.
func (a *AsyncIngester) Run(ctx context.Context) error {
	if a == nil || a.store == nil || !a.store.Enabled() {
		<-ctx.Done()
		return nil
	}
	for {
		flush, cursor, ok, err := a.nextFlush(ctx)
		if err != nil {
			return err
		}
		if !ok {
			select {
			case <-a.wake:
				continue
			case <-ctx.Done():
				a.drainShutdown(ctx)
				return nil
			}
		}
		if err := a.flushWithRetry(ctx, flush); err != nil {
			a.backlog.Release()
			if ctx.Err() != nil {
				a.drainShutdown(ctx)
				return nil
			}
			return err
		}
		if err := a.backlog.Commit(cursor, nil); err != nil {
			slog.Warn("commit log ingest journal", "error", err)
		}
	}
}

// nextFlush merges one bounded round of journal records into a single store
// write. Every read pairs with a commit or release.
func (a *AsyncIngester) nextFlush(ctx context.Context) (pendingFlush, logpipeline.Cursor, bool, error) {
	records, cursor, err := a.backlog.Read(ingestFlushRecords)
	if err != nil {
		return pendingFlush{}, cursor, false, err
	}
	if len(records) == 0 {
		a.backlog.Release()
		return pendingFlush{}, cursor, false, nil
	}
	flush := pendingFlush{}
	for _, record := range records {
		var decoded journalRecord
		if err := json.Unmarshal(record.Payload, &decoded); err != nil {
			a.backlog.Release()
			return pendingFlush{}, cursor, false, fmt.Errorf("decode durable log journal payload: %w", err)
		}
		flush.lines = append(flush.lines, decoded.Lines...)
		flush.gaps = append(flush.gaps, decoded.Gaps...)
	}
	if len(flush.lines) == 0 && len(flush.gaps) == 0 {
		if err := a.backlog.Commit(cursor, nil); err != nil {
			return pendingFlush{}, cursor, false, err
		}
		return a.nextFlush(ctx)
	}
	return flush, cursor, true, nil
}

// drainShutdown flushes the journal under a grace deadline decoupled from
// the canceled run context, then seals admission. Undrained lines stay
// journaled for the next boot.
func (a *AsyncIngester) drainShutdown(ctx context.Context) {
	graceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.shutdownGrace)
	defer cancel()
	a.seal()
	for {
		flush, cursor, ok, err := a.nextFlush(graceCtx)
		if err != nil {
			slog.Error("read log journal during shutdown", "error", err)
			return
		}
		if !ok {
			return
		}
		if err := a.flushWithRetry(graceCtx, flush); err != nil {
			a.backlog.Release()
			slog.Warn("log ingest shutdown drain expired; queued lines stay journaled for the next boot", "error", err)
			return
		}
		if err := a.backlog.Commit(cursor, nil); err != nil {
			slog.Warn("commit log ingest journal", "error", err)
		}
	}
}

func (a *AsyncIngester) Stats() IngesterStats {
	if a == nil {
		return IngesterStats{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	queuedFlushes, queuedBytes := 0, int64(0)
	if a.backlog != nil {
		backlogStats, err := a.backlog.Stats()
		if err != nil {
			a.lastError = err.Error()
			a.lastErrorAt = time.Now().UTC()
		}
		queuedFlushes = int(backlogStats.PendingRecords)
		queuedBytes = backlogStats.Bytes
	}
	return IngesterStats{
		QueuedFlushes: queuedFlushes,
		QueuedBytes:   queuedBytes,
		AcceptedLines: a.acceptedLines.Load(),
		ShedLines:     a.shedLines.Load(),
		GapsLost:      a.gapsLost.Load(),
		FlushedLines:  a.flushedLines.Load(),
		LastFlush:     a.lastFlush,
		LastError:     a.lastError,
		LastErrorAt:   a.lastErrorAt,
	}
}

// journalGaps appends one durable gap-only record.
func (a *AsyncIngester) journalGaps(gaps []GapInput) bool {
	if len(gaps) == 0 {
		return true
	}
	payload, err := json.Marshal(journalRecord{Gaps: gaps})
	if err != nil {
		slog.Error("encode log ingest gap record", "error", err)
		return false
	}
	if err := a.backlog.Append(logpipeline.Record{ObservedAt: time.Now().UTC(), Payload: payload}); err != nil {
		if !errors.Is(err, logpipeline.ErrSpoolFull) && !errors.Is(err, logpipeline.ErrRecordTooLarge) {
			slog.Warn("append log ingest gap record", "error", err)
		}
		return false
	}
	return true
}

// shedToGaps converts a queue-overflowed flush into gap rows.
func (a *AsyncIngester) shedToGaps(flush pendingFlush) []GapInput {
	now := time.Now().UTC()
	gaps := make([]GapInput, 0, len(flush.gaps)+8)
	counts := make(map[gapKey]uint64)
	for _, in := range flush.lines {
		counts[gapKey{
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
	}
	// Producer gap reports inside a shed flush keep their own rows.
	gaps = append(gaps, flush.gaps...)
	return gaps
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

// Close releases the journal after admission and Run have stopped. Accepted
// records not written during the shutdown grace remain durable for the next boot.
func (a *AsyncIngester) Close() error {
	if a == nil || a.backlog == nil {
		return nil
	}
	a.seal()
	return a.backlog.Close()
}
