package builder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/logpipeline"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func testBuildLogShipConfig(t *testing.T, buildID string) (buildLogShipConfig, string) {
	t.Helper()
	dir := buildLogSpoolDir(t.TempDir(), buildID)
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

func TestBuildLogReporterShipsBatchesWithStableIdentities(t *testing.T) {
	t.Parallel()

	client := &recordingBuilderServiceClient{calls: make(chan struct{}, 8)}
	cfg, spoolDir := testBuildLogShipConfig(t, "build-1")
	reporter := newBuildLogReporter(context.Background(), client, "builder-1", "build-1", "svc-1", 3, cfg)
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
	reporter := newBuildLogReporter(context.Background(), client, "builder-1", "build-1", "svc-1", 1, cfg)
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
	reporter := newBuildLogReporter(context.Background(), client, "builder-1", "build-1", "svc-1", 1, cfg)
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
	reporter := newBuildLogReporter(context.Background(), client, "builder-1", "build-1", "svc-1", 1, cfg)
	reporter.Report(context.Background(), commandOutputLine{ObservedAt: time.Now().UTC(), Stream: "stdout", Line: "one"})
	reporter.Close()

	if !reporter.orphaned.Load() {
		t.Fatal("lease loss must orphan the reporter")
	}
	if _, err := os.Stat(spoolDir); !os.IsNotExist(err) {
		t.Fatalf("orphaned attempt spool must be removed: %v", err)
	}
	// Reports after orphaning are no-ops.
	reporter.Report(context.Background(), commandOutputLine{ObservedAt: time.Now().UTC(), Stream: "stdout", Line: "two"})
	if got := len(client.ReportRequests()); got != 1 {
		t.Fatalf("expected no further reports after orphaning, got %d", got)
	}
}

func TestBuildLogReporterRateLimitsWithGapSummaries(t *testing.T) {
	t.Parallel()

	client := &recordingBuilderServiceClient{calls: make(chan struct{}, 4)}
	cfg, _ := testBuildLogShipConfig(t, "build-1")
	cfg.RatePerSec = 1
	cfg.Burst = 1
	cfg.FlushInterval = time.Hour
	reporter := newBuildLogReporter(context.Background(), client, "builder-1", "build-1", "svc-1", 1, cfg)
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
	reporter := newBuildLogReporter(context.Background(), client, "builder-1", "build-1", "svc-1", 1, cfg)
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
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	ancient := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, ancient, ancient); err != nil {
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
	if got, err := gcStaleBuildLogSpools(filepath.Join(base, "missing"), staleBuildLogSpoolMaxAge); err != nil || got != 0 {
		t.Fatalf("missing base must be a no-op: %d %v", got, err)
	}
}

func TestBuildLogShipConfigZeroRateDisablesLimiting(t *testing.T) {
	t.Parallel()

	app := &App{cfg: config.BuilderConfig{
		WorkDir: t.TempDir(),
		Logs:    config.BuilderLogShippingConfig{RatePerSec: 0, Burst: 1000},
	}}
	ship := app.buildLogShipConfig("build-1")
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
