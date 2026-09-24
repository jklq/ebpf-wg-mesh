package builder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/logpipeline"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func testBuildLogShipConfig(t *testing.T, buildID string) (buildLogShipConfig, string) {
	t.Helper()
	dir := buildLogSpoolDir(t.TempDir(), buildID, 1)
	return buildLogShipConfig{
		SpoolDir:      dir,
		SpoolMaxBytes: 1 << 20,
		RatePerSec:    100000,
		Burst:         100000,
		BatchSize:     10,
		FlushInterval: 10 * time.Millisecond,
		CloseTimeout:  5 * time.Second,
	}, dir
}

func mustBuildLogReporter(t *testing.T, client platformv1.BuilderServiceClient, epoch int64, cfg buildLogShipConfig) *buildLogReporter {
	t.Helper()
	reporter, err := newBuildLogReporter(context.Background(), client, "builder-1", "build-1", "svc-1", epoch, cfg)
	if err != nil {
		t.Fatalf("new build log reporter: %v", err)
	}
	return reporter
}

func TestBuildLogReporterShipsBatchesWithStableIdentities(t *testing.T) {
	t.Parallel()

	client := &recordingBuilderServiceClient{calls: make(chan struct{}, 8)}
	cfg, spoolDir := testBuildLogShipConfig(t, "build-1")
	reporter := mustBuildLogReporter(t, client, 3, cfg)
	if reporter == nil {
		t.Fatal("expected reporter")
	}
	now := time.Now().UTC()
	for i := 0; i < 11; i++ {
		reporter.Report(context.Background(), commandOutputLine{
			ObservedAt: now.Add(time.Duration(i) * time.Millisecond),
			Stream:     "stdout",
			Line:       "line",
		})
	}
	waitForBuilderReportCall(t, client.calls)
	reporter.Close()

	requests := client.ReportRequests()
	var sequences []uint64
	seenIDs := make(map[string]struct{})
	for _, req := range requests {
		if req.GetBuildId() != "build-1" || req.GetLeaseEpoch() != 3 {
			t.Fatalf("request misattributed: %+v", req)
		}
		for _, line := range req.GetLines() {
			sequences = append(sequences, line.GetSequence())
			seenIDs[line.GetLineId()] = struct{}{}
		}
	}
	if len(sequences) != 11 {
		t.Fatalf("expected 11 shipped lines, got %d", len(sequences))
	}
	for i, sequence := range sequences {
		if want := uint64(i + 1); sequence != want {
			t.Fatalf("sequence %d = %d, want %d", i, sequence, want)
		}
	}
	if len(seenIDs) != 11 {
		t.Fatal("line identities are not unique per line")
	}
	if _, err := os.Stat(spoolDir); !os.IsNotExist(err) {
		t.Fatalf("clean completion must remove the attempt spool: %v", err)
	}
}

func TestBuildLogReporterFlushesRemainingLinesOnClose(t *testing.T) {
	t.Parallel()

	client := &recordingBuilderServiceClient{calls: make(chan struct{}, 4)}
	cfg, _ := testBuildLogShipConfig(t, "build-1")
	cfg.FlushInterval = time.Hour // Close must drain without waiting for a tick.
	reporter := mustBuildLogReporter(t, client, 1, cfg)
	reporter.Report(context.Background(), commandOutputLine{ObservedAt: time.Now().UTC(), Stream: "stdout", Line: "one"})
	reporter.Report(context.Background(), commandOutputLine{ObservedAt: time.Now().UTC(), Stream: "stderr", Line: "two"})
	reporter.Close()

	requests := client.ReportRequests()
	if len(requests) != 1 {
		t.Fatalf("expected 1 report request, got %d", len(requests))
	}
	if got := len(requests[0].GetLines()); got != 2 {
		t.Fatalf("expected 2 flushed lines, got %d", got)
	}
}

