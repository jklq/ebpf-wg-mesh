package logs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
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
	maxIngestOwedGaps            = 4096
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
	OwedGaps      int
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
	store          flushStore
	backlog        *logpipeline.Spool
	wake           chan struct{}
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
	lines        []LogLineInput
	gaps         []GapInput
	corruptDrops map[string]logpipeline.CorruptDrop
}

// journalRecord is one durable queue entry.
type journalRecord struct {
	Lines []LogLineInput `json:"lines"`
	Gaps  []GapInput     `json:"gaps"`
}

type owedGapKey struct {
	serviceID    string
	allocationID string
	buildID      string
	logType      string
	stream       string
	reason       string
	reporter     string
	// summaryID is the producer's stable summary identity; replays replace
	// the same gap row instead of double-counting.
	summaryID string
}

type owedGap struct {
	key          owedGapKey
	droppedCount uint64
	windowStart  time.Time
	windowEnd    time.Time
}

func (o *owedGap) add(count uint64, start, end time.Time) {
	o.widen(start, end)
	o.droppedCount += count
}

// replay replaces the count for a re-reported window instead of double-counting.
func (o *owedGap) replay(count uint64, start, end time.Time) {
	o.widen(start, end)
	o.droppedCount = count
}

func (o *owedGap) widen(start, end time.Time) {
	if o.droppedCount == 0 && o.windowStart.IsZero() && o.windowEnd.IsZero() {
		o.windowStart = start
		if o.windowStart.IsZero() {
			o.windowStart = end
		}
		o.windowEnd = end
		return
	}
	if !start.IsZero() && start.Before(o.windowStart) {
		o.windowStart = start
	}
	if end.After(o.windowEnd) {
		o.windowEnd = end
	}
}

// noteOwedLocked records one shed window. Past the detailed key cap the
// window folds into a service-level aggregate; past that it lands in GapsLost.
func (a *AsyncIngester) noteOwedLocked(key owedGapKey, count uint64, start, end time.Time) {
	if owed, ok := a.owed[key]; ok {
		if key.summaryID != "" {
			owed.replay(count, start, end)
		} else {
			owed.add(count, start, end)
		}
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
			SyncWrites:   true,
			RejectOnFull: true,
		})
		if err != nil {
			return nil, fmt.Errorf("open log ingest journal: %w", err)
		}
	}
	return &AsyncIngester{
		store:          store,
		backlog:        backlog,
		wake:           make(chan struct{}, 1),
		queueByteLimit: queueBytes,
		limiter:        logpipeline.NewLimiter(rate, burst),
		backoff:        &logpipeline.Backoff{},
		owed:           make(map[owedGapKey]*owedGap),
		owedFold:       make(map[owedGapKey]*owedGap),
		shutdownGrace:  grace,
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
	// Producer drop reports consume the same guard; gap rows are retained
	// writes too.
	keptGaps := gaps[:0]
	deniedGaps := make(map[owedGapKey]GapInput)
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
		mergeKey := owedGapKey{
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
	result := AdmitAccepted
	for _, record := range splitJournalRecords(flush) {
		payload, err := json.Marshal(record)
		if err != nil {
			slog.Error("encode log ingest journal record", "error", err)
			if !a.shedRecord(record) {
				result = AdmitRetry
				break
			}
			continue
		}
		err = a.backlog.Append(journalRecordKey(record), "", time.Now().UTC(), payload)
		if err != nil {
			if !a.shedRecord(record) {
				// An acknowledged gap may never live only in memory; the
				// producer's copy still carries the lines.
				result = AdmitRetry
				break
			}
			if !errors.Is(err, logpipeline.ErrSpoolFull) && !errors.Is(err, logpipeline.ErrRecordTooLarge) {
				slog.Warn("append log ingest journal", "error", err)
			}
			continue
		}
		a.acceptedLines.Add(uint64(len(record.Lines)))
	}
	select {
	case a.wake <- struct{}{}:
	default:
	}
	return result
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
		attached := a.attachOwed(&flush)
		if err := a.flushWithRetry(ctx, flush); err != nil {
			a.backlog.Release()
			a.reoweGaps(flush.gaps[len(flush.gaps)-attached:])
			if ctx.Err() != nil {
				a.drainShutdown(ctx)
				return nil
			}
			return err
		}
		if err := a.backlog.AcknowledgeCorruptDrops(flush.corruptDrops); err != nil {
			slog.Warn("acknowledge log journal corruption gaps", "error", err)
		}
		if err := a.backlog.Commit(cursor); err != nil {
			slog.Warn("commit log ingest journal", "error", err)
		}
	}
}

// maxIngestAttributionKeyBytes bounds the per-service attribution in one
// journal record's key.
const maxIngestAttributionKeyBytes = 512

// journalRecordKey attributes a record to the services it carries, so a
// corrupted record surfaces as per-service gap rows instead of vanishing.
func journalRecordKey(record journalRecord) string {
	counts := make(map[string]uint64)
	for _, in := range record.Lines {
		counts[in.ServiceID]++
	}
	for _, gap := range record.Gaps {
		counts[gap.ServiceID] += gap.DroppedCount
	}
	services := make([]string, 0, len(counts))
	for service := range counts {
		services = append(services, service)
	}
	sort.Strings(services)
	var b strings.Builder
	b.WriteString("ingest")
	for _, service := range services {
		part := fmt.Sprintf("|%s:%d", service, counts[service])
		if b.Len()+len(part) > maxIngestAttributionKeyBytes {
			break
		}
		b.WriteString(part)
	}
	return b.String()
}

