package builder

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"
)

const (
	commandScannerInitialBuffer = 64 * 1024
	commandScannerMaxLineBytes  = 256 * 1024
	commandFailureOutputBytes   = 256 * 1024
	maxBuildFailureTailBytes    = 8 * 1024
)

type commandOutputLine struct {
	ObservedAt time.Time
	Stream     string
	Line       string
}

type osCommandRunner struct{}

func (osCommandRunner) Run(ctx context.Context, req commandRequest, onLine func(commandOutputLine)) ([]byte, error) {
	cmd := newBuildCommand(ctx, req.Binary, req.Args)
	cmd.Dir = req.Dir
	// Explicit environment only: a nil Env must not fall back to
	// inheriting the builder process environment.
	cmd.Env = req.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}

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
	if req.Limits != nil {
		if err := applyProcessLimits(cmd.Process.Pid, *req.Limits); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, err
		}
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

	wg.Wait()
	waitErr := cmd.Wait()
	// Report cancellation as the context error so callers can
	// distinguish a cancelled build from a failed one.
	if ctx.Err() != nil {
		return combined.Bytes(), ctx.Err()
	}
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