func TestBuildLogReporterRetriesAcrossOutage(t *testing.T) {
	t.Parallel()

	client := &recordingBuilderServiceClient{
		calls:         make(chan struct{}, 16),
		failRemaining: 3,
		failErr:       errors.New("boom"),
	}
	cfg, _ := testBuildLogShipConfig(t, "build-1")
	reporter := mustBuildLogReporter(t, client, 1, cfg)
	reporter.Report(context.Background(), commandOutputLine{ObservedAt: time.Now().UTC(), Stream: "stdout", Line: "durable"})
	reporter.Close()

	requests := client.ReportRequests()
	if len(requests) != 4 {
		t.Fatalf("expected 3 failures plus 1 success, got %d requests", len(requests))
	}
	// Every attempt carries the same stable identity, so the
	// retried reports deduplicate server-side.
	first := requests[0].GetLines()[0].GetLineId()
	if first == "" {
		t.Fatal("missing stable line identity")
	}
	for _, req := range requests[1:] {
		if got := req.GetLines()[0].GetLineId(); got != first {
			t.Fatalf("retry changed line identity: %q vs %q", got, first)
		}
	}
}

func TestBuildLogReporterOrphansOnLeaseLoss(t *testing.T) {
	t.Parallel()

	client := &recordingBuilderServiceClient{
		calls:     make(chan struct{}, 4),
		reportErr: status.Error(codes.PermissionDenied, "build lease lost"),
	}
	cfg, spoolDir := testBuildLogShipConfig(t, "build-1")
	reporter := mustBuildLogReporter(t, client, 1, cfg)
	reporter.Report(context.Background(), commandOutputLine{ObservedAt: time.Now().UTC(), Stream: "stdout", Line: "one"})
	reporter.Close()

	if !reporter.orphaned.Load() {
		t.Fatal("lease loss must orphan the reporter")
	}
	drops, err := logpipeline.LoadDrops(spoolDir)
	if err != nil {
		t.Fatalf("LoadDrops: %v", err)
	}
	var dropped uint64
	for _, drop := range drops {
		dropped += drop.GetDroppedCount()
	}
	if dropped == 0 {
		t.Fatal("orphaned attempt deleted its unshipped lines without a gap")
	}
	// Reports after orphaning are no-ops.
	reporter.Report(context.Background(), commandOutputLine{ObservedAt: time.Now().UTC(), Stream: "stdout", Line: "two"})
	if got := len(client.ReportRequests()); got != 1 {
		t.Fatalf("expected no further reports after orphaning, got %d", got)
	}
}

func TestBuildLogReporterPersistsRateLimitBeforeFlush(t *testing.T) {
	t.Parallel()

	client := &recordingBuilderServiceClient{calls: make(chan struct{}, 4)}
	cfg, dir := testBuildLogShipConfig(t, "build-1")
	cfg.RatePerSec = 1
	cfg.Burst = 1
	cfg.FlushInterval = time.Hour
	reporter := mustBuildLogReporter(t, client, 1, cfg)
	defer reporter.Close()
	now := time.Now().UTC()
	reporter.Report(context.Background(), commandOutputLine{ObservedAt: now, Stream: "stdout", Line: "kept"})
	reporter.Report(context.Background(), commandOutputLine{ObservedAt: now, Stream: "stdout", Line: "denied"})

	drops, err := logpipeline.LoadDrops(dir)
	if err != nil {
		t.Fatalf("LoadDrops: %v", err)
	}
	var dropped uint64
	for _, drop := range drops {
		dropped += drop.GetDroppedCount()
	}
	if dropped != 1 {
		t.Fatalf("rate-limit denial was not durable before flush, got %d", dropped)
	}
}

