package builder

import (
	"context"
	"errors"
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

// buildLogShipConfig bounds one build attempt's log reporter. Zero values select
// defaults, except RatePerSec: a non-positive rate disables producer limiting.
type buildLogShipConfig struct {
	SpoolDir      string
	SpoolMaxBytes int64
	RatePerSec    float64
	Burst         int
	BatchSize     int
	FlushInterval time.Duration
	CloseTimeout  time.Duration
}

// buildLogReporter ships one build attempt's output through a bounded disk-backed
// spool with retry. Stable line identities deduplicate retried reports
// server-side. The spool is removed on clean completion; leftovers are deleted
// by startup garbage collection, and a retried attempt re-emits its output.
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

	mu sync.Mutex
	// lineQueue feeds the spool writer goroutine so output readers never block on disk syncs.
	lineQueue   chan queuedBuildLine
	linesStop   chan struct{}
	writerDone  chan struct{}
	linesClosed bool
	pending     *logpipeline.DropSet
	overflow    map[string]uint64
}

func newBuildLogReporter(ctx context.Context, client platformv1.BuilderServiceClient, builderID, buildID, serviceID string, leaseEpoch int64, cfg buildLogShipConfig) (*buildLogReporter, error) {
	if client == nil || buildID == "" {
		return nil, errors.New("build log reporter requires a client and a build ID")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.SpoolDir == "" {
		return nil, errors.New("build log spool directory is required; refusing to run a build whose output cannot ship")
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
		return nil, fmt.Errorf("open build log spool: %w", err)
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
		lineQueue:     make(chan queuedBuildLine, buildLineQueueCap),
		linesStop:     make(chan struct{}),
		writerDone:    make(chan struct{}),
	}
	go reporter.writeLoop()
	// Take over dead attempts' drop summaries first: a retry re-emits its own output
	// but can never recreate lines the dead attempt dropped.
	consumed := loadAttemptDrops(filepath.Dir(cfg.SpoolDir), buildID, cfg.SpoolDir, reporter.pending)
	if err := reporter.persistPendingLocked(); err != nil {
		// The merge is not durable: keep the takeover copies as the surviving record.
		slog.Warn("persist inherited build log drops", "builder_id", builderID, "build_id", buildID, "error", err)
	} else {
		for _, dir := range consumed {
			_ = os.Remove(filepath.Join(dir, logpipeline.PendingDropsFile))
		}
	}
	go reporter.run()
	return reporter, nil
}

// loadAttemptDrops folds this attempt's and earlier attempts' persisted drop
// summaries into pending, returning the taken-over sibling directories.
func loadAttemptDrops(baseDir, buildID, ownDir string, pending *logpipeline.DropSet) []string {
	if rows, err := logpipeline.LoadDrops(ownDir); err != nil {
		slog.Warn("load pending build log drops", "build_id", buildID, "error", err)
	} else {
		pending.Restore(rows)
	}
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil
	}
	prefix := sanitizeBuildSpoolName(buildID) + "-e"
	var consumed []string
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		dir := filepath.Join(baseDir, entry.Name())
		if filepath.Clean(dir) == filepath.Clean(ownDir) {
			continue
		}
		rows, err := logpipeline.LoadDrops(dir)
		if err != nil {
			slog.Warn("load inherited build log drops", "build_id", buildID, "dir", dir, "error", err)
			continue
		}
		inherited := make([]*platformv1.LogDropSummary, 0, len(rows))
		for _, row := range rows {
			if row.GetBuildId() == buildID {
				inherited = append(inherited, row)
			}
		}
		if len(inherited) == 0 {
			continue
		}
		pending.Restore(inherited)
		consumed = append(consumed, dir)
	}
	return consumed
}