func parseIngestKeyCounts(key string) map[string]uint64 {
	counts := make(map[string]uint64)
	for _, part := range strings.Split(strings.TrimPrefix(key, "ingest"), "|") {
		if part == "" {
			continue
		}
		service, raw, ok := strings.Cut(part, ":")
		if !ok || service == "" {
			continue
		}
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			continue
		}
		counts[service] += n
	}
	return counts
}

// spoolDropsToGaps converts corrupted journal records into gap rows.
func (a *AsyncIngester) spoolDropsToGaps() ([]GapInput, map[string]logpipeline.CorruptDrop) {
	corrupt := a.backlog.PendingCorruptDrops()
	if len(corrupt) == 0 {
		return nil, nil
	}
	now := time.Now().UTC()
	var gaps []GapInput
	reported := make(map[string]logpipeline.CorruptDrop)
	for key, drop := range corrupt {
		for service, count := range parseIngestKeyCounts(key) {
			gaps = append(gaps, GapInput{
				ServiceID:    service,
				LogType:      normalizeLogType(""),
				WindowStart:  now,
				WindowEnd:    now,
				DroppedCount: count * drop.Count,
				Reason:       logpipeline.ReasonCorruptSpool,
				Reporter:     reporterControlPlane,
				SummaryID:    drop.ID + ":" + service,
			})
			reported[key] = drop
		}
	}
	return gaps, reported
}

// nextFlush merges one bounded round of journal records into a single store
// write. Every read pairs with a commit or release.
func (a *AsyncIngester) nextFlush(ctx context.Context) (pendingFlush, logpipeline.Cursor, bool, error) {
	drops, corruptDrops := a.spoolDropsToGaps()
	records, cursor, err := a.backlog.Read(ingestFlushRecords)
	if err != nil {
		return pendingFlush{}, cursor, false, err
	}
	if len(records) == 0 {
		// Gap-only pump: owed shed windows must reach reads even when idle.
		a.mu.Lock()
		hasOwed := len(a.owed) > 0 || len(a.owedFold) > 0
		a.mu.Unlock()
		if !hasOwed && len(drops) == 0 {
			return pendingFlush{}, cursor, false, nil
		}
		owed := pendingFlush{gaps: drops, corruptDrops: corruptDrops}
		a.attachOwed(&owed)
		if len(owed.gaps) == 0 {
			return pendingFlush{}, cursor, false, nil
		}
		return owed, cursor, true, nil
	}
	flush := pendingFlush{gaps: drops, corruptDrops: corruptDrops}
	for _, record := range records {
		var decoded journalRecord
		if err := json.Unmarshal(record.Payload, &decoded); err != nil {
			slog.Error("decode log ingest journal record", "error", err, "key", record.Key, "id", record.ID)
			continue
		}
		flush.lines = append(flush.lines, decoded.Lines...)
		flush.gaps = append(flush.gaps, decoded.Gaps...)
	}
	if len(flush.lines) == 0 && len(flush.gaps) == 0 {
		if err := a.backlog.Commit(cursor); err != nil {
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
		if err != nil || !ok {
			return
		}
		attached := a.attachOwed(&flush)
		if err := a.flushWithRetry(graceCtx, flush); err != nil {
			a.backlog.Release()
			a.reoweGaps(flush.gaps[len(flush.gaps)-attached:])
			slog.Warn("log ingest shutdown drain expired; queued lines stay journaled for the next boot",
				"pending_records", a.backlog.Stats().PendingRecords,
				"error", err)
			return
		}
		if err := a.backlog.AcknowledgeCorruptDrops(flush.corruptDrops); err != nil {
			slog.Warn("acknowledge log journal corruption gaps", "error", err)
		}
		if err := a.backlog.Commit(cursor); err != nil {
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
		backlogStats := a.backlog.Stats()
		queuedFlushes = int(backlogStats.PendingRecords)
		queuedBytes = backlogStats.Bytes
	}
	return IngesterStats{
		QueuedFlushes: queuedFlushes,
		QueuedBytes:   queuedBytes,
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

// shedRecord journals an unjournaled record's loss as gap rows. False means
// the loss accounting itself is not durable and the caller must reject.
func (a *AsyncIngester) shedRecord(record journalRecord) bool {
	gaps := a.shedToGaps(pendingFlush{lines: record.Lines, gaps: record.Gaps})
	if !a.journalGaps(gaps) {
		return false
	}
	a.shedLines.Add(uint64(len(record.Lines)))
	return true
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
	if err := a.backlog.Append(journalRecordKey(journalRecord{Gaps: gaps}), "", time.Now().UTC(), payload); err != nil {
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

// noteGapLocked re-owes one gap row. The admission lock must be held.
func (a *AsyncIngester) noteGapLocked(gap GapInput) {
	if gap.ServiceID == "" || gap.DroppedCount == 0 {
		return
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

// reoweGaps returns a failed flush's gap rows to the owed maps.
func (a *AsyncIngester) reoweGaps(gaps []GapInput) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, gap := range gaps {
		a.noteGapLocked(gap)
	}
}

// attachOwed pops owed gap rows into the outgoing flush.
func (a *AsyncIngester) attachOwed(flush *pendingFlush) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.attachOwedLocked(flush)
}

func (a *AsyncIngester) attachOwedLocked(flush *pendingFlush) int {
	if len(a.owed) == 0 && len(a.owedFold) == 0 {
		return 0
	}
	attached := 0
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
			attached++
		}
	}
	return attached
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