func TestBuildLogReporterRateLimitsWithGapSummaries(t *testing.T) {
	t.Parallel()

	client := &recordingBuilderServiceClient{calls: make(chan struct{}, 4)}
	cfg, _ := testBuildLogShipConfig(t, "build-1")
	cfg.RatePerSec = 1
	cfg.Burst = 1
	cfg.FlushInterval = time.Hour
	reporter := mustBuildLogReporter(t, client, 1, cfg)
	for i := 0; i < 5; i++ {
		reporter.Report(context.Background(), commandOutputLine{ObservedAt: time.Now().UTC(), Stream: "stdout", Line: "flood"})
	}
	reporter.Close()

	requests := client.ReportRequests()
	if len(requests) != 1 {
		t.Fatalf("expected 1 report request, got %d", len(requests))
	}
	if got := len(requests[0].GetLines()); got != 1 {
		t.Fatalf("expected burst of 1 shipped line, got %d", got)
	}
	if got := requests[0].GetDroppedLines(); got != 4 {
		t.Fatalf("expected 4 dropped lines, got %d", got)
	}
	drops := requests[0].GetDrops()
	if len(drops) != 1 || drops[0].GetDroppedCount() != 4 || drops[0].GetReason() != logpipeline.ReasonRateLimited {
		t.Fatalf("missing rate-limit summary: %+v", drops)
	}
	if drops[0].GetBuildId() != "build-1" || drops[0].GetServiceId() != "svc-1" {
		t.Fatalf("summary misattributed: %+v", drops[0])
	}
}

func TestBuildLogReporterTruncatesOversizedLines(t *testing.T) {
	t.Parallel()

	client := &recordingBuilderServiceClient{calls: make(chan struct{}, 4)}
	cfg, _ := testBuildLogShipConfig(t, "build-1")
	cfg.FlushInterval = time.Hour
	reporter := mustBuildLogReporter(t, client, 1, cfg)
	huge := make([]byte, logpipeline.MaxLogLineBytes+100)
	for i := range huge {
		huge[i] = 'y'
	}
	reporter.Report(context.Background(), commandOutputLine{ObservedAt: time.Now().UTC(), Stream: "stdout", Line: string(huge)})
	reporter.Close()

	requests := client.ReportRequests()
	if len(requests) != 1 || len(requests[0].GetLines()) != 1 {
		t.Fatalf("expected 1 shipped line, got %+v", requests)
	}
	line := requests[0].GetLines()[0]
	if len(line.GetLine()) != logpipeline.MaxLogLineBytes || !line.GetTruncated() {
		t.Fatalf("oversized line not truncated: len=%d truncated=%v", len(line.GetLine()), line.GetTruncated())
	}
}

func TestGCStaleBuildLogSpools(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	stale := filepath.Join(base, "old-build")
	fresh := filepath.Join(base, "new-build")
	active := filepath.Join(base, "long-build")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(active, 0o755); err != nil {
		t.Fatal(err)
	}
	ancient := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, ancient, ancient); err != nil {
		t.Fatal(err)
	}
	// A long-running attempt keeps writing to its segment file without
	// touching the directory entry: staleness must follow the newest
	// write, not the directory mtime.
	if err := os.WriteFile(filepath.Join(active, "seg-0000000000.log"), []byte("output"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(active, ancient, ancient); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := gcStaleBuildLogSpools(base, staleBuildLogSpoolMaxAge)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed %d, want 1", reclaimed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale spool not collected")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh spool wrongly collected: %v", err)
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatalf("actively written spool wrongly collected despite old directory mtime: %v", err)
	}
	if got, err := gcStaleBuildLogSpools(filepath.Join(base, "missing"), staleBuildLogSpoolMaxAge); err != nil || got != 0 {
		t.Fatalf("missing base must be a no-op: %d %v", got, err)
	}
}

