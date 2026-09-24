package logs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/logpipeline"

	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeFlushStore struct {
	mu        sync.Mutex
	enabled   bool
	lines     []LogLineInput
	gaps      []GapInput
	flushes   int
	biggest   int
	failLines error
	failGaps  error
	failUntil time.Time
}

func (f *fakeFlushStore) Enabled() bool { return f.enabled }

func (f *fakeFlushStore) WriteLogLines(_ context.Context, inputs []LogLineInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushes++
	if f.failLines != nil && time.Now().Before(f.failUntil) {
		return f.failLines
	}
	if f.failLines != nil && f.failUntil.IsZero() {
		return f.failLines
	}
	f.lines = append(f.lines, inputs...)
	if len(inputs) > f.biggest {
		f.biggest = len(inputs)
	}
	return nil
}

func (f *fakeFlushStore) WriteGaps(_ context.Context, gaps []GapInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGaps != nil {
		return f.failGaps
	}
	f.gaps = append(f.gaps, gaps...)
	return nil
}

func testAgentBatch(serviceID, allocID string, count int) *agentv1.LogBatch {
	batch := &agentv1.LogBatch{AgentId: "agent-1"}
	now := time.Now().UTC()
	for i := 0; i < count; i++ {
		seq := uint64(i + 1)
		batch.Entries = append(batch.Entries, &agentv1.LogEntry{
			ObservedAt:    timestamppb.New(now),
			EnvironmentId: "env-1",
			ServiceId:     serviceID,
			AllocationId:  allocID,
			Stream:        "stdout",
			Sequence:      seq,
			Line:          "line",
			LineId:        logpipeline.AgentLineID("agent-1", "boot-1", allocID, "stdout", seq),
			LogType:       platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME,
		})
	}
	return batch
}

func waitForIngest(t *testing.T, ingester *AsyncIngester, wantFlushed uint64) IngesterStats {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		stats := ingester.Stats()
		if stats.FlushedLines >= wantFlushed {
			return stats
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d flushed lines: %+v", wantFlushed, stats)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runIngester(t *testing.T, ingester *AsyncIngester) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ingester.Run(ctx)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("ingester did not stop")
		}
	}
}

func newTestIngester(t *testing.T, store flushStore, cfg AsyncIngesterConfig) *AsyncIngester {
	t.Helper()
	if cfg.SpoolDir == "" {
		cfg.SpoolDir = t.TempDir()
	}
	ingester, err := NewAsyncIngester(store, cfg)
	if err != nil {
		t.Fatalf("NewAsyncIngester: %v", err)
	}
	return ingester
}

func TestAsyncIngesterFlushesQueuedBatches(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true}
	ingester := newTestIngester(t, store, AsyncIngesterConfig{})
	stop := runIngester(t, ingester)
	defer stop()

	ingester.EnqueueAgentBatch("agent-1", testAgentBatch("svc-1", "alloc-1", 5))
	stats := waitForIngest(t, ingester, 5)
	if stats.ShedLines != 0 || stats.OwedGaps != 0 {
		t.Fatalf("clean ingest shed lines: %+v", stats)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.lines) != 5 {
		t.Fatalf("expected 5 flushed lines, got %d", len(store.lines))
	}
}

func TestAsyncIngesterRateLimitsAbusiveAllocations(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true}
	ingester := newTestIngester(t, store, AsyncIngesterConfig{RatePerSec: 1, Burst: 2})
	stop := runIngester(t, ingester)
	defer stop()

	ingester.EnqueueAgentBatch("agent-1", testAgentBatch("svc-1", "alloc-hot", 10))
	stats := waitForIngest(t, ingester, 2)
	if stats.ShedLines != 8 {
		t.Fatalf("shed %d lines, want 8", stats.ShedLines)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.gaps) != 1 || store.gaps[0].DroppedCount != 8 {
		t.Fatalf("missing ingest gap for shed lines: %+v", store.gaps)
	}
	gap := store.gaps[0]
	if gap.Reason != logpipeline.ReasonRateLimited || gap.AllocationID != "alloc-hot" || gap.ServiceID != "svc-1" {
		t.Fatalf("gap misattributed: %+v", gap)
	}
}

