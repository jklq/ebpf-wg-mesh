package builder

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/logpipeline"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	defaultBuildLogBatchSize     = 100
	defaultBuildLogFlushInterval = time.Second
	defaultBuildLogCloseTimeout  = 10 * time.Second
	defaultBuildLogReportTimeout = 10 * time.Second
	defaultBuildLogSpoolMaxBytes = 64 << 20
	staleBuildLogSpoolMaxAge     = 24 * time.Hour
	buildLogFinalDrainInterval   = 200 * time.Millisecond
)

// buildLogShipConfig bounds one build attempt's log reporter. Zero
// values select defaults, except RatePerSec: a non-positive rate
// disables producer limiting.
type buildLogShipConfig struct {
	SpoolDir      string
	SpoolMaxBytes int64
	RatePerSec    float64
	Burst         int
	BatchSize     int
	FlushInterval time.Duration
	CloseTimeout  time.Duration
}

// buildLogReporter ships one build attempt's output through a
// bounded disk-backed spool with retry. Lines and drop summaries
// carry stable identities so retried reports deduplicate
// server-side. The spool directory is removed on clean completion;
// anything left behind (crash, lost lease, failed final flush) is
// deleted by startup garbage collection, and a retried attempt
// re-emits its own output from scratch.
type buildLogReporter struct {
	client     platformv1.BuilderServiceClient
	builderID  string
	buildID    string
	serviceID  string
	leaseEpoch int64

	spool    *logpipeline.Spool
	spoolDir string
	limiter  *logpipeline.Limiter

	sequence      atomic.Uint64
	batchSize     int
	flushInterval time.Duration
	closeTimeout  time.Duration
	reportTimeout time.Duration
	ctx           context.Context

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	orphaned  atomic.Bool
	abandoned atomic.Bool

	mu       sync.Mutex
	pending  *logpipeline.DropSet
	overflow map[string]uint64
}

func newBuildLogReporter(ctx context.Context, client platformv1.BuilderServiceClient, builderID, buildID, serviceID string, leaseEpoch int64, cfg buildLogShipConfig) *buildLogReporter {
	if client == nil || buildID == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.SpoolDir == "" {
		slog.Warn("build log spool directory is required; build output will not ship", "builder_id", builderID, "build_id", buildID)
		return nil
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBuildLogBatchSize
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = defaultBuildLogFlushInterval
	}
	if cfg.CloseTimeout <= 0 {
		cfg.CloseTimeout = defaultBuildLogCloseTimeout
	}
	spool, err := logpipeline.OpenSpool(logpipeline.SpoolConfig{
		Dir:        cfg.SpoolDir,
		MaxBytes:   cfg.SpoolMaxBytes,
		SyncWrites: true,
	})
	if err != nil {
		slog.Warn("open build log spool; build output will not ship", "builder_id", builderID, "build_id", buildID, "error", err)
		return nil
	}
	reporter := &buildLogReporter{
		client:        client,
		builderID:     builderID,
		buildID:       buildID,
		serviceID:     serviceID,
		leaseEpoch:    leaseEpoch,
		spool:         spool,
		spoolDir:      cfg.SpoolDir,
		limiter:       logpipeline.NewLimiter(cfg.RatePerSec, cfg.Burst),
		batchSize:     cfg.BatchSize,
		flushInterval: cfg.FlushInterval,
		closeTimeout:  cfg.CloseTimeout,
		reportTimeout: defaultBuildLogReportTimeout,
		ctx:           ctx,
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
		pending:       logpipeline.NewDropSet(),
		overflow:      make(map[string]uint64),
	}
	go reporter.run()
	return reporter
}