// A crashed attempt leaves its spool behind. The same build
// reclaimed under a new lease epoch must start from a fresh spool
// instead of re-emitting the previous attempt's records under the
// new lease.
func TestBuildLogReporterDoesNotReclaimPreviousEpochSpool(t *testing.T) {
	t.Parallel()

	if buildLogSpoolDir("/base", "build-1", 1) == buildLogSpoolDir("/base", "build-1", 2) {
		t.Fatal("spool dir must be keyed by lease epoch")
	}
	base := t.TempDir()
	staleID := logpipeline.BuilderLineID("builder-1", "build-1", 1, 1)
	now := time.Now().UTC()
	payload, err := proto.Marshal(&platformv1.BuildLogLine{
		ObservedAt: timestamppb.New(now),
		Stream:     "stdout",
		Sequence:   1,
		Line:       "stale",
		LineId:     staleID,
	})
	if err != nil {
		t.Fatalf("marshal stale line: %v", err)
	}
	oldDir := buildLogSpoolDir(base, "build-1", 1)
	oldSpool, err := logpipeline.OpenSpool(logpipeline.SpoolConfig{Dir: oldDir, MaxBytes: 1 << 20, SyncWrites: true})
	if err != nil {
		t.Fatalf("open old spool: %v", err)
	}
	if err := oldSpool.Append("stdout", staleID, now, payload); err != nil {
		t.Fatalf("append old spool: %v", err)
	}
	if err := oldSpool.Close(); err != nil {
		t.Fatalf("close old spool: %v", err)
	}

	client := &recordingBuilderServiceClient{calls: make(chan struct{}, 4)}
	cfg, _ := testBuildLogShipConfig(t, "build-1")
	cfg.SpoolDir = buildLogSpoolDir(base, "build-1", 2)
	cfg.FlushInterval = time.Hour
	reporter := mustBuildLogReporter(t, client, 2, cfg)
	reporter.Report(context.Background(), commandOutputLine{ObservedAt: now, Stream: "stdout", Line: "fresh"})
	reporter.Close()

	requests := client.ReportRequests()
	if len(requests) != 1 {
		t.Fatalf("expected 1 report request, got %d", len(requests))
	}
	lines := requests[0].GetLines()
	if len(lines) != 1 {
		t.Fatalf("new lease re-emitted the previous attempt's output: %+v", lines)
	}
	if lines[0].GetLineId() == staleID {
		t.Fatalf("new lease reported a stale-epoch line: %q", lines[0].GetLineId())
	}
	// The crashed attempt's spool survives untouched for garbage
	// collection.
	if _, err := os.Stat(oldDir); err != nil {
		t.Fatalf("previous attempt spool must survive for GC: %v", err)
	}
}

func TestBuildLogShipConfigZeroRateDisablesLimiting(t *testing.T) {
	t.Parallel()

	app := &App{cfg: config.BuilderConfig{
		WorkDir: t.TempDir(),
		Logs:    config.BuilderLogShippingConfig{RatePerSec: 0, Burst: 1000},
	}}
	ship := app.buildLogShipConfig("build-1", 1)
	if ship.RatePerSec != 0 {
		t.Fatalf("zero rate must disable producer limiting, got %v", ship.RatePerSec)
	}
	limiter := logpipeline.NewLimiter(ship.RatePerSec, ship.Burst)
	for i := 0; i < 100; i++ {
		if !limiter.Allow("build-1") {
			t.Fatal("zero rate must allow every line")
		}
	}
}

// Close must report limiter and overflow drops even when every line
// was dropped and the spool holds no records: the drain loop flushes
// (collecting drop summaries) before ever consulting drained().
func TestBuildLogReporterCloseReportsDropsWithEmptySpool(t *testing.T) {
	t.Parallel()

	client := &recordingBuilderServiceClient{calls: make(chan struct{}, 8)}
	cfg, _ := testBuildLogShipConfig(t, "build-1")
	cfg.FlushInterval = time.Hour // only the close path may report
	cfg.RatePerSec = 0.001
	cfg.Burst = 1
	reporter := mustBuildLogReporter(t, client, 3, cfg)
	if reporter == nil {
		t.Fatal("expected reporter")
	}
	// Consume the single burst token so every line below is denied.
	if !reporter.limiter.Allow("build-1") {
		t.Fatal("expected one burst token")
	}
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		reporter.Report(context.Background(), commandOutputLine{
			ObservedAt: now.Add(time.Duration(i) * time.Millisecond),
			Stream:     "stdout",
			Line:       "line",
		})
	}
	reporter.Close()

	var dropped uint64
	for _, req := range client.ReportRequests() {
		for _, summary := range req.GetDrops() {
			if summary.GetReason() != logpipeline.ReasonRateLimited {
				t.Fatalf("unexpected drop reason %q", summary.GetReason())
			}
			if summary.GetBuildId() != "build-1" {
				t.Fatalf("drop summary misattributed: %+v", summary)
			}
			dropped += summary.GetDroppedCount()
		}
	}
	if dropped != 5 {
		t.Fatalf("close reported %d dropped lines, want 5", dropped)
	}
}

