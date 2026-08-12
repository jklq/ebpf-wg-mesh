package agent

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	logBatchSize        = 100
	logFlushInterval    = time.Second
	logSinkBufferSize   = 4096
	maxContainerLogLine = 256 * 1024
)

type LogSink interface {
	AppendLog(*agentv1.LogEntry)
}

type logSinkRuntime interface {
	SetLogSink(LogSink)
}

type streamLogSink struct {
	ctx     context.Context
	agentID string
	send    func(*agentv1.AgentClientMessage) error
	ch      chan *agentv1.LogEntry
	done    chan struct{}
	once    sync.Once
	mu      sync.RWMutex
	closed  bool
}

func newStreamLogSink(ctx context.Context, agentID string, send func(*agentv1.AgentClientMessage) error) *streamLogSink {
	sink := &streamLogSink{
		ctx:     ctx,
		agentID: agentID,
		send:    send,
		ch:      make(chan *agentv1.LogEntry, logSinkBufferSize),
		done:    make(chan struct{}),
	}
	go sink.run()
	return sink
}

func (s *streamLogSink) AppendLog(entry *agentv1.LogEntry) {
	if s == nil || entry == nil {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return
	}
	select {
	case <-s.ctx.Done():
	case s.ch <- entry:
	default:
		slog.Warn("dropping container log line because agent log buffer is full", "agent_id", s.agentID)
	}
}

func (s *streamLogSink) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.ch)
		s.mu.Unlock()
		<-s.done
	})
}

func (s *streamLogSink) run() {
	defer close(s.done)
	ticker := time.NewTicker(logFlushInterval)
	defer ticker.Stop()
	batch := make([]*agentv1.LogEntry, 0, logBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		entries := append([]*agentv1.LogEntry(nil), batch...)
		batch = batch[:0]
		if err := s.send(&agentv1.AgentClientMessage{
			Payload: &agentv1.AgentClientMessage_LogBatch{
				LogBatch: &agentv1.LogBatch{
					AgentId: s.agentID,
					Entries: entries,
				},
			},
		}); err != nil && s.ctx.Err() == nil {
			slog.Warn("send container log batch failed", "agent_id", s.agentID, "error", err)
		}
	}
	for {
		select {
		case entry, ok := <-s.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, entry)
			if len(batch) >= logBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-s.ctx.Done():
			flush()
			return
		}
	}
}

type containerLogWriter struct {
	environmentID     string
	serviceID         string
	allocationID      string
	stream            string
	rolloutGeneration int64
	nextSequence      func() uint64
	sink              func() LogSink

	mu  sync.Mutex
	buf []byte
}

func (w *containerLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written := len(p)
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			w.appendLocked(p)
			break
		}
		w.appendLocked(p[:idx])
		w.emitLocked()
		p = p[idx+1:]
	}
	return written, nil
}

func (w *containerLogWriter) appendLocked(p []byte) {
	for len(p) > 0 {
		room := maxContainerLogLine - len(w.buf)
		if room <= 0 {
			w.emitLocked()
			room = maxContainerLogLine
		}
		if room > len(p) {
			room = len(p)
		}
		w.buf = append(w.buf, p[:room]...)
		p = p[room:]
		if len(w.buf) >= maxContainerLogLine {
			w.emitLocked()
		}
	}
}

func (w *containerLogWriter) emitLocked() {
	if w == nil || w.nextSequence == nil || w.sink == nil {
		w.buf = w.buf[:0]
		return
	}
	line := strings.TrimSuffix(string(w.buf), "\r")
	w.buf = w.buf[:0]
	sink := w.sink()
	if sink == nil {
		return
	}
	sink.AppendLog(&agentv1.LogEntry{
		ObservedAt:        timestamppb.Now(),
		EnvironmentId:     w.environmentID,
		ServiceId:         w.serviceID,
		AllocationId:      w.allocationID,
		Stream:            w.stream,
		RolloutGeneration: w.rolloutGeneration,
		Sequence:          w.nextSequence(),
		Line:              line,
	})
}
