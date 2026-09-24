package agent

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
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/logpipeline"

	"google.golang.org/protobuf/proto"
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
		var shipped int
		for _, batch := range sender.batches {
			shipped += len(batch.GetEntries())
		}
		sender.mu.Unlock()
		// The ship tick may fire between the appends; wait for both
		// lines instead of the first batch.
		if shipped >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for shipped lines")
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
		var drops, quiet, hot uint64
		for _, batch := range sender.batches {
			drops += batch.GetDroppedLines()
			for _, entry := range batch.GetEntries() {
				switch entry.GetAllocationId() {
				case "alloc-quiet":
					quiet++
				case "alloc-hot":
					hot++
				}
			}
		}
		sender.mu.Unlock()
		// Drop summaries and entries ride separate messages; wait
		// for everything under assertion, not just the drop report.
		if drops >= 8 && quiet >= 1 && hot >= 2 {
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

// A control-plane outage must not grow the pending drop summaries
// without bound: every failed flush cycle folds its drops into one
// coalesced summary per identity, and one successful send carries
// the whole accumulated accounting.
func TestLogShipperCoalescesPendingDropSummariesAcrossOutage(t *testing.T) {
	t.Parallel()

	shipper, err := newLogShipper("agent-1", logShipConfig{
		SpoolDir:      t.TempDir(),
		RatePerSec:    0.001,
		Burst:         1,
		BatchSize:     10,
		FlushInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newLogShipper: %v", err)
	}
	defer shipper.Close()
	sender := &recordingSender{fail: errors.New("control plane down")}
	shipper.Attach(sender.send)
	// Consume the single burst token so every line below is denied.
	if !shipper.limiter.Allow("alloc-1") {
		t.Fatal("expected one burst token")
	}
	base := time.Now().UTC()
	var seq uint64
	for round := 0; round < 20; round++ {
		for i := 0; i < 5; i++ {
			seq++
			shipper.AppendLog(testEntry("alloc-1", "svc-1", "flood", seq))
		}
		shipper.collectDrops(base.Add(time.Duration(round) * time.Second))
		shipper.flush() // Send fails; the summaries must fold back.
	}
	shipper.mu.Lock()
	summaries := shipper.pending.Summaries()
	shipper.mu.Unlock()
	if len(summaries) != 1 {
		t.Fatalf("pending drop summaries not coalesced: %d entries after 20 outage rounds", len(summaries))
	}
	summary := summaries[0]
	if summary.GetDroppedCount() != 100 || summary.GetReason() != logpipeline.ReasonRateLimited {
		t.Fatalf("coalesced summary wrong: %+v", summary)
	}
	if summary.GetAllocationId() != "alloc-1" || summary.GetServiceId() != "svc-1" {
		t.Fatalf("coalesced summary misattributed: %+v", summary)
	}
	if summary.GetWindowStart().AsTime().After(base) || summary.GetWindowEnd().AsTime().Before(base.Add(19*time.Second)) {
		t.Fatalf("coalesced window does not span the outage: %+v", summary)
	}

	// Once the outage ends, one batch carries the whole accounting.
	sender.mu.Lock()
	sender.fail = nil
	sender.mu.Unlock()
	shipper.flush()
	shipper.mu.Lock()
	left := shipper.pending.Summaries()
	shipper.mu.Unlock()
	if len(left) != 0 {
		t.Fatalf("sent summaries not cleared: %+v", left)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.batches) != 1 {
		t.Fatalf("expected 1 recovered batch, got %d", len(sender.batches))
	}
	if got := sender.batches[0].GetDroppedLines(); got != 100 {
		t.Fatalf("recovered batch dropped %d lines, want 100", got)
	}
}

func TestLogShipperCapsBatchBytes(t *testing.T) {
	t.Parallel()

	shipper, err := newLogShipper("agent-1", logShipConfig{
		SpoolDir:      t.TempDir(),
		RatePerSec:    100000,
		Burst:         100000,
		BatchSize:     100,
		FlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("newLogShipper: %v", err)
	}
	// A batch of permitted maximum-size lines must split at the wire
	// budget instead of exceeding the transport's receive limit.
	big := strings.Repeat("x", logpipeline.MaxLogLineBytes)
	for i := uint64(1); i <= 40; i++ {
		shipper.AppendLog(testEntry("alloc-1", "svc-1", big, i))
	}
	sender := &recordingSender{}
	shipper.Attach(sender.send)
	shipper.flush()

	sender.mu.Lock()
	batches := append([]*agentv1.LogBatch(nil), sender.batches...)
	sender.mu.Unlock()
	if len(batches) < 2 {
		t.Fatalf("40 max-size lines must split across batches, got %d", len(batches))
	}
	var total int
	for _, batch := range batches {
		var size int
		for _, entry := range batch.GetEntries() {
			size += proto.Size(entry)
			total++
		}
		if size > logpipeline.MaxBatchBytes {
			t.Fatalf("batch line payload %d exceeds the wire budget %d", size, logpipeline.MaxBatchBytes)
		}
		if proto.Size(batch) >= 4<<20 {
			t.Fatalf("batch message %d exceeds the gRPC receive limit", proto.Size(batch))
		}
	}
	if total != 40 {
		t.Fatalf("chunking lost lines: shipped %d of 40", total)
	}

	// Every chunk committed: nothing is re-sent.
	shipper.flush()
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.batches) != len(batches) {
		t.Fatalf("committed chunks re-sent: %d batches after reflush", len(sender.batches))
	}
}

func TestLogShipperPersistsDropsWhileDetached(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	shipper, err := newLogShipper("agent-1", logShipConfig{
		SpoolDir:      dir,
		RatePerSec:    1,
		Burst:         2,
		BatchSize:     10,
		FlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("newLogShipper: %v", err)
	}
	for i := uint64(1); i <= 10; i++ {
		shipper.AppendLog(testEntry("alloc-1", "svc-1", "flood", i))
	}
	// Never attached and never closed: a flush while detached must
	// still persist the rate-limited losses, so a crash cannot lose
	// them without a gap.
	shipper.flush()

	drops, err := logpipeline.LoadDrops(dir)
	if err != nil {
		t.Fatalf("LoadDrops after detached flush: %v", err)
	}
	var dropped uint64
	for _, drop := range drops {
		dropped += drop.GetDroppedCount()
	}
	if dropped != 8 {
		t.Fatalf("detached flush persisted %d dropped lines, want 8", dropped)
	}
}

func TestLogShipperAttachRewindSurvivesInflightFlush(t *testing.T) {
	t.Parallel()

	shipper, err := newLogShipper("agent-1", logShipConfig{
		SpoolDir:      t.TempDir(),
		RatePerSec:    100000,
		Burst:         100000,
		BatchSize:     10,
		FlushInterval: time.Hour,
		ReplayWindow:  time.Hour,
	})
	if err != nil {
		t.Fatalf("newLogShipper: %v", err)
	}
	for i := uint64(1); i <= 10; i++ {
		shipper.AppendLog(testEntry("alloc-1", "svc-1", "line", i))
	}
	first := &recordingSender{}
	shipper.Attach(first.send)
	shipper.flush() // Acknowledged: cursor committed past recs 1..10.
	shipper.Detach()
	shipper.AppendLog(testEntry("alloc-1", "svc-1", "line", 11))

	// A flush holding a batch read from the old cursor races the
	// reconnect: its commit must not swallow Attach's replay rewind.
	var enteredOnce sync.Once
	entered := make(chan struct{})
	release := make(chan struct{})
	blocking := func(*agentv1.AgentClientMessage) error {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return nil
	}
	shipper.mu.Lock()
	shipper.send = blocking
	shipper.mu.Unlock()

	flushDone := make(chan struct{})
	go func() {
		shipper.flush()
		close(flushDone)
	}()
	<-entered

	replayed := &recordingSender{}
	attachDone := make(chan struct{})
	go func() {
		shipper.Attach(replayed.send)
		close(attachDone)
	}()
	time.Sleep(50 * time.Millisecond) // Let an unsynchronized Attach rewind mid-flight.
	close(release)
	<-flushDone
	<-attachDone

	shipper.flush()
	replayed.mu.Lock()
	defer replayed.mu.Unlock()
	for _, batch := range replayed.batches {
		for _, entry := range batch.GetEntries() {
			if entry.GetSequence() == 1 {
				return
			}
		}
	}
	t.Fatal("in-flight flush committed past the attach rewind; replay window swallowed")
}

func TestLogShipperSplitsOversizedDropSets(t *testing.T) {
	t.Parallel()

	shipper, err := newLogShipper("agent-1", logShipConfig{
		SpoolDir:      t.TempDir(),
		RatePerSec:    100000,
		Burst:         100000,
		BatchSize:     10,
		FlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("newLogShipper: %v", err)
	}
	// Allocation churn must not leave one durable summary per
	// allocation. The extra counts fold into the surviving identities
	// and still ship.
	now := time.Now().UTC()
	for i := 0; i < 30000; i++ {
		shipper.pending.Add(logpipeline.DropKey{
			ServiceID:    "svc-1",
			AllocationID: fmt.Sprintf("alloc-%d", i),
			LogType:      platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME,
			Stream:       "stdout",
			Reason:       logpipeline.ReasonRateLimited,
		}, 1, now, now)
	}
	sender := &recordingSender{}
	shipper.Attach(sender.send)
	shipper.flush()

	sender.mu.Lock()
	batches := append([]*agentv1.LogBatch(nil), sender.batches...)
	sender.mu.Unlock()
	var dropped uint64
	var identities int
	for _, batch := range batches {
		if proto.Size(batch) >= 4<<20 {
			t.Fatalf("drop message %d exceeds the transport limit", proto.Size(batch))
		}
		identities += len(batch.GetDrops())
		for _, drop := range batch.GetDrops() {
			dropped += drop.GetDroppedCount()
		}
	}
	if identities > maxAgentPendingDrops {
		t.Fatalf("shipped %d drop identities, cap is %d", identities, maxAgentPendingDrops)
	}
	if dropped != 30000 {
		t.Fatalf("bounded summaries lost drop accounting: %d of 30000", dropped)
	}

	// Everything was sent and committed: nothing re-sends.
	shipper.flush()
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.batches) != len(batches) {
		t.Fatalf("sent summaries re-sent: %d batches after reflush", len(sender.batches))
	}
}

func TestShipAdmitRateNeverExceedsDrain(t *testing.T) {
	t.Parallel()
	// The default shipper must drain at least its allowed input
	// rate, or sustained permitted traffic evicts lines even with a
	// healthy backend.
	drain := float64(defaultShipBatchSize) / defaultShipFlushInterval.Seconds()
	if drain < 200 {
		t.Fatalf("default drain %.0f/s is below the 200/s default admit rate", drain)
	}
	// A configured rate past what one tick drains clamps down to
	// the drain rate; non-positive stays unlimited (0).
	if got := shipAdmitRate(5000, 100, time.Second); got != 100 {
		t.Fatalf("admit %.0f, want the 100/s drain rate", got)
	}
	if got := shipAdmitRate(50, 100, time.Second); got != 50 {
		t.Fatalf("admit %.0f, want 50", got)
	}
	if got := shipAdmitRate(0, 100, time.Second); got != 0 {
		t.Fatalf("admit %.0f, want 0 (unlimited)", got)
	}
}

func TestLogShipperAggregateLimitCapsConcurrentAllocations(t *testing.T) {
	t.Parallel()

	shipper, err := newLogShipper("agent-1", logShipConfig{
		SpoolDir:      t.TempDir(),
		RatePerSec:    100000,
		Burst:         100,
		BatchSize:     4,
		FlushInterval: time.Second,
	})
	if err != nil {
		t.Fatalf("newLogShipper: %v", err)
	}
	defer shipper.Close()
	for i := uint64(1); i <= 50; i++ {
		shipper.AppendLog(testEntry("alloc-a", "svc-1", "a", i))
		shipper.AppendLog(testEntry("alloc-b", "svc-1", "b", i))
	}
	// Each allocation's own burst would admit all 50. Together they
	// stop at one flush batch.
	if got := shipper.Stats().Accepted; got != 4 {
		t.Fatalf("accepted %d lines, one flush drains 4", got)
	}
}

func TestLogShipperDropsAllocationMetadataAfterReporting(t *testing.T) {
	t.Parallel()

	shipper, err := newLogShipper("agent-1", logShipConfig{
		SpoolDir:      t.TempDir(),
		RatePerSec:    100000,
		Burst:         100000,
		BatchSize:     100,
		FlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("newLogShipper: %v", err)
	}
	defer shipper.Close()
	for i := 0; i < 40; i++ {
		shipper.AppendLog(testEntry(fmt.Sprintf("alloc-%d", i), "svc-1", "line", uint64(i+1)))
	}
	shipper.flush()
	shipper.mu.Lock()
	left := len(shipper.meta)
	shipper.mu.Unlock()
	if left != 0 {
		t.Fatalf("retained metadata for %d allocations with nothing pending", left)
	}
}

func TestLogShipperCloseWaitsForInflightDropSend(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	shipper, err := newLogShipper("agent-1", logShipConfig{
		SpoolDir:      dir,
		RatePerSec:    100000,
		Burst:         100,
		BatchSize:     10,
		FlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("newLogShipper: %v", err)
	}
	now := time.Now().UTC()
	shipper.pending.Add(logpipeline.DropKey{
		ServiceID:    "svc-1",
		AllocationID: "alloc-1",
		Reason:       logpipeline.ReasonRateLimited,
	}, 4, now, now)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	shipper.Attach(func(*agentv1.AgentClientMessage) error {
		once.Do(func() { close(entered) })
		<-release
		return errors.New("send failed")
	})
	go shipper.flush()
	<-entered

	closed := make(chan error, 1)
	go func() {
		closed <- shipper.Close()
	}()
	select {
	case err := <-closed:
		t.Fatalf("Close finished while the send still held the summaries: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not finish after the send returned")
	}
	drops, err := logpipeline.LoadDrops(dir)
	if err != nil {
		t.Fatalf("LoadDrops: %v", err)
	}
	var dropped uint64
	for _, drop := range drops {
		dropped += drop.GetDroppedCount()
	}
	if dropped != 4 {
		t.Fatalf("in-flight summaries persisted as %d, want 4", dropped)
	}
}