// Report rate-limits and spools one build output line. It never
// blocks on the network.
func (r *buildLogReporter) Report(ctx context.Context, line commandOutputLine) {
	if r == nil || r.orphaned.Load() {
		return
	}
	select {
	case <-ctx.Done():
		return
	default:
	}
	if !r.limiter.Allow(r.buildID) {
		// The limiter counts the denial; collectDrops reports it.
		return
	}
	text, truncated := logpipeline.TruncateLine(strings.TrimRight(line.Line, "\r\n"))
	observedAt := line.ObservedAt.UTC()
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	sequence := r.sequence.Add(1)
	payload, err := proto.Marshal(&platformv1.BuildLogLine{
		ObservedAt: timestamppb.New(observedAt),
		Stream:     line.Stream,
		Sequence:   sequence,
		Line:       text,
		Truncated:  truncated,
		LineId:     logpipeline.BuilderLineID(r.builderID, r.buildID, r.leaseEpoch, sequence),
	})
	if err != nil {
		slog.Warn("marshal build log line", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
		r.countOverflow(line.Stream, 1)
		return
	}
	if err := r.spool.Append(line.Stream, "", observedAt, payload); err != nil {
		slog.Warn("spool build log line", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
		r.countOverflow(line.Stream, 1)
		return
	}
}

func (r *buildLogReporter) countOverflow(stream string, count uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overflow[stream] += count
}

// Close stops shipping, makes a best-effort final flush, and removes
// the attempt spool when fully drained. The final flush always runs
// at least once — it collects limiter and overflow drops first — so
// an attempt whose lines were all dropped still reports its drop
// summaries before the spool directory goes away. Anything left
// behind is deleted by startup garbage collection.
//
// Close reports an error when the backend never accepted the
// attempt's output within the close timeout. A build must not
// complete successfully with its transcript undelivered: the
// abandoned spool is garbage-collected and the output would be lost,
// while a failed build is retried and re-emits its output from
// scratch.
func (r *buildLogReporter) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		close(r.stop)
		select {
		case <-r.done:
		case <-time.After(r.closeTimeout):
			r.abandoned.Store(true)
			return
		}
		r.cleanup()
	})
	if r.abandoned.Load() && !r.orphaned.Load() {
		return fmt.Errorf("build log delivery abandoned: attempt output for build %s was not accepted within %s", r.buildID, r.closeTimeout)
	}
	return nil
}

func (r *buildLogReporter) cleanup() {
	pending := int64(0)
	if r.spool != nil {
		pending = r.spool.Stats().PendingRecords
		_ = r.spool.Close()
	}
	r.mu.Lock()
	pending += int64(r.pending.Len())
	r.mu.Unlock()
	if r.orphaned.Load() {
		slog.Warn("build log reporter orphaned by lease loss; attempt output is incomplete",
			"builder_id", r.builderID, "build_id", r.buildID, "unshipped", pending)
		_ = os.RemoveAll(r.spoolDir)
		return
	}
	if pending != 0 {
		slog.Warn("build log spool left behind for garbage collection",
			"builder_id", r.builderID, "build_id", r.buildID, "unshipped", pending)
		return
	}
	_ = os.RemoveAll(r.spoolDir)
}

func (r *buildLogReporter) run() {
	defer func() {
		if r.abandoned.Load() && r.spool != nil {
			_ = r.spool.Close()
		}
		close(r.done)
	}()
	ticker := time.NewTicker(r.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			// Always flush at least once: flush collects limiter and
			// overflow drops first, so rate-limited or overflowed
			// attempts with an empty spool still report their drops
			// instead of draining away unreported.
			for !r.orphaned.Load() && !r.abandoned.Load() {
				r.flush()
				if r.drained() {
					return
				}
				time.Sleep(buildLogFinalDrainInterval)
			}
			return
		case <-ticker.C:
			r.flush()
		}
	}
}

func (r *buildLogReporter) drained() bool {
	if r.spool == nil {
		return true
	}
	if r.spool.Stats().PendingRecords > 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pending.Len() == 0
}

