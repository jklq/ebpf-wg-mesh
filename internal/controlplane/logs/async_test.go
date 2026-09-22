package logs

import (
	"context"
	"errors"
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