func TestAsyncIngesterShedsWithOwedGapsWhenJournalFull(t *testing.T) {
	t.Parallel()

	// No Run loop: the journal fills and stays full.
	store := &fakeFlushStore{enabled: true}
	ingester := newTestIngester(t, store, AsyncIngesterConfig{QueueBytes: 3000})

	for i := 0; i < 20 && ingester.Stats().ShedLines == 0; i++ {
		if !ingester.EnqueueAgentBatch("agent-1", testAgentBatch("svc-1", "alloc-1", 3)) {
			t.Fatal("enqueue rejected before the drain seal")
		}
	}
	stats := ingester.Stats()
	if stats.ShedLines == 0 || stats.AcceptedLines == 0 {
		t.Fatalf("journal budget must keep some and shed some: %+v", stats)
	}

	// Draining the journal flushes the owed gap alongside the lines.
	stop := runIngester(t, ingester)
	defer stop()
	waitForIngest(t, ingester, stats.AcceptedLines)
	store.mu.Lock()
	defer store.mu.Unlock()
	var gapLines uint64
	for _, gap := range store.gaps {
		gapLines += gap.DroppedCount
	}
	if gapLines != stats.ShedLines {
		t.Fatalf("owed gap not flushed: %+v", store.gaps)
	}
	if store.gaps[0].Reason != logpipeline.ReasonIngestOverflow {
		t.Fatalf("owed gap reason wrong: %+v", store.gaps)
	}
}

func TestAsyncIngesterRetriesAcrossBackendOutage(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true, failLines: errors.New("clickhouse is down"), failUntil: time.Now().Add(300 * time.Millisecond)}
	ingester := newTestIngester(t, store, AsyncIngesterConfig{})
	stop := runIngester(t, ingester)
	defer stop()

	ingester.EnqueueAgentBatch("agent-1", testAgentBatch("svc-1", "alloc-1", 4))
	stats := waitForIngest(t, ingester, 4)
	if stats.ShedLines != 0 {
		t.Fatalf("outage shed lines: %+v", stats)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.flushes < 2 {
		t.Fatalf("expected retries during the outage, got %d flush attempts", store.flushes)
	}
	if len(store.lines) != 4 {
		t.Fatalf("expected 4 lines after recovery, got %d", len(store.lines))
	}
}

func TestAsyncIngesterIsNoOpWhenDisabled(t *testing.T) {
	t.Parallel()

	ingester := newTestIngester(t, &fakeFlushStore{}, AsyncIngesterConfig{})
	ingester.EnqueueAgentBatch("agent-1", testAgentBatch("svc-1", "alloc-1", 2))
	if stats := ingester.Stats(); stats.AcceptedLines != 0 {
		t.Fatalf("disabled ingester accepted lines: %+v", stats)
	}
	var nilIngester *AsyncIngester
	nilIngester.EnqueueAgentBatch("agent-1", testAgentBatch("svc-1", "alloc-1", 1))
	if stats := nilIngester.Stats(); stats.AcceptedLines != 0 {
		t.Fatalf("nil ingester accepted lines: %+v", stats)
	}
}