// A control-plane outage must not grow the pending drop summaries
// without bound: every failed flush cycle folds its drops into one
// coalesced summary per identity, and one successful report carries
// the whole accumulated accounting.
func TestBuildLogReporterCoalescesDropSummariesAcrossOutage(t *testing.T) {
	t.Parallel()

	client := &recordingBuilderServiceClient{
		calls:         make(chan struct{}, 64),
		failRemaining: 1000,
		failErr:       errors.New("control plane down"),
	}
	cfg, _ := testBuildLogShipConfig(t, "build-1")
	cfg.RatePerSec = 0.001
	cfg.Burst = 1
	cfg.FlushInterval = time.Hour // Only the manual flushes below report.
	reporter := mustBuildLogReporter(t, client, 1, cfg)
	// Consume the single burst token so every line below is denied.
	if !reporter.limiter.Allow("build-1") {
		t.Fatal("expected one burst token")
	}
	base := time.Now().UTC()
	for round := 0; round < 20; round++ {
		for i := 0; i < 5; i++ {
			reporter.Report(context.Background(), commandOutputLine{ObservedAt: base, Stream: "stdout", Line: "flood"})
		}
		reporter.collectDrops(base.Add(time.Duration(round) * time.Second))
		reporter.flush() // Report fails; the summaries must fold back.
	}
	reporter.mu.Lock()
	summaries := reporter.pending.Summaries()
	reporter.mu.Unlock()
	if len(summaries) != 1 {
		t.Fatalf("pending drop summaries not coalesced: %d entries after 20 outage rounds", len(summaries))
	}
	summary := summaries[0]
	if summary.GetDroppedCount() != 100 || summary.GetReason() != logpipeline.ReasonRateLimited {
		t.Fatalf("coalesced summary wrong: %+v", summary)
	}
	if summary.GetBuildId() != "build-1" || summary.GetServiceId() != "svc-1" {
		t.Fatalf("coalesced summary misattributed: %+v", summary)
	}
	// Denials are checkpointed at the line's observed time, so the
	// coalesced window is that time rather than the later flush.
	if got := summary.GetWindowStart().AsTime(); !got.Equal(base) {
		t.Fatalf("window start %v, want %v", got, base)
	}
	if got := summary.GetWindowEnd().AsTime(); !got.Equal(base) {
		t.Fatalf("window end %v, want %v", got, base)
	}

	// Once the outage ends, one report carries the whole accounting.
	client.mu.Lock()
	client.failRemaining = 0
	client.mu.Unlock()
	reporter.flush()
	reporter.mu.Lock()
	left := reporter.pending.Summaries()
	reporter.mu.Unlock()
	if len(left) != 0 {
		t.Fatalf("reported summaries not cleared: %+v", left)
	}
	reporter.Close()

	// Failed attempts re-report their restored summaries
	// (at-least-once), so only the final recovered request carries
	// the complete accounting exactly once.
	requests := client.ReportRequests()
	if len(requests) < 2 {
		t.Fatalf("expected outage attempts plus a recovery, got %d requests", len(requests))
	}
	if recovered := requests[len(requests)-1].GetDroppedLines(); recovered != 100 {
		t.Fatalf("recovered report carried %d dropped lines, want 100", recovered)
	}
}