// persistPendingLocked snapshots the pending drop summaries next to the attempt
// spool so a dead attempt's accounting survives into the retry. Callers hold r.mu.
func (r *buildLogReporter) persistPendingLocked() error {
	if err := logpipeline.SaveDrops(r.spoolDir, r.pending.Summaries()); err != nil {
		slog.Warn("persist pending build log drops", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
		return err
	}
	return nil
}

// Report rate-limits and spools one build output line. It never blocks on the network.
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
		// Persist the denial now: a crash before the next flush would lose the count.
		denied := r.limiter.DrainDrops()
		observedAt := line.ObservedAt.UTC()
		if observedAt.IsZero() {
			observedAt = time.Now().UTC()
		}
		r.mu.Lock()
		for _, count := range denied {
			r.notePendingLocked(line.Stream, count, logpipeline.ReasonRateLimited, observedAt, observedAt)
		}
		_ = r.persistPendingLocked()
		r.mu.Unlock()
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
	if err := r.enqueueLine(line.Stream, observedAt, payload); err != nil {
		slog.Warn("spool build log line", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
		r.countOverflow(line.Stream, 1)
		return
	}
}

const buildLineQueueCap = 1024

type queuedBuildLine struct {
	stream     string
	observedAt time.Time
	payload    []byte
}

// enqueueLine hands one marshaled line to the spool writer. Report runs on the
// build's output reader, so a blocking per-line sync there would fill the child
// pipe and stall execution; a full queue drops into overflow accounting instead.
func (r *buildLogReporter) enqueueLine(stream string, observedAt time.Time, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.linesClosed {
		return fmt.Errorf("reporter closed")
	}
	select {
	case r.lineQueue <- queuedBuildLine{stream: stream, observedAt: observedAt, payload: payload}:
		return nil
	default:
		return fmt.Errorf("build line queue full")
	}
}

func (r *buildLogReporter) writeLoop() {
	defer close(r.writerDone)
	for {
		select {
		case queued := <-r.lineQueue:
			r.appendQueued(queued)
		case <-r.linesStop:
			for {
				select {
				case queued := <-r.lineQueue:
					r.appendQueued(queued)
				default:
					return
				}
			}
		}
	}
}

func (r *buildLogReporter) appendQueued(queued queuedBuildLine) {
	if err := r.spool.Append(queued.stream, "", queued.observedAt, queued.payload); err != nil {
		slog.Warn("spool build log line", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
		r.countOverflow(queued.stream, 1)
	}
}

func (r *buildLogReporter) countOverflow(stream string, count uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overflow[stream] += count
}

// Close stops shipping, makes a best-effort final flush, and removes the attempt
// spool when fully drained. The final flush always runs at least once — an
// attempt whose lines were all dropped still reports its drop summaries first.
//
// Close reports an error when the backend never accepted the output within the
// close timeout: a build must not complete with its transcript undelivered.
func (r *buildLogReporter) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.linesClosed = true
		r.mu.Unlock()
		close(r.linesStop)
		<-r.writerDone
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
	unshipped := int64(0)
	if r.spool != nil {
		unshipped = r.spool.Stats().PendingRecords
		_ = r.spool.Close()
		r.spool = nil
	}
	r.mu.Lock()
	summaries := int64(r.pending.Len())
	r.mu.Unlock()
	left := unshipped + summaries
	if r.orphaned.Load() {
		slog.Warn("build log reporter orphaned by lease loss; attempt output is incomplete",
			"builder_id", r.builderID, "build_id", r.buildID, "unshipped", left)
		r.persistOrphanGap(unshipped)
		r.removeSpoolSegments()
		return
	}
	if left != 0 {
		slog.Warn("build log spool left behind for garbage collection",
			"builder_id", r.builderID, "build_id", r.buildID, "unshipped", left)
		return
	}
	_ = os.RemoveAll(r.spoolDir)
}

// persistOrphanGap records unshipped spool lines as a gap and keeps the summary on
// disk for the next lease to inherit.
func (r *buildLogReporter) persistOrphanGap(unshipped int64) {
	if r.spool != nil {
		_ = r.spool.Close()
		r.spool = nil
	}
	if unshipped <= 0 {
		return
	}
	now := time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notePendingLocked("combined", uint64(unshipped), logpipeline.ReasonSpoolOverflow, now, now)
	_ = r.persistPendingLocked()
}

// removeSpoolSegments deletes the attempt's log bytes, leaving the pending-drops
// file for the next lease to inherit.
func (r *buildLogReporter) removeSpoolSegments() {
	entries, err := os.ReadDir(r.spoolDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Name() == logpipeline.PendingDropsFile {
			continue
		}
		_ = os.RemoveAll(filepath.Join(r.spoolDir, entry.Name()))
	}
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
	// Every request stays within the wire budget; a batch past the transport
	// receive limit would wedge delivery and fail the build.
	chunks := logpipeline.ChunkByBytes(lines, func(l *platformv1.BuildLogLine) int { return proto.Size(l) }, logpipeline.MaxBatchBytes)
	if len(chunks) == 0 {
		chunks = [][]*platformv1.BuildLogLine{nil}
	}
	for i, chunk := range chunks {
		req := &platformv1.ReportBuildLogsRequest{
			BuilderId:  r.builderID,
			BuildId:    r.buildID,
			Lines:      chunk,
			LeaseEpoch: r.leaseEpoch,
		}
		if i == 0 {
			req.DroppedLines = dropped
			req.Drops = taken
		}
		reportCtx, cancel := context.WithTimeout(r.ctx, r.reportTimeout)
		_, err = r.client.ReportBuildLogs(reportCtx, req)
		cancel()
		if err != nil {
			r.restorePending(taken)
			r.spool.Release()
			if status.Code(err) == codes.PermissionDenied {
				r.orphaned.Store(true)
				slog.Warn("build log reporter orphaned by lease loss", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
				return
			}
			slog.Warn("report build logs",
				"builder_id", r.builderID, "build_id", r.buildID, "line_count", len(chunk), "error", err)
			return
		}
	}
	if err := r.spool.Commit(cursor); err != nil {
		slog.Warn("commit build log spool", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
		r.restorePending(taken)
		return
	}
	r.mu.Lock()
	r.persistPendingLocked()
	r.mu.Unlock()
}

// restorePending merges unsent summaries back so a failed report keeps its accounting.
func (r *buildLogReporter) restorePending(taken []*platformv1.LogDropSummary) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending.Restore(taken)
	r.persistPendingLocked()
}

// collectDrops drains limiter, spool, and overflow counters into the pending gap
// summaries, coalescing by identity so an outage cannot grow the pending set.
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
	r.persistPendingLocked()
}

// notePendingLocked coalesces one drop window into the pending entry for its identity.
func (r *buildLogReporter) notePendingLocked(stream string, count uint64, reason string, windowStart, windowEnd time.Time) {
	r.pending.Add(logpipeline.DropKey{
		ServiceID: r.serviceID,
		BuildID:   r.buildID,
		LogType:   platformv1.ServiceLogType_SERVICE_LOG_TYPE_BUILD,
		Stream:    stream,
		Reason:    reason,
	}, count, windowStart, windowEnd)
}

// sanitizeBuildSpoolName maps a build ID onto a safe single path segment.
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

// buildLogSpoolDir returns the per-attempt spool directory, keyed by build ID and
// lease epoch so a reclaimed build starts from a fresh spool.
func buildLogSpoolDir(baseDir, buildID string, leaseEpoch int64) string {
	return filepath.Join(baseDir, sanitizeBuildSpoolName(buildID)+"-e"+strconv.FormatInt(leaseEpoch, 10))
}

// gcStaleBuildLogSpools deletes per-attempt spool directories with no writes in
// the last maxAge. Staleness follows the newest write inside the spool, not the
// directory entry: appends touch the active segment while the directory mtime
// stays old on a long-running attempt.
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

// spoolLastWrite reports the newest modification time across a spool directory
// and its files, or false when it cannot be inspected.
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