func TestAsyncIngesterRunBlocksUntilContextEndsWhenDisabled(t *testing.T) {
	t.Parallel()

	// A disabled ingester must not return early: hosting servers
	// treat any Run return as a shutdown signal.
	ingester := newTestIngester(t, &fakeFlushStore{}, AsyncIngesterConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ingester.Run(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("disabled Run returned early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("disabled Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disabled Run did not exit after cancel")
	}
}

// Past the detailed key cap, shed windows fold into service-level
// aggregate gaps instead of vanishing: reads must surface every shed
// line even when the overload explodes the distinct-key count.
func TestAsyncIngesterFoldsOwedGapsPastKeyCap(t *testing.T) {
	t.Parallel()

	// No Run loop: the journal fills and stays full.
	store := &fakeFlushStore{enabled: true}
	ingester := newTestIngester(t, store, AsyncIngesterConfig{QueueBytes: 512})

	services := []string{"svc-a", "svc-b", "svc-c"}
	totalKeys := 2 * maxIngestOwedGaps
	for i := 0; i < totalKeys; i++ {
		service := services[i%len(services)]
		ingester.EnqueueAgentBatch("agent-1", testAgentBatch(service, fmt.Sprintf("alloc-%05d", i), 2))
	}

	stats := ingester.Stats()
	if stats.ShedLines == 0 {
		t.Fatalf("tiny journal must shed: %+v", stats)
	}
	// Folding must bound the owed maps: past the detailed cap the
	// overflow collapses into a handful of service aggregates.
	if stats.OwedGaps > maxIngestOwedGaps+3 {
		t.Fatalf("owed %d gaps, want at most %d detailed + 3 folded", stats.OwedGaps, maxIngestOwedGaps)
	}

	stop := runIngester(t, ingester)
	defer stop()
	deadline := time.Now().Add(15 * time.Second)
	for {
		store.mu.Lock()
		flushed := len(store.gaps) > 0
		store.mu.Unlock()
		if flushed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owed gaps never flushed")
		}
		time.Sleep(10 * time.Millisecond)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	var total uint64
	for _, gap := range store.gaps {
		total += gap.DroppedCount
	}
	if total+stats.GapsLost != stats.ShedLines {
		t.Fatalf("flushed %d gap lines + %d lost != %d shed: folds must preserve every count",
			total, stats.GapsLost, stats.ShedLines)
	}
}

// Shutdown must not discard batches the Sync loop already accepted:
// Run drains the queued backlog and owed gap windows under a grace
// deadline even when its context is already canceled.
func TestAsyncIngesterDrainsAcceptedBacklogOnShutdown(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true}
	ingester := newTestIngester(t, store, AsyncIngesterConfig{QueueBytes: 3000, ShutdownGrace: 5 * time.Second})

	for i := 0; i < 5 && ingester.Stats().ShedLines == 0; i++ {
		ingester.EnqueueAgentBatch("agent-1", testAgentBatch("svc-1", fmt.Sprintf("alloc-%d", i), 3))
	}
	stats := ingester.Stats()
	if stats.ShedLines == 0 {
		t.Fatal("expected the journal budget to shed batches")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ingester.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	var gapLines uint64
	for _, gap := range store.gaps {
		gapLines += gap.DroppedCount
	}
	// Every accepted line and every shed line must reach the store:
	// kept lines as rows, shed lines as owed gap rows.
	if len(store.lines) != int(stats.AcceptedLines) || gapLines != stats.ShedLines {
		t.Fatalf("shutdown delivered %d of %d kept lines and %d of %d shed lines",
			len(store.lines), stats.AcceptedLines, gapLines, stats.ShedLines)
	}
	if len(store.gaps) == 0 {
		t.Fatal("shed batches must surface as gap rows at shutdown")
	}
}

// Shutdown must also keep the batch currently being retried: when the
// run context ends mid-outage, the dequeued flush and its attached
// gap windows are handed to the drain instead of vanishing with the
// canceled retry.
func TestAsyncIngesterDrainsInFlightRetryOnShutdown(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true, failLines: errors.New("clickhouse is down"), failUntil: time.Now().Add(250 * time.Millisecond)}
	ingester := newTestIngester(t, store, AsyncIngesterConfig{ShutdownGrace: 10 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ingester.Run(ctx) }()

	ingester.EnqueueAgentBatch("agent-1", testAgentBatch("svc-1", "alloc-1", 3))
	// Cancel only once the batch is dequeued and its first write has
	// failed: the flush is now in flight inside flushWithRetry.
	deadline := time.Now().Add(2 * time.Second)
	for {
		store.mu.Lock()
		attempts := store.flushes
		store.mu.Unlock()
		if attempts >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no write attempt for the queued batch")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not exit after cancel")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.lines) != 3 {
		t.Fatalf("shutdown dropped the in-flight batch: delivered %d of 3 lines", len(store.lines))
	}
}

// Once the shutdown drain finished, arrivals are rejected and loudly
// accounted — never silently accepted into a dead queue — so callers
// fall back to synchronous writes or producer replay.
func TestAsyncIngesterRejectsArrivalsAfterDrain(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true}
	ingester := newTestIngester(t, store, AsyncIngesterConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ingester.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if ingester.EnqueueAgentBatch("agent-1", testAgentBatch("svc-1", "alloc-1", 2)) {
		t.Fatal("batch admitted after the shutdown drain")
	}
	if ingester.EnqueueLines([]LogLineInput{{ID: "sy:1"}}) {
		t.Fatal("platform line admitted after the shutdown drain")
	}
	stats := ingester.Stats()
	if stats.AcceptedLines != 0 {
		t.Fatalf("rejected lines counted as accepted: %+v", stats)
	}
	if stats.GapsLost != 3 {
		t.Fatalf("rejected lines lost without accounting: %+v", stats)
	}
}

// Producers racing the shutdown drain never lose accounting: every
// offered line is either admitted and flushed by the drain (as a
// line or an owed gap row) or rejected and counted — never silently
// dropped between the drain's last look and the seal.
func TestAsyncIngesterAccountsForEveryLineAcrossShutdown(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true}
	ingester := newTestIngester(t, store, AsyncIngesterConfig{
		RatePerSec:    1e9,
		Burst:         100000,
		ShutdownGrace: 10 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ingester.Run(ctx) }()

	const producers, batches = 8, 25
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < batches; i++ {
				ingester.EnqueueAgentBatch("agent-1", testAgentBatch("svc-1", "alloc-1", 1))
			}
		}()
	}
	time.Sleep(5 * time.Millisecond)
	cancel()
	wg.Wait()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	stats := ingester.Stats()
	offered := uint64(producers * batches)
	if stats.AcceptedLines+stats.GapsLost != offered {
		t.Fatalf("accounting lost lines: offered %d, %+v", offered, stats)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	var gapLines uint64
	for _, gap := range store.gaps {
		gapLines += gap.DroppedCount
	}
	if uint64(len(store.lines))+gapLines != stats.AcceptedLines {
		t.Fatalf("admitted lines did not all reach the store: stored %d lines + %d gap lines, %+v",
			len(store.lines), gapLines, stats)
	}
}

