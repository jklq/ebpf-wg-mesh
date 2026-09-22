package logs

import (
	"context"
	"errors"
	"fmt"
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

func TestAsyncIngesterFlushesQueuedBatches(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true}
	ingester := NewAsyncIngester(store, AsyncIngesterConfig{QueueFlushes: 8})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ingester.Run(ctx) }()

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
	ingester := NewAsyncIngester(store, AsyncIngesterConfig{QueueFlushes: 8, RatePerSec: 1, Burst: 2})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ingester.Run(ctx) }()

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

func TestAsyncIngesterShedsWithOwedGapsPastQueueCap(t *testing.T) {
	t.Parallel()

	// No Run loop: the queue fills and stays full.
	store := &fakeFlushStore{enabled: true}
	ingester := NewAsyncIngester(store, AsyncIngesterConfig{QueueFlushes: 2})

	ingester.EnqueueAgentBatch("agent-1", testAgentBatch("svc-1", "alloc-1", 3))
	ingester.EnqueueAgentBatch("agent-1", testAgentBatch("svc-1", "alloc-1", 3))
	ingester.EnqueueAgentBatch("agent-1", testAgentBatch("svc-1", "alloc-1", 3))
	stats := ingester.Stats()
	if stats.QueuedFlushes != 2 {
		t.Fatalf("queued %d flushes, want 2", stats.QueuedFlushes)
	}
	if stats.ShedLines != 3 {
		t.Fatalf("shed %d lines, want 3", stats.ShedLines)
	}
	if stats.OwedGaps != 1 {
		t.Fatalf("owed %d gaps, want 1", stats.OwedGaps)
	}

	// Draining the queue flushes the owed gap alongside the lines.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ingester.Run(ctx) }()
	waitForIngest(t, ingester, 6)
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.gaps) != 1 || store.gaps[0].DroppedCount != 3 || store.gaps[0].Reason != logpipeline.ReasonIngestOverflow {
		t.Fatalf("owed gap not flushed: %+v", store.gaps)
	}
}

func TestAsyncIngesterRetriesAcrossBackendOutage(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true, failLines: errors.New("clickhouse is down"), failUntil: time.Now().Add(300 * time.Millisecond)}
	ingester := NewAsyncIngester(store, AsyncIngesterConfig{QueueFlushes: 8})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ingester.Run(ctx) }()

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

	ingester := NewAsyncIngester(&fakeFlushStore{}, AsyncIngesterConfig{})
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
	ingester := NewAsyncIngester(&fakeFlushStore{}, AsyncIngesterConfig{})
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

	// No Run loop: the queue fills and stays full.
	store := &fakeFlushStore{enabled: true}
	ingester := NewAsyncIngester(store, AsyncIngesterConfig{QueueFlushes: 1})

	services := []string{"svc-a", "svc-b", "svc-c"}
	foldKeys := maxIngestOwedGaps
	totalKeys := foldKeys + 50
	expectedFold := map[string]uint64{}
	for i := 0; i < totalKeys+1; i++ {
		service := services[i%len(services)]
		ingester.EnqueueAgentBatch("agent-1", testAgentBatch(service, fmt.Sprintf("alloc-%05d", i), 2))
		// i == 0 fills the queue; sheds start at i == 1, so the
		// (foldKeys+1)-th and later shed keys fold.
		if i > foldKeys {
			expectedFold[service] += 2
		}
	}

	stats := ingester.Stats()
	if stats.ShedLines != uint64(totalKeys*2) {
		t.Fatalf("shed %d lines, want %d", stats.ShedLines, totalKeys*2)
	}
	if stats.GapsLost != 0 {
		t.Fatalf("gaps lost = %d, want 0: overflow must fold into service aggregates", stats.GapsLost)
	}
	if stats.OwedGaps != foldKeys+len(expectedFold) {
		t.Fatalf("owed %d gaps, want %d detailed + %d folded", stats.OwedGaps, foldKeys, len(expectedFold))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ingester.Run(ctx) }()
	waitForIngest(t, ingester, 2)

	store.mu.Lock()
	defer store.mu.Unlock()
	var detailed, folded int
	var total uint64
	gotFold := map[string]uint64{}
	for _, gap := range store.gaps {
		total += gap.DroppedCount
		if gap.AllocationID == "" {
			folded++
			gotFold[gap.ServiceID] += gap.DroppedCount
			continue
		}
		detailed++
	}
	if total != uint64(totalKeys*2) {
		t.Fatalf("gap rows account for %d dropped lines, want %d", total, totalKeys*2)
	}
	if detailed != foldKeys {
		t.Fatalf("flushed %d detailed gap rows, want %d", detailed, foldKeys)
	}
	if folded != len(expectedFold) {
		t.Fatalf("flushed %d folded aggregate gap rows, want %d", folded, len(expectedFold))
	}
	for service, want := range expectedFold {
		if gotFold[service] != want {
			t.Fatalf("folded gap for %s covers %d lines, want %d", service, gotFold[service], want)
		}
	}
}

