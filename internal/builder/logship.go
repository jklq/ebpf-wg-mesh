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
	closeErr  error
	orphaned  atomic.Bool
	abandoned atomic.Bool

	mu sync.Mutex
	// lineQueue feeds the spool writer goroutine so output readers never block on disk syncs.
	lineQueue   chan queuedBuildLine
	linesStop   chan struct{}
	writerDone  chan struct{}
	linesClosed bool
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
		Dir:      cfg.SpoolDir,
		MaxBytes: cfg.SpoolMaxBytes,
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
		lineQueue:     make(chan queuedBuildLine, buildLineQueueCap),
		linesStop:     make(chan struct{}),
		writerDone:    make(chan struct{}),
	}
	// A retry inherits loss accounting with stable identities before it starts
	// producing output. Source snapshots stay intact until the import commits.
	if err := reporter.inheritAttemptDrops(); err != nil {
		_ = spool.Close()
		return nil, fmt.Errorf("inherit dead build attempt logs: %w", err)
	}
	go reporter.writeLoop()
	go reporter.run()
	return reporter, nil
}

func (r *buildLogReporter) inheritAttemptDrops() error {
	base := filepath.Dir(r.spoolDir)
	entries, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	prefix := sanitizeBuildSpoolName(r.buildID) + "-e"
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		epoch, err := strconv.ParseInt(strings.TrimPrefix(entry.Name(), prefix), 10, 64)
		if err != nil || epoch >= r.leaseEpoch {
			continue
		}
		dir := filepath.Join(base, entry.Name())
		if filepath.Clean(dir) == filepath.Clean(r.spoolDir) {
			continue
		}
		previous, err := logpipeline.OpenSpool(logpipeline.SpoolConfig{Dir: dir, MaxBytes: defaultBuildLogSpoolMaxBytes})
		if err != nil {
			return err
		}
		err = func() error {
			if err := previous.Abandon(); err != nil {
				return err
			}
			rows, err := previous.PendingDrops()
			if err != nil {
				return err
			}
			for _, row := range rows {
				if row.GetBuildId() != r.buildID {
					return errors.New("dead build attempt contains another build's log losses")
				}
			}
			if err := r.spool.MergeDrops(rows); err != nil {
				return err
			}
			return previous.AcknowledgeDrops(rows)
		}()
		closeErr := previous.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
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
		r.limiter.DrainDrops()
		r.noteDrop(line.Stream, 1, logpipeline.ReasonRateLimited, line.ObservedAt)
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
		r.noteDrop(line.Stream, 1, logpipeline.ReasonSpoolOverflow, observedAt)
		return
	}
	if err := r.enqueueLine(line.Stream, observedAt, payload); err != nil {
		slog.Warn("spool build log line", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
		r.noteDrop(line.Stream, 1, logpipeline.ReasonSpoolOverflow, observedAt)
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
	if err := r.spool.Append(logpipeline.Record{DropKey: r.dropKey(queued.stream, ""), ObservedAt: queued.observedAt, Payload: queued.payload}); err != nil {
		slog.Warn("spool build log line", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
		r.noteDrop(queued.stream, 1, logpipeline.ReasonSpoolOverflow, queued.observedAt)
	}
}

func (r *buildLogReporter) dropKey(stream, reason string) logpipeline.DropKey {
	return logpipeline.DropKey{ServiceID: r.serviceID, BuildID: r.buildID, LogType: platformv1.ServiceLogType_SERVICE_LOG_TYPE_BUILD, Stream: stream, Reason: reason}
}

func (r *buildLogReporter) noteDrop(stream string, count uint64, reason string, observedAt time.Time) {
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	if err := r.spool.AddDrops(logpipeline.Drop{Key: r.dropKey(stream, reason), Count: count, Start: observedAt, End: observedAt}); err != nil {
		r.abandoned.Store(true)
		slog.Error("persist build log loss", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
	}
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
		r.closeErr = r.cleanup()
	})
	if r.abandoned.Load() && !r.orphaned.Load() {
		return fmt.Errorf("build log delivery abandoned: attempt output for build %s was not accepted within %s", r.buildID, r.closeTimeout)
	}
	return r.closeErr
}

func (r *buildLogReporter) cleanup() error {
	if r.spool == nil || r.abandoned.Load() || r.orphaned.Load() {
		return nil
	}
	rows, err := r.spool.PendingDrops()
	stats, statsErr := r.spool.Stats()
	if err != nil || statsErr != nil || stats.PendingRecords > 0 || len(rows) > 0 {
		slog.Warn("build log spool retained for the next attempt", "builder_id", r.builderID, "build_id", r.buildID, "unshipped", stats.PendingRecords, "error", errors.Join(err, statsErr))
		return errors.Join(err, statsErr, r.spool.Close())
	}
	if err := r.spool.Close(); err != nil {
		return err
	}
	return os.RemoveAll(r.spoolDir)
}

func (r *buildLogReporter) run() {
	defer func() {
		if r.abandoned.Load() || r.orphaned.Load() {
			r.spool.Release()
			if err := r.spool.Abandon(); err != nil {
				slog.Error("persist abandoned build transcript gaps", "build_id", r.buildID, "error", err)
			}
			if err := r.spool.Close(); err != nil {
				slog.Error("close abandoned build log spool", "build_id", r.buildID, "error", err)
			}
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
	stats, err := r.spool.Stats()
	if err != nil || stats.PendingRecords > 0 {
		return false
	}
	drops, err := r.spool.PendingDrops()
	return err == nil && len(drops) == 0
}

func (r *buildLogReporter) flush() {
	if r.orphaned.Load() {
		return
	}
	r.limiter.DrainDrops()
	records, cursor, err := r.spool.Read(r.batchSize)
	if err != nil {
		slog.Warn("read build log spool", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
		return
	}
	taken, err := r.spool.PendingDrops()
	if err != nil {
		r.spool.Release()
		slog.Error("read build log losses", "build_id", r.buildID, "error", err)
		return
	}
	if len(records) == 0 && len(taken) == 0 {
		r.spool.Release()
		return
	}
	lines := make([]*platformv1.BuildLogLine, 0, len(records))
	for _, record := range records {
		var line platformv1.BuildLogLine
		if err := proto.Unmarshal(record.Payload, &line); err != nil {
			slog.Warn("decode spooled build log line", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
			if err := r.spool.Discard(record); err != nil {
				r.spool.Release()
				r.abandoned.Store(true)
				slog.Error("record corrupt build log loss", "error", err)
				return
			}
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
	lineChunks := logpipeline.ChunkByBytes(lines, func(l *platformv1.BuildLogLine) int { return proto.Size(l) }, logpipeline.MaxBatchBytes)
	dropChunks := logpipeline.ChunkByBytes(taken, func(d *platformv1.LogDropSummary) int { return proto.Size(d) }, logpipeline.MaxBatchBytes)
	requests := make([]*platformv1.ReportBuildLogsRequest, 0, len(lineChunks)+len(dropChunks))
	for i, chunk := range dropChunks {
		req := &platformv1.ReportBuildLogsRequest{BuilderId: r.builderID, BuildId: r.buildID, LeaseEpoch: r.leaseEpoch, Drops: chunk}
		if i == 0 {
			req.DroppedLines = dropped
		}
		requests = append(requests, req)
	}
	for _, chunk := range lineChunks {
		requests = append(requests, &platformv1.ReportBuildLogsRequest{BuilderId: r.builderID, BuildId: r.buildID, LeaseEpoch: r.leaseEpoch, Lines: chunk})
	}
	for _, req := range requests {
		reportCtx, cancel := context.WithTimeout(r.ctx, r.reportTimeout)
		_, err = r.client.ReportBuildLogs(reportCtx, req)
		cancel()
		if err != nil {
			r.spool.Release()
			if status.Code(err) == codes.PermissionDenied {
				r.orphaned.Store(true)
				slog.Warn("build log reporter orphaned by lease loss", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
				return
			}
			slog.Warn("report build logs", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
			return
		}
	}
	if err := r.spool.Commit(cursor, taken); err != nil {
		slog.Warn("commit build log spool", "builder_id", r.builderID, "build_id", r.buildID, "error", err)
		return
	}
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
// directory entry: writes touch the database file while the directory mtime
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