// The durable journal is the acceptance contract: batches queued
// while no flusher runs survive a control-plane restart and land
// after the next boot instead of vanishing past the replay window.
func TestAsyncIngesterJournalSurvivesRestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := &fakeFlushStore{enabled: true}
	ingester := newTestIngester(t, store, AsyncIngesterConfig{SpoolDir: dir})
	for i := 0; i < 6; i++ {
		ingester.EnqueueLines([]LogLineInput{{ID: fmt.Sprintf("sy:%d", i), Line: "kept"}})
	}
	// No flusher ran: the process "crashes" here. Acceptance already
	// happened, so the ack contract demands these lines survive.
	reopened := newTestIngester(t, store, AsyncIngesterConfig{SpoolDir: dir})
	for i := 0; i < 6; i++ {
		reopened.EnqueueLines([]LogLineInput{{ID: fmt.Sprintf("sy:late-%d", i), Line: "kept"}})
	}
	stop := runIngester(t, reopened)
	defer stop()
	waitForIngest(t, reopened, 12)

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.lines) != 12 {
		t.Fatalf("journal recovery lost accepted lines: %d of 12", len(store.lines))
	}
	_ = ingester
}

// When the shutdown grace expires during a backend outage, the
// accepted backlog stays journaled for the next boot: nothing is
// abandoned, it just waits for a healthy store.
func TestAsyncIngesterKeepsAbandonedBacklogForNextBoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := &fakeFlushStore{enabled: true, failLines: errors.New("clickhouse is down")}
	ingester := newTestIngester(t, store, AsyncIngesterConfig{
		SpoolDir:      dir,
		ShutdownGrace: 100 * time.Millisecond,
	})
	for i := 0; i < 3; i++ {
		lines := make([]LogLineInput, 2500)
		for j := range lines {
			lines[j] = LogLineInput{ID: fmt.Sprintf("sy:%d:%d", i, j)}
		}
		ingester.EnqueueLines(lines)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ingester.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stats := ingester.Stats()
	if stats.FlushedLines != 0 {
		t.Fatalf("outage must not report flushed lines: %+v", stats)
	}
	if stats.QueuedFlushes == 0 {
		t.Fatalf("abandoned backlog must stay journaled: %+v", stats)
	}

	// The store recovers; the next boot writes everything.
	store.mu.Lock()
	store.failLines = nil
	store.mu.Unlock()
	reopened := newTestIngester(t, store, AsyncIngesterConfig{SpoolDir: dir})
	stop := runIngester(t, reopened)
	defer stop()
	waitForIngest(t, reopened, 7500)
}

