package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/logpipeline"

	"google.golang.org/protobuf/types/known/timestamppb"
)

type recordingSender struct {
	mu      sync.Mutex
	batches []*agentv1.LogBatch
	fail    error
}

func (s *recordingSender) send(msg *agentv1.AgentClientMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	if batch := msg.GetLogBatch(); batch != nil {
		s.batches = append(s.batches, batch)
	}
	return nil
}

func testEntry(alloc, service, line string, seq uint64) *agentv1.LogEntry {
	return &agentv1.LogEntry{
		ObservedAt:    timestamppb.New(time.Now().UTC()),
		EnvironmentId: "env-1",
		ServiceId:     service,
		AllocationId:  alloc,
		Stream:        "stdout",
		Sequence:      seq,
		Line:          line,
		LineId:        logpipeline.AgentLineID("agent-1", "boot-1", alloc, "stdout", seq),
		LogType:       platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME,
	}
}

func TestLogShipperShipsAndCommits(t *testing.T) {
	t.Parallel()

	shipper, err := newLogShipper("agent-1", logShipConfig{
		SpoolDir:      t.TempDir(),
		RatePerSec:    10000,
		Burst:         10000,
		BatchSize:     10,
		FlushInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newLogShipper: %v", err)
	}
	defer shipper.Close()
	sender := &recordingSender{}
	shipper.Attach(sender.send)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = shipper.Run(ctx) }()

	shipper.AppendLog(testEntry("alloc-1", "svc-1", "hello", 1))
	shipper.AppendLog(testEntry("alloc-1", "svc-1", "world", 2))

	deadline := time.Now().Add(5 * time.Second)
	for {
		sender.mu.Lock()
		got := len(sender.batches)
		sender.mu.Unlock()
		if got > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for shipped batch")
		}
		time.Sleep(10 * time.Millisecond)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	var lines int
	for _, batch := range sender.batches {
		lines += len(batch.GetEntries())
	}
	if lines != 2 {
		t.Fatalf("expected 2 shipped lines, got %d", lines)
	}
	if stats := shipper.Stats(); stats.Unshipped != 0 {
		t.Fatalf("expected drained spool, unshipped=%d", stats.Unshipped)
	}
}

func TestLogShipperRetriesFailedSends(t *testing.T) {
	t.Parallel()

	shipper, err := newLogShipper("agent-1", logShipConfig{
		SpoolDir:      t.TempDir(),
		RatePerSec:    10000,
		Burst:         10000,
		BatchSize:     10,
		FlushInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newLogShipper: %v", err)
	}
	defer shipper.Close()
	sender := &recordingSender{fail: errors.New("boom")}
	shipper.Attach(sender.send)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = shipper.Run(ctx) }()

	shipper.AppendLog(testEntry("alloc-1", "svc-1", "durable", 1))
	time.Sleep(100 * time.Millisecond)
	if stats := shipper.Stats(); stats.Unshipped != 1 {
		t.Fatalf("failed send must retain the spool record, unshipped=%d", stats.Unshipped)
	}
	sender.mu.Lock()
	sender.fail = nil
	sender.mu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if stats := shipper.Stats(); stats.Unshipped == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for retry")
		}
		time.Sleep(10 * time.Millisecond)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.batches) != 1 || len(sender.batches[0].GetEntries()) != 1 {
		t.Fatalf("expected one retried batch, got %d", len(sender.batches))
	}
	if got := sender.batches[0].GetEntries()[0].GetLineId(); got == "" {
		t.Fatal("retried line lost its stable identity")
	}
}

