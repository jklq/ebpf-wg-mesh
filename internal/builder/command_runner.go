package builder

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	commandScannerInitialBuffer = 64 * 1024
	commandScannerMaxLineBytes  = 256 * 1024
	commandFailureOutputBytes   = 256 * 1024
	maxBuildFailureTailBytes    = 8 * 1024
	buildLogBatchSize           = 100
	buildLogFlushInterval       = time.Second
	buildLogReporterBufferSize  = 1024
	buildLogCloseFlushTimeout   = 2 * time.Second
)

type commandOutputLine struct {
	ObservedAt time.Time
	Stream     string
	Line       string
}

type osCommandRunner struct{}

func (osCommandRunner) Run(ctx context.Context, req commandRequest, onLine func(commandOutputLine)) ([]byte, error) {
	cmd := exec.CommandContext(ctx, req.Binary, req.Args...)
	cmd.Dir = req.Dir
	cmd.Env = append(os.Environ(), req.Env...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var (
		combined boundedTailBuffer
		wg       sync.WaitGroup
	)
	combined.max = commandFailureOutputBytes

	wg.Add(2)
	go func() {
		defer wg.Done()
		scanCommandStream(ctx, stdout, "stdout", &combined, onLine)
	}()
	go func() {
		defer wg.Done()
		scanCommandStream(ctx, stderr, "stderr", &combined, onLine)
	}()

	waitErr := cmd.Wait()
	wg.Wait()
	return combined.Bytes(), waitErr
}

func scanCommandStream(ctx context.Context, reader io.Reader, stream string, combined *boundedTailBuffer, onLine func(commandOutputLine)) {
	scanner := bufio.NewScanner(newTruncatingLineReader(reader, commandScannerMaxLineBytes))
	scanner.Buffer(make([]byte, commandScannerInitialBuffer), commandScannerMaxLineBytes+1)
	for scanner.Scan() {
		line := scanner.Text()
		combined.Write([]byte(line))
		combined.Write([]byte{'\n'})
		if onLine != nil {
			onLine(commandOutputLine{
				ObservedAt: time.Now().UTC(),
				Stream:     stream,
				Line:       line,
			})
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
		slog.WarnContext(ctx, "scan command stream", "stream", stream, "error", err)
	}
}

type boundedTailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (b *boundedTailBuffer) Write(p []byte) {
	if b == nil || len(p) == 0 || b.max <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= b.max {
		b.buf = append(b.buf[:0], p[len(p)-b.max:]...)
		return
	}
	overflow := len(b.buf) + len(p) - b.max
	if overflow > 0 {
		b.buf = append([]byte(nil), b.buf[overflow:]...)
	}
	b.buf = append(b.buf, p...)
}

func (b *boundedTailBuffer) Bytes() []byte {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf...)
}

type truncatingLineReader struct {
	reader     io.Reader
	maxLineLen int
	currentLen int
	dropping   bool
	srcBuf     [32 * 1024]byte
	pending    bytes.Buffer
}

func newTruncatingLineReader(reader io.Reader, maxLineLen int) io.Reader {
	return &truncatingLineReader{
		reader:     reader,
		maxLineLen: maxLineLen,
	}
}

func (r *truncatingLineReader) Read(p []byte) (int, error) {
	for r.pending.Len() == 0 {
		n, err := r.reader.Read(r.srcBuf[:])
		if n > 0 {
			r.transform(r.srcBuf[:n])
		}
		if r.pending.Len() > 0 {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	return r.pending.Read(p)
}

func (r *truncatingLineReader) transform(src []byte) {
	for _, b := range src {
		if r.dropping {
			if b == '\n' {
				r.dropping = false
				r.currentLen = 0
			}
			continue
		}
		if b == '\n' {
			r.currentLen = 0
			_ = r.pending.WriteByte(b)
			continue
		}
		if r.currentLen < r.maxLineLen {
			r.currentLen++
			_ = r.pending.WriteByte(b)
			continue
		}
		r.currentLen = 0
		r.dropping = true
		_ = r.pending.WriteByte('\n')
	}
}

type buildLogReporter struct {
	client    platformv1.BuilderServiceClient
	builderID string
	buildID   string
	sequence  atomic.Uint64
	ch        chan commandOutputLine
	done      chan struct{}
	closeOnce sync.Once
	ctx       context.Context
}

func newBuildLogReporter(ctx context.Context, client platformv1.BuilderServiceClient, builderID, buildID string) *buildLogReporter {
	if client == nil || buildID == "" {
		return nil
	}
	reporter := &buildLogReporter{
		client:    client,
		builderID: builderID,
		buildID:   buildID,
		ch:        make(chan commandOutputLine, buildLogReporterBufferSize),
		done:      make(chan struct{}),
		ctx:       ctx,
	}
	go reporter.run()
	return reporter
}

func (r *buildLogReporter) Report(ctx context.Context, line commandOutputLine) {
	if r == nil {
		return
	}
	select {
	case r.ch <- line:
	case <-ctx.Done():
	default:
	}
}

func (r *buildLogReporter) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		close(r.ch)
		select {
		case <-r.done:
		case <-time.After(buildLogCloseFlushTimeout):
		}
	})
}

func (r *buildLogReporter) run() {
	defer close(r.done)

	ticker := time.NewTicker(buildLogFlushInterval)
	defer ticker.Stop()

	batch := make([]*platformv1.BuildLogLine, 0, buildLogBatchSize)
	flush := func(ctx context.Context) {
		if len(batch) == 0 {
			return
		}
		req := &platformv1.ReportBuildLogsRequest{
			BuilderId: r.builderID,
			BuildId:   r.buildID,
			Lines:     batch,
		}
		if _, err := r.client.ReportBuildLogs(ctx, req); err != nil {
			slog.WarnContext(ctx, "report build logs", "builder_id", r.builderID, "build_id", r.buildID, "line_count", len(batch), "error", err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case line, ok := <-r.ch:
			if !ok {
				flushCtx, cancel := context.WithTimeout(context.Background(), buildLogCloseFlushTimeout)
				flush(flushCtx)
				cancel()
				return
			}
			observedAt := line.ObservedAt.UTC()
			if observedAt.IsZero() {
				observedAt = time.Now().UTC()
			}
			batch = append(batch, &platformv1.BuildLogLine{
				ObservedAt: timestamppb.New(observedAt),
				Stream:     line.Stream,
				Sequence:   r.sequence.Add(1),
				Line:       line.Line,
			})
			if len(batch) >= buildLogBatchSize {
				flush(r.ctx)
			}
		case <-ticker.C:
			flush(r.ctx)
		}
	}
}