// A shed flush keeps producer gap identity: the owed row must carry
// the summary's stable ID so a replayed summary replaces the same
// gap row instead of double-counting the loss.
func TestAsyncIngesterShedProducerGapsKeepSummaryIdentity(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true}
	ingester := newTestIngester(t, store, AsyncIngesterConfig{})

	batch := testAgentBatch("svc-1", "alloc-2", 2)
	batch.Drops = []*platformv1.LogDropSummary{{
		ServiceId:    "svc-1",
		AllocationId: "alloc-2",
		LogType:      platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME,
		DroppedCount: 3,
		Reason:       logpipeline.ReasonRateLimited,
		WindowStart:  timestamppb.New(time.Now().UTC()),
		WindowEnd:    timestamppb.New(time.Now().UTC()),
		SummaryId:    "sid-9",
	}}
	if !ingester.EnqueueAgentBatch("agent-1", testAgentBatch("svc-1", "alloc-1", 1)) {
		t.Fatal("first batch must be admitted")
	}
	if !ingester.EnqueueAgentBatch("agent-1", batch) {
		t.Fatal("second batch must be accepted with shed gaps")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ingester.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	for _, gap := range store.gaps {
		if gap.SummaryID == "sid-9" {
			if gap.DroppedCount != 3 {
				t.Fatalf("shed producer gap lost counts: %+v", gap)
			}
			return
		}
	}
	t.Fatalf("shed producer gap lost its identity: %+v", store.gaps)
}

// The ingest guard surfaces its rate-limited lines as gaps itself, so
// it must drain the limiter's denied map: allocation churn cannot
// grow process memory without a bound.
func TestAsyncIngesterKeepsLimiterDeniedMapBounded(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true}
	ingester := newTestIngester(t, store, AsyncIngesterConfig{RatePerSec: 1, Burst: 1})
	for i := 0; i < 50; i++ {
		ingester.EnqueueAgentBatch("agent-1", testAgentBatch("svc-1", fmt.Sprintf("alloc-%d", i), 5))
	}
	if got := ingester.limiter.DrainDrops(); len(got) != 0 {
		t.Fatalf("ingest guard retained %d denied keys without a bound", len(got))
	}
}

// blockingFlushStore blocks the first write until released, then
// fails every write — a backend outage whose first attempt spans
// concurrent enqueues.
type blockingFlushStore struct {
	mu      sync.Mutex
	flushes int
	blocked chan struct{}
	release chan struct{}
	lines   []LogLineInput
	gaps    []GapInput
}

func (b *blockingFlushStore) Enabled() bool { return true }