func TestBuildLogReporterCloseReportsAbandonedDelivery(t *testing.T) {
	t.Parallel()

	client := &recordingBuilderServiceClient{
		calls:     make(chan struct{}, 4),
		reportErr: errors.New("ingest down"),
	}
	cfg, spoolDir := testBuildLogShipConfig(t, "build-1")
	cfg.CloseTimeout = 50 * time.Millisecond
	reporter := mustBuildLogReporter(t, client, 1, cfg)
	reporter.Report(context.Background(), commandOutputLine{ObservedAt: time.Now().UTC(), Stream: "stdout", Line: "one"})

	// The output is never accepted: completing the build anyway
	// would garbage-collect the transcript and lose it.
	if err := reporter.Close(); err == nil {
		t.Fatal("Close must report abandoned delivery")
	}
	if _, err := os.Stat(spoolDir); err != nil {
		t.Fatalf("abandoned spool must stay behind for garbage collection: %v", err)
	}
	if err := reporter.Close(); err == nil {
		t.Fatal("abandonment must persist across Close calls")
	}
	var nilReporter *buildLogReporter
	if err := nilReporter.Close(); err != nil {
		t.Fatalf("nil reporter close: %v", err)
	}
}

func TestNewBuildLogReporterRefusesBrokenSpoolDir(t *testing.T) {
	t.Parallel()

	client := &recordingBuilderServiceClient{calls: make(chan struct{}, 4)}
	cfg, _ := testBuildLogShipConfig(t, "build-1")
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	cfg.SpoolDir = filepath.Join(blocker, "spool") // parent is a file: open must fail
	reporter, err := newBuildLogReporter(context.Background(), client, "builder-1", "build-1", "svc-1", 1, cfg)
	if err == nil {
		t.Fatal("spool-open failure must propagate: a build cannot complete without its transcript path")
	}
	if reporter != nil {
		t.Fatal("no reporter without a spool")
	}
}

func TestBuildLogReporterInheritsPriorAttemptDropSummaries(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	now := time.Now().UTC()

	// Attempt 1 drops lines under a dead control plane and dies with
	// the accounting still pending.
	failing := &recordingBuilderServiceClient{
		calls:     make(chan struct{}, 8),
		reportErr: errors.New("ingest down"),
	}
	cfg1 := buildLogShipConfig{
		SpoolDir:      buildLogSpoolDir(base, "build-1", 1),
		SpoolMaxBytes: 1 << 20,
		RatePerSec:    1,
		Burst:         1,
		BatchSize:     10,
		FlushInterval: time.Hour,
		CloseTimeout:  50 * time.Millisecond,
	}
	reporter1, err := newBuildLogReporter(context.Background(), failing, "builder-1", "build-1", "svc-1", 1, cfg1)
	if err != nil {
		t.Fatalf("new build log reporter: %v", err)
	}
	for i := 0; i < 5; i++ {
		reporter1.Report(context.Background(), commandOutputLine{ObservedAt: now, Stream: "stdout", Line: "flood"})
	}
	if err := reporter1.Close(); err == nil {
		t.Fatal("expected abandoned delivery for attempt 1")
	}
	if _, err := os.Stat(filepath.Join(cfg1.SpoolDir, logpipeline.PendingDropsFile)); err != nil {
		t.Fatalf("drop summaries must persist beside the attempt spool: %v", err)
	}

	// Attempt 2 (new epoch) takes over the dead attempt's accounting:
	// it re-emits its own output but can never recreate the lines
	// attempt 1 dropped, so their gap must surface from here.
	cfg2 := cfg1
	cfg2.SpoolDir = buildLogSpoolDir(base, "build-1", 2)
	cfg2.RatePerSec = 100000
	cfg2.Burst = 100000
	cfg2.FlushInterval = 10 * time.Millisecond
	client := &recordingBuilderServiceClient{calls: make(chan struct{}, 8)}
	reporter2, err := newBuildLogReporter(context.Background(), client, "builder-1", "build-1", "svc-1", 2, cfg2)
	if err != nil {
		t.Fatalf("new build log reporter: %v", err)
	}
	defer reporter2.Close()
	reporter2.Report(context.Background(), commandOutputLine{ObservedAt: now, Stream: "stdout", Line: "rerun"})
	waitForBuilderReportCall(t, client.calls)

	requests := client.ReportRequests()
	var inherited uint64
	for _, drop := range requests[0].GetDrops() {
		inherited += drop.GetDroppedCount()
	}
	if inherited != 4 {
		t.Fatalf("expected the 4 dropped lines of attempt 1, got %d in %+v", inherited, requests[0].GetDrops())
	}
	if _, err := os.Stat(filepath.Join(cfg1.SpoolDir, logpipeline.PendingDropsFile)); !os.IsNotExist(err) {
		t.Fatalf("taken-over summaries must be consumed: %v", err)
	}
}

