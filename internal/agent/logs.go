package agent

import (
	"bytes"
	"sync"
	"unicode/utf8"

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

	mu         sync.Mutex
	buf        []byte
	truncated  bool
	discarding bool
}

func (w *containerLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written := len(p)
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			if !w.discarding {
				w.appendLocked(p)
				if len(w.buf) == logpipeline.MaxLogLineBytes {
					// A stream may never send a newline: ship the capped prefix now.
					w.truncated = true
					w.emitLocked()
					w.discarding = true
				}
			}
			break
		}
		if !w.discarding {
			w.appendLocked(p[:idx])
			w.emitLocked()
		}
		w.discarding = false
		p = p[idx+1:]
	}
	return written, nil
}

// appendLocked accumulates the line prefix up to the size limit. Bytes past the
// limit are discarded with the truncated flag set; the prefix stays byte-exact.
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
	if truncated {
		// The cap can cut mid-rune, and protobuf rejects invalid UTF-8; trim back
		// to the last rune boundary so the line survives.
		n := len(line)
		for n > 0 && !utf8.RuneStart(line[n-1]) {
			n--
		}
		if n > 0 && !utf8.FullRune(line[n-1:]) {
			n--
		} else {
			n = len(line)
		}
		line = line[:n]
	}
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