func (r *buildLogReporter) flush() {
	if r.orphaned.Load() {
		return
	}
	now := time.Now().UTC()
	r.collectDrops(now)
	records, cursor, err := r.spool.Read(r.batchSize)
	if err != nil {
		slog.Warn("read build log spool", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
		return
	}
	r.mu.Lock()
	taken := r.pending.Take()
	r.mu.Unlock()
	if len(records) == 0 && len(taken) == 0 {
		return
	}
	lines := make([]*platformv1.BuildLogLine, 0, len(records))
	for _, record := range records {
		var line platformv1.BuildLogLine
		if err := proto.Unmarshal(record.Payload, &line); err != nil {
			slog.Warn("decode spooled build log line", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
			r.countOverflow(record.Key, 1)
			continue
		}
		lines = append(lines, &line)
	}
	var dropped uint64
	for _, summary := range taken {
		dropped += summary.GetDroppedCount()
	}
	reportCtx, cancel := context.WithTimeout(r.ctx, r.reportTimeout)
	defer cancel()
	_, err = r.client.ReportBuildLogs(reportCtx, &platformv1.ReportBuildLogsRequest{
		BuilderId:    r.builderID,
		BuildId:      r.buildID,
		Lines:        lines,
		LeaseEpoch:   r.leaseEpoch,
		DroppedLines: dropped,
		Drops:        taken,
	})
	if err != nil {
		r.restorePending(taken)
		if status.Code(err) == codes.PermissionDenied {
			r.orphaned.Store(true)
			slog.Warn("build log reporter orphaned by lease loss", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
			return
		}
		slog.WarnContext(reportCtx, "report build logs",
			"builder_id", r.builderID, "build_id", r.buildID, "line_count", len(lines), "error", err)
		return
	}
	if err := r.spool.Commit(cursor); err != nil {
		slog.Warn("commit build log spool", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
		r.restorePending(taken)
		return
	}
}

// restorePending merges unsent summaries back into the pending set
// so a failed report keeps its accounting; coalescing folds them
// into newer windows for the same identity.
func (r *buildLogReporter) restorePending(taken []*platformv1.LogDropSummary) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending.Restore(taken)
}

// collectDrops drains limiter, spool, and overflow counters into the
// pending gap summaries reported with the next batch, coalescing by
// identity so repeated collections during an outage cannot grow the
// pending set.
func (r *buildLogReporter) collectDrops(now time.Time) {
	windowStart := now.Add(-r.flushInterval)
	limited := r.limiter.DrainDrops()
	evicted, corrupt := r.spool.DrainDrops()
	r.mu.Lock()
	overflow := r.overflow
	r.overflow = make(map[string]uint64)
	r.mu.Unlock()
	if len(limited) == 0 && len(evicted) == 0 && len(corrupt) == 0 && len(overflow) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, count := range limited {
		r.notePendingLocked("", count, logpipeline.ReasonRateLimited, windowStart, now)
	}
	for stream, count := range evicted {
		r.notePendingLocked(stream, count, logpipeline.ReasonSpoolOverflow, windowStart, now)
	}
	for stream, count := range corrupt {
		r.notePendingLocked(stream, count, logpipeline.ReasonCorruptSpool, windowStart, now)
	}
	for stream, count := range overflow {
		r.notePendingLocked(stream, count, logpipeline.ReasonSpoolOverflow, windowStart, now)
	}
}

// notePendingLocked coalesces one drop window into the pending entry
// for its identity: one entry per (stream, reason) however long the
// outage lasts.
func (r *buildLogReporter) notePendingLocked(stream string, count uint64, reason string, windowStart, windowEnd time.Time) {
	r.pending.Add(logpipeline.DropKey{
		ServiceID: r.serviceID,
		BuildID:   r.buildID,
		LogType:   platformv1.ServiceLogType_SERVICE_LOG_TYPE_BUILD,
		Stream:    stream,
		Reason:    reason,
	}, count, windowStart, windowEnd)
}

// sanitizeBuildSpoolName maps a build ID onto a safe single path
// segment. UUID build IDs pass through unchanged.
func sanitizeBuildSpoolName(buildID string) string {
	var out strings.Builder
	for _, r := range strings.TrimSpace(buildID) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out.WriteRune(r)
		default:
			out.WriteRune('_')
		}
	}
	name := out.String()
	if len(name) > 128 {
		name = name[:128]
	}
	if name == "" || name == "." || name == ".." {
		name = "build"
	}
	return name
}

// buildLogSpoolDir returns the per-attempt spool directory for one
// build attempt, keyed by build ID and lease epoch: a build reclaimed
// under a new lease starts from a fresh spool instead of re-opening
// the previous attempt's records.
func buildLogSpoolDir(baseDir, buildID string, leaseEpoch int64) string {
	return filepath.Join(baseDir, sanitizeBuildSpoolName(buildID)+"-e"+strconv.FormatInt(leaseEpoch, 10))
}

// gcStaleBuildLogSpools deletes per-attempt spool directories with
// no writes in the last maxAge. Attempts are short-lived and a
// retried build re-emits its own output, so leftovers are always
// safe to delete. Staleness follows the newest write inside the
// spool, not the directory entry: appends touch the active segment
// file while a long-running attempt's directory mtime stays old.
func gcStaleBuildLogSpools(baseDir string, maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	reclaimed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(baseDir, entry.Name())
		lastWrite, ok := spoolLastWrite(path)
		if !ok || lastWrite.After(cutoff) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return reclaimed, err
		}
		reclaimed++
	}
	return reclaimed, nil
}

// spoolLastWrite reports the newest modification time across a spool
// directory and its files. It reports false when the directory
// cannot be inspected, so callers leave it alone.
func spoolLastWrite(dir string) (time.Time, bool) {
	info, err := os.Stat(dir)
	if err != nil {
		return time.Time{}, false
	}
	newest := info.ModTime()
	files, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, false
	}
	for _, file := range files {
		info, err := file.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest, true
}