func (b *blockingFlushStore) WriteLogLines(_ context.Context, inputs []LogLineInput) error {
	b.mu.Lock()
	b.flushes++
	first := b.flushes == 1
	b.mu.Unlock()
	if first {
		close(b.blocked)
		<-b.release
	}
	return errors.New("ingest down")
}

func (b *blockingFlushStore) WriteGaps(_ context.Context, gaps []GapInput) error {
	return errors.New("ingest down")
}

// When the shutdown grace expires during a backend outage, late
// arrivals are rejected with loud accounting and the journaled
// backlog stays queued for the next boot: nothing vanishes without a
// gap or a retained record.
func TestDrainShutdownRejectsLateArrivalsAndKeepsBacklog(t *testing.T) {
	store := &blockingFlushStore{blocked: make(chan struct{}), release: make(chan struct{})}
	a := newTestIngester(t, store, AsyncIngesterConfig{ShutdownGrace: 250 * time.Millisecond})
	// The first batch queues; the drain reads it and blocks in
	// the failing store.
	if !a.EnqueueAgentBatch("agent-1", testAgentBatch("svc-1", "alloc-1", 1)) {
		t.Fatal("expected the batch to queue")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.drainShutdown(context.Background())
	}()
	<-store.blocked
	// While the drain retries, arrivals after the seal are rejected
	// and accounted — never silently accepted and lost.
	if a.EnqueueAgentBatch("agent-1", testAgentBatch("svc-2", "alloc-2", 1)) {
		t.Fatal("batch accepted after the drain sealed")
	}
	if a.EnqueueAgentBatch("agent-1", testAgentBatch("svc-3", "alloc-3", 1)) {
		t.Fatal("batch accepted after the drain sealed")
	}
	close(store.release)
	<-done

	// The grace expired: the rejected arrivals surface as accounted
	// loss, and the unflushed backlog stays journaled for recovery.
	stats := a.Stats()
	if stats.GapsLost != 2 {
		t.Fatalf("post-seal arrivals must be accounted, got %+v", stats)
	}
	if stats.QueuedFlushes == 0 {
		t.Fatalf("unflushed backlog must stay journaled, got %+v", stats)
	}
}

func TestAsyncIngesterShedsByBytesPastQueueBudget(t *testing.T) {
	t.Parallel()

	// No Run loop: the journal budget binds on real retained bytes,
	// so maximum-size lines can never pile up in memory or on disk
	// past the configured bound.
	store := &fakeFlushStore{enabled: true}
	ingester := newTestIngester(t, store, AsyncIngesterConfig{QueueBytes: 4096})

	batch := &agentv1.LogBatch{AgentId: "agent-1"}
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		batch.Entries = append(batch.Entries, &agentv1.LogEntry{
			ObservedAt:    timestamppb.New(now),
			EnvironmentId: "env-1",
			AllocationId:  "alloc-1",
			ServiceId:     "svc-1",
			Stream:        "stdout",
			Sequence:      uint64(i + 1),
			Line:          strings.Repeat("x", 64<<10),
			LineId:        logpipeline.AgentLineID("agent-1", "boot-1", "alloc-1", "stdout", uint64(i+1)),
			LogType:       platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME,
		})
	}
	ingester.EnqueueAgentBatch("agent-1", batch)
	stats := ingester.Stats()
	if stats.QueuedBytes > 4096 {
		t.Fatalf("maximum-size lines retained past the budget: %+v", stats)
	}
	if stats.ShedLines != 3 {
		t.Fatalf("shed %d lines, want 3", stats.ShedLines)
	}
	// The shed loss is durable: its gap rows drain to the store and
	// reach reads even though the lines themselves were too big.
	stop := runIngester(t, ingester)
	defer stop()
	deadline := time.Now().Add(3 * time.Second)
	for {
		store.mu.Lock()
		var total uint64
		for _, gap := range store.gaps {
			total += gap.DroppedCount
		}
		store.mu.Unlock()
		if total == stats.ShedLines {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shed gaps never reached reads: %d of %d", total, stats.ShedLines)
		}
		time.Sleep(10 * time.Millisecond)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.gaps[0].DroppedCount != 3 || store.gaps[0].Reason != logpipeline.ReasonIngestOverflow {
		t.Fatalf("shed gap wrong: %+v", store.gaps)
	}
}