// Shutdown must not discard batches the Sync loop already accepted:
// Run drains the queued backlog and owed gap windows under a grace
// deadline even when its context is already canceled.
func TestAsyncIngesterDrainsAcceptedBacklogOnShutdown(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true}
	ingester := NewAsyncIngester(store, AsyncIngesterConfig{QueueFlushes: 1, ShutdownGrace: 5 * time.Second})

	ingester.EnqueueAgentBatch("agent-1", testAgentBatch("svc-1", "alloc-1", 3))
	for i := 0; i < 4; i++ {
		ingester.EnqueueAgentBatch("agent-1", testAgentBatch("svc-1", fmt.Sprintf("alloc-%d", i), 2))
	}
	stats := ingester.Stats()
	if stats.ShedLines == 0 {
		t.Fatal("expected queue overflow to shed batches")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ingester.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	var total uint64
	for _, in := range store.lines {
		total++
		_ = in
	}
	for _, gap := range store.gaps {
		total += gap.DroppedCount
	}
	// 3 accepted lines in the queued flush + 8 shed lines surfaced as
	// owed gaps must all reach the store.
	if total != 11 {
		t.Fatalf("shutdown delivered %d of 11 accepted lines (lines %d)", total, len(store.lines))
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
	ingester := NewAsyncIngester(store, AsyncIngesterConfig{QueueFlushes: 8, ShutdownGrace: 10 * time.Second})
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
	ingester := NewAsyncIngester(store, AsyncIngesterConfig{QueueFlushes: 8})
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
	ingester := NewAsyncIngester(store, AsyncIngesterConfig{
		QueueFlushes:  64,
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

// The shutdown seal must hand the drain every queued flush, not just
// the first: each was accepted by the Sync loop and cannot be
// abandoned without accounting.
func TestAsyncIngesterSealAdmissionReturnsEveryQueuedFlush(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true}
	ingester := NewAsyncIngester(store, AsyncIngesterConfig{QueueFlushes: 8})
	for i := 0; i < 3; i++ {
		ingester.EnqueueLines([]LogLineInput{{ID: fmt.Sprintf("sy:%d", i)}})
	}
	left := ingester.sealAdmission()
	if len(left) != 3 {
		t.Fatalf("seal returned %d of 3 queued flushes", len(left))
	}
	if ingester.EnqueueLines([]LogLineInput{{ID: "sy:late"}}) {
		t.Fatal("flush admitted after the shutdown seal")
	}
	stats := ingester.Stats()
	if stats.GapsLost != 1 {
		t.Fatalf("post-seal arrival lost without accounting: %+v", stats)
	}
}

// When the shutdown grace expires during a backend outage, every
// accepted batch still held — the in-flight write and every queued
// flush behind it — lands in the loud accounting instead of
// vanishing without a gap or count.
func TestAsyncIngesterAccountsAbandonedBacklogWhenGraceExpires(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true, failLines: errors.New("clickhouse is down")}
	ingester := NewAsyncIngester(store, AsyncIngesterConfig{
		QueueFlushes:  8,
		RatePerSec:    1e9,
		Burst:         100000,
		ShutdownGrace: 100 * time.Millisecond,
	})

	// Three batches past the coalesce cap: the first two merge into
	// the in-flight write, the third stays queued behind it.
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
	if stats.GapsLost != 7500 {
		t.Fatalf("shutdown abandoned backlog without accounting: %+v", stats)
	}
	if stats.FlushedLines != 0 {
		t.Fatalf("outage must not report flushed lines: %+v", stats)
	}
}

// A shed flush keeps producer gap identity: the owed row must carry
// the summary's stable ID so a replayed summary replaces the same
// gap row instead of double-counting the loss.
func TestAsyncIngesterShedProducerGapsKeepSummaryIdentity(t *testing.T) {
	t.Parallel()

	store := &fakeFlushStore{enabled: true}
	ingester := NewAsyncIngester(store, AsyncIngesterConfig{QueueFlushes: 1})

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