func TestLogShipperSpoolsWhileDetached(t *testing.T) {
	t.Parallel()

	shipper, err := newLogShipper("agent-1", logShipConfig{
		SpoolDir:      t.TempDir(),
		RatePerSec:    10000,
		Burst:         10000,
		BatchSize:     10,
		FlushInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newLogShipper: %v", err)
	}
	defer shipper.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = shipper.Run(ctx) }()

	// No attach: output accumulates on disk instead of dropping.
	shipper.AppendLog(testEntry("alloc-1", "svc-1", "offline", 1))
	time.Sleep(50 * time.Millisecond)
	if stats := shipper.Stats(); stats.Unshipped != 1 {
		t.Fatalf("detached shipper must spool, unshipped=%d", stats.Unshipped)
	}
	sender := &recordingSender{}
	shipper.Attach(sender.send)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if stats := shipper.Stats(); stats.Unshipped == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for post-attach flush")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLogShipperRateLimitsPerAllocation(t *testing.T) {
	t.Parallel()

	shipper, err := newLogShipper("agent-1", logShipConfig{
		SpoolDir:      t.TempDir(),
		RatePerSec:    1,
		Burst:         2,
		BatchSize:     100,
		FlushInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newLogShipper: %v", err)
	}
	defer shipper.Close()
	sender := &recordingSender{}
	shipper.Attach(sender.send)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = shipper.Run(ctx) }()

	for i := uint64(1); i <= 10; i++ {
		shipper.AppendLog(testEntry("alloc-hot", "svc-1", "flood", i))
	}
	// A quiet allocation is unaffected.
	shipper.AppendLog(testEntry("alloc-quiet", "svc-1", "hello", 1))

	deadline := time.Now().Add(5 * time.Second)
	for {
		sender.mu.Lock()
		var drops uint64
		for _, batch := range sender.batches {
			drops += batch.GetDroppedLines()
		}
		sender.mu.Unlock()
		if drops >= 8 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for drop reports, got %d", drops)
		}
		time.Sleep(10 * time.Millisecond)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	var quiet, hot int
	var gapAlloc string
	var gapReason string
	for _, batch := range sender.batches {
		for _, entry := range batch.GetEntries() {
			switch entry.GetAllocationId() {
			case "alloc-quiet":
				quiet++
			case "alloc-hot":
				hot++
			}
		}
		for _, drop := range batch.GetDrops() {
			if drop.GetDroppedCount() > 0 {
				gapAlloc = drop.GetAllocationId()
				gapReason = drop.GetReason()
			}
		}
	}
	if quiet != 1 {
		t.Fatalf("quiet allocation lost lines: %d", quiet)
	}
	if hot != 2 {
		t.Fatalf("hot allocation shipped %d, want burst of 2", hot)
	}
	if gapAlloc != "alloc-hot" || gapReason != logpipeline.ReasonRateLimited {
		t.Fatalf("missing rate-limit gap: alloc=%q reason=%q", gapAlloc, gapReason)
	}
}

func TestLogShipperSurvivesRestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	shipper, err := newLogShipper("agent-1", logShipConfig{
		SpoolDir:      dir,
		RatePerSec:    10000,
		Burst:         10000,
		BatchSize:     10,
		FlushInterval: time.Hour, // Never flush; simulate a crash.
	})
	if err != nil {
		t.Fatalf("newLogShipper: %v", err)
	}
	shipper.AppendLog(testEntry("alloc-1", "svc-1", "before-crash", 1))
	if err := shipper.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := newLogShipper("agent-1", logShipConfig{
		SpoolDir:      dir,
		RatePerSec:    10000,
		Burst:         10000,
		BatchSize:     10,
		FlushInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	sender := &recordingSender{}
	reopened.Attach(sender.send)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = reopened.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		sender.mu.Lock()
		got := len(sender.batches)
		sender.mu.Unlock()
		if got > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("crashed line was not recovered from the spool")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLogShipperPersistsDropAccountingAcrossShutdown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	shipper, err := newLogShipper("agent-1", logShipConfig{
		SpoolDir:      dir,
		RatePerSec:    1,
		Burst:         2,
		BatchSize:     10,
		FlushInterval: time.Hour, // Never flush before shutdown.
	})
	if err != nil {
		t.Fatalf("newLogShipper: %v", err)
	}
	for i := uint64(1); i <= 10; i++ {
		shipper.AppendLog(testEntry("alloc-1", "svc-1", "flood", i))
	}
	// Shutdown without ever shipping: the rate-limited lines must
	// survive as durable pending drop summaries.
	if err := shipper.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := newLogShipper("agent-1", logShipConfig{
		SpoolDir:      dir,
		RatePerSec:    10000,
		Burst:         10000,
		BatchSize:     10,
		FlushInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	sender := &recordingSender{}
	reopened.Attach(sender.send)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = reopened.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		sender.mu.Lock()
		var dropped uint64
		for _, batch := range sender.batches {
			for _, drop := range batch.GetDrops() {
				dropped += drop.GetDroppedCount()
			}
		}
		sender.mu.Unlock()
		if dropped >= 8 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shutdown drop accounting lost, got %d dropped", dropped)
		}
		time.Sleep(10 * time.Millisecond)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	var reason, alloc string
	var dropped uint64
	for _, batch := range sender.batches {
		for _, drop := range batch.GetDrops() {
			reason, alloc = drop.GetReason(), drop.GetAllocationId()
			dropped += drop.GetDroppedCount()
		}
	}
	if reason != logpipeline.ReasonRateLimited || alloc != "alloc-1" || dropped != 8 {
		t.Fatalf("drop summary wrong: reason=%q alloc=%q dropped=%d", reason, alloc, dropped)
	}
}

func TestLogShipConfigFromAgentZeroRateDisablesLimiting(t *testing.T) {
	t.Parallel()

	ship := logShipConfigFromAgent(config.AgentConfig{
		Logs: config.AgentLogShippingConfig{RatePerSec: 0, Burst: 1000},
	})
	if ship.RatePerSec != 0 {
		t.Fatalf("zero rate must disable producer limiting, got %v", ship.RatePerSec)
	}
	limiter := logpipeline.NewLimiter(ship.RatePerSec, ship.Burst)
	for i := 0; i < 100; i++ {
		if !limiter.Allow("alloc-1") {
			t.Fatal("zero rate must allow every line")
		}
	}
}