func TestBuildLogReporterCapsBatchBytes(t *testing.T) {
	t.Parallel()

	client := &recordingBuilderServiceClient{calls: make(chan struct{}, 64)}
	cfg, _ := testBuildLogShipConfig(t, "build-1")
	cfg.SpoolMaxBytes = 8 << 20
	cfg.BatchSize = 100
	cfg.FlushInterval = time.Hour // Close must drain without waiting for a tick.
	reporter := mustBuildLogReporter(t, client, 1, cfg)
	// A report of permitted maximum-size lines must split at the wire
	// budget instead of exceeding the transport's receive limit,
	// which would wedge delivery and fail the build.
	big := strings.Repeat("x", logpipeline.MaxLogLineBytes)
	for i := 0; i < 40; i++ {
		reporter.Report(context.Background(), commandOutputLine{ObservedAt: time.Now().UTC(), Stream: "stdout", Line: big})
	}
	reporter.Close()

	requests := client.ReportRequests()
	if len(requests) < 2 {
		t.Fatalf("40 max-size lines must split across requests, got %d", len(requests))
	}
	var total int
	for _, req := range requests {
		var size int
		for _, line := range req.GetLines() {
			size += proto.Size(line)
			total++
		}
		if size > logpipeline.MaxBatchBytes {
			t.Fatalf("request line payload %d exceeds the wire budget %d", size, logpipeline.MaxBatchBytes)
		}
	}
	if total != 40 {
		t.Fatalf("chunking lost lines: reported %d of 40", total)
	}
}

func TestBuildLogReporterKeepsInheritedDropsWhenSaveFails(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	now := time.Now().UTC()

	// A dead attempt's accounting persists beside its spool.
	prior := buildLogSpoolDir(base, "build-1", 1)
	if err := os.MkdirAll(prior, 0o700); err != nil {
		t.Fatalf("create prior attempt dir: %v", err)
	}
	if err := logpipeline.SaveDrops(prior, []*platformv1.LogDropSummary{{
		ServiceId:    "svc-1",
		BuildId:      "build-1",
		LogType:      platformv1.ServiceLogType_SERVICE_LOG_TYPE_BUILD,
		Stream:       "stdout",
		DroppedCount: 3,
		Reason:       logpipeline.ReasonRateLimited,
		WindowStart:  timestamppb.New(now),
		WindowEnd:    timestamppb.New(now),
	}}); err != nil {
		t.Fatalf("save prior drops: %v", err)
	}

	// The merged snapshot cannot commit this time: the summary path
	// is occupied by a directory, so SaveDrops' rename fails.
	own := buildLogSpoolDir(base, "build-1", 2)
	if err := os.MkdirAll(filepath.Join(own, logpipeline.PendingDropsFile), 0o700); err != nil {
		t.Fatalf("block pending drops file: %v", err)
	}
	cfg := buildLogShipConfig{
		SpoolDir:      own,
		SpoolMaxBytes: 1 << 20,
		RatePerSec:    100000,
		Burst:         100000,
		BatchSize:     10,
		FlushInterval: time.Hour,
		CloseTimeout:  5 * time.Second,
	}
	reporter, err := newBuildLogReporter(context.Background(), &recordingBuilderServiceClient{calls: make(chan struct{}, 8)}, "builder-1", "build-1", "svc-1", 2, cfg)
	if err != nil {
		t.Fatalf("new build log reporter: %v", err)
	}
	defer reporter.Close()

	// The takeover copy must survive until a merged snapshot is
	// durable: deleting it on a failed save loses the accounting
	// forever.
	if _, err := os.Stat(filepath.Join(prior, logpipeline.PendingDropsFile)); err != nil {
		t.Fatalf("inherited drop summaries removed before the merge was durable: %v", err)
	}
}
