package agent

import (
	"bytes"
	"sync"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/logpipeline"

	"google.golang.org/protobuf/types/known/timestamppb"
)

type LogSink interface {
	AppendLog(*agentv1.LogEntry)
}

type logSinkRuntime interface {
	SetLogSink(LogSink)
}

type containerLogWriter struct {
	agentID       string
	bootID        string
	environmentID string
	serviceID     string
	allocationID  string
	stream        string

	rolloutGeneration int64
	nextSequence      func() uint64
	sink              func() LogSink

	mu        sync.Mutex
	buf       []byte
	truncated bool
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

// appendLocked accumulates the line prefix up to the documented size
// limit. Bytes past the limit are discarded with the truncated flag
// set; the retained prefix stays byte-exact.
func (w *containerLogWriter) appendLocked(p []byte) {
	room := logpipeline.MaxLogLineBytes - len(w.buf)
	if room <= 0 {
		if len(p) > 0 {
			w.truncated = true
		}
		return
	}
	if len(p) > room {
		p = p[:room]
		w.truncated = true
	}
	w.buf = append(w.buf, p...)
}

func (w *containerLogWriter) emitLocked() {
	line := w.buf
	truncated := w.truncated
	w.buf = nil
	w.truncated = false
	if w == nil || w.nextSequence == nil || w.sink == nil {
		return
	}
	text := string(bytes.TrimSuffix(line, []byte("\r")))
	sequence := w.nextSequence()
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
		Sequence:          sequence,
		Line:              text,
		Truncated:         truncated,
		LineId:            logpipeline.AgentLineID(w.agentID, w.bootID, w.allocationID, w.stream, sequence),
	})
}