// The flusher merges a bounded journal round per store write, so
// writes stay bounded even when the backlog holds far more.
func TestAsyncIngesterWriteRoundsStayBounded(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true}
	ingester := newTestIngester(t, store, AsyncIngesterConfig{})
	one := testAgentBatch("svc-1", "alloc-1", 3)
	inputs, _ := convertAgentBatch("agent-1", one)
	for i := 0; i < 20; i++ {
		ingester.EnqueueLines(inputs)
	}
	stop := runIngester(t, ingester)
	defer stop()
	waitForIngest(t, ingester, 60)

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.biggest > ingestFlushRecords*3 {
		t.Fatalf("write round merged %d lines, want <= %d", store.biggest, ingestFlushRecords*3)
	}
	if len(store.lines) != 60 {
		t.Fatalf("stored %d of 60 lines", len(store.lines))
	}
}

func TestAsyncIngesterByteBudgetCountsAttributes(t *testing.T) {
	t.Parallel()

	// Retained attribute maps count against the byte budget: tiny
	// lines with fat attributes must not slip past the bound.
	store := &fakeFlushStore{enabled: true}
	ingester := newTestIngester(t, store, AsyncIngesterConfig{QueueBytes: 2000})

	batch := &agentv1.LogBatch{AgentId: "agent-1"}
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		batch.Entries = append(batch.Entries, &agentv1.LogEntry{
			ObservedAt:    timestamppb.New(now),
			EnvironmentId: "env-1",
			AllocationId:  "alloc-1",
			ServiceId:     "svc-1",
			Stream:        "stdout",
			Sequence:      uint64(i + 1),
			Line:          "x",
			LineId:        logpipeline.AgentLineID("agent-1", "boot-1", "alloc-1", "stdout", uint64(i+1)),
			Attributes:    map[string]string{"k": strings.Repeat("v", 1024)},
		})
	}
	if ingester.EnqueueAgentBatch("agent-1", batch) != true {
		t.Fatal("enqueue must accept-or-shed, never fail")
	}
	stats := ingester.Stats()
	if stats.QueuedBytes > 1024 {
		t.Fatalf("queued %d bytes despite oversized attributes", stats.QueuedBytes)
	}
	if stats.ShedLines != 3 {
		t.Fatalf("shed %d lines, want 3", stats.ShedLines)
	}
	stop := runIngester(t, ingester)
	defer stop()
	waitForIngest(t, ingester, 0)
	deadline := time.Now().Add(3 * time.Second)
	for {
		store.mu.Lock()
		done := len(store.gaps) > 0
		store.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shed gap never reached reads")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAsyncIngesterReplayedProducerGapsDoNotInflate(t *testing.T) {
	t.Parallel()

	// A producer re-reporting the same drop summary after a failed
	// send must not double-count its loss when the batch sheds.
	store := &fakeFlushStore{enabled: true}
	ingester := newTestIngester(t, store, AsyncIngesterConfig{})
	summary := func() *agentv1.LogBatch {
		return &agentv1.LogBatch{AgentId: "agent-1", Drops: []*platformv1.LogDropSummary{{
			ServiceId:    "svc-1",
			AllocationId: "alloc-1",
			DroppedCount: 5,
			Reason:       logpipeline.ReasonSpoolOverflow,
			WindowStart:  timestamppb.New(time.Now().UTC()),
			WindowEnd:    timestamppb.New(time.Now().UTC()),
			SummaryId:    "sum-1",
		}}}
	}
	ingester.EnqueueAgentBatch("agent-1", summary()) // queued
	ingester.EnqueueAgentBatch("agent-1", summary()) // shed -> owed
	ingester.EnqueueAgentBatch("agent-1", summary()) // replayed while shed

	stop := runIngester(t, ingester)
	defer stop()
	deadline := time.Now().Add(10 * time.Second)
	for ingester.Stats().FlushedLines < 0 || ingester.Stats().OwedGaps > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("owed gap never flushed: %+v", ingester.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, gap := range store.gaps {
		if gap.SummaryID == "sum-1" && gap.DroppedCount != 5 {
			t.Fatalf("replayed summary inflated to %d: %+v", gap.DroppedCount, store.gaps)
		}
	}
}

func TestAsyncIngesterLimitsGapRows(t *testing.T) {
	t.Parallel()

	// Gap rows are retained writes too: a producer spamming gap-only
	// batches past the per-allocation guard gets its reports owed
	// (kept visible, replay-safe) instead of unlimited ClickHouse
	// rows.
	store := &fakeFlushStore{enabled: true}
	ingester := newTestIngester(t, store, AsyncIngesterConfig{RatePerSec: 1, Burst: 1})
	summary := func(id string) *agentv1.LogBatch {
		return &agentv1.LogBatch{AgentId: "agent-1", Drops: []*platformv1.LogDropSummary{{
			ServiceId:    "svc-1",
			AllocationId: "alloc-1",
			DroppedCount: 2,
			Reason:       logpipeline.ReasonSpoolOverflow,
			WindowStart:  timestamppb.New(time.Now().UTC()),
			WindowEnd:    timestamppb.New(time.Now().UTC()),
			SummaryId:    id,
		}}}
	}
	ingester.EnqueueAgentBatch("agent-1", summary("sum-1"))
	ingester.EnqueueAgentBatch("agent-1", summary("sum-2"))
	ingester.EnqueueAgentBatch("agent-1", summary("sum-3"))
	stats := ingester.Stats()
	if stats.QueuedFlushes != 1 {
		t.Fatalf("queued %d flushes, want 1", stats.QueuedFlushes)
	}
	if stats.OwedGaps != 2 {
		t.Fatalf("owed %d gap reports, want 2", stats.OwedGaps)
	}
}

// A shed batch's loss accounting is durable: gap rows journaled at
// shed time survive a restart and reach reads instead of dying with
// the process that dropped the lines.
func TestAsyncIngesterShedGapsSurviveRestart(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true}
	spoolDir := t.TempDir()
	first := newTestIngester(t, store, AsyncIngesterConfig{SpoolDir: spoolDir, QueueBytes: 4096})

	batch := &agentv1.LogBatch{AgentId: "agent-1"}
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		batch.Entries = append(batch.Entries, &agentv1.LogEntry{
			ObservedAt:    timestamppb.New(now),
			EnvironmentId: "env-1",
			AllocationId:  "alloc-1",
			ServiceId:     "svc-1",
			Stream:        "stdout",
			Sequence:      uint64(i + 1),
			Line:          strings.Repeat("x", 64<<10),
			LineId:        logpipeline.AgentLineID("agent-1", "boot-1", "alloc-1", "stdout", uint64(i+1)),
		})
	}
	if !first.EnqueueAgentBatch("agent-1", batch) {
		t.Fatal("enqueue must accept-or-shed, never fail")
	}
	if first.Stats().ShedLines != 3 {
		t.Fatalf("expected the oversized batch to shed: %+v", first.Stats())
	}

	// Crash before any flush: the restart must surface the shed
	// lines as gap rows from the journal.
	second := newTestIngester(t, store, AsyncIngesterConfig{SpoolDir: spoolDir, QueueBytes: 4096})
	stop := runIngester(t, second)
	defer stop()
	deadline := time.Now().Add(3 * time.Second)
	for {
		store.mu.Lock()
		var total uint64
		for _, gap := range store.gaps {
			total += gap.DroppedCount
		}
		store.mu.Unlock()
		if total == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shed gaps lost across restart: %d of 3", total)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
