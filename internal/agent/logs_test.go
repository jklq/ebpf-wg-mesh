package agent

import (
	"strings"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/logpipeline"
)

type recordingLogSink struct {
	entries []*agentv1.LogEntry
}

func (s *recordingLogSink) AppendLog(entry *agentv1.LogEntry) {
	s.entries = append(s.entries, entry)
}

func TestContainerLogWriterSplitsLinesWithMetadata(t *testing.T) {
	t.Parallel()

	sink := &recordingLogSink{}
	var seq uint64
	writer := &containerLogWriter{
		agentID:           "agent-1",
		bootID:            "boot-1",
		environmentID:     "project-1",
		serviceID:         "service-1",
		allocationID:      "alloc-1",
		stream:            "stdout",
		rolloutGeneration: 7,
		nextSequence: func() uint64 {
			seq++
			return seq
		},
		sink: func() LogSink { return sink },
	}

	n, err := writer.Write([]byte("first\nsecond\r\npartial"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len("first\nsecond\r\npartial") {
		t.Fatalf("unexpected bytes written: %d", n)
	}
	if len(sink.entries) != 2 {
		t.Fatalf("expected 2 emitted lines, got %d", len(sink.entries))
	}
	if got := sink.entries[0].GetLine(); got != "first" {
		t.Fatalf("unexpected first line %q", got)
	}
	if got := sink.entries[1].GetLine(); got != "second" {
		t.Fatalf("unexpected second line %q", got)
	}
	if got := sink.entries[0].GetEnvironmentId(); got != "project-1" {
		t.Fatalf("unexpected project id %q", got)
	}
	if got := sink.entries[0].GetRolloutGeneration(); got != 7 {
		t.Fatalf("unexpected rollout generation %d", got)
	}
	if got := sink.entries[1].GetSequence(); got != 2 {
		t.Fatalf("unexpected sequence %d", got)
	}
	if got := sink.entries[0].GetLineId(); got != "ag:agent-1:boot-1:alloc-1:stdout:1" {
		t.Fatalf("unexpected line id %q", got)
	}
	if sink.entries[0].GetTruncated() || sink.entries[1].GetTruncated() {
		t.Fatal("short lines must not be truncated")
	}
}

func TestContainerLogWriterTruncatesOversizedLines(t *testing.T) {
	t.Parallel()

	sink := &recordingLogSink{}
	var seq uint64
	writer := &containerLogWriter{
		agentID:           "agent-1",
		bootID:            "boot-1",
		environmentID:     "project-1",
		serviceID:         "service-1",
		allocationID:      "alloc-1",
		stream:            "stderr",
		rolloutGeneration: 1,
		nextSequence: func() uint64 {
			seq++
			return seq
		},
		sink: func() LogSink { return sink },
	}

	line := strings.Repeat("x", logpipeline.MaxLogLineBytes+100) + "\nnext\n"
	if _, err := writer.Write([]byte(line)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(sink.entries) != 2 {
		t.Fatalf("expected 2 emitted lines, got %d", len(sink.entries))
	}
	if got := len(sink.entries[0].GetLine()); got != logpipeline.MaxLogLineBytes {
		t.Fatalf("unexpected truncated length %d", got)
	}
	if !sink.entries[0].GetTruncated() {
		t.Fatal("oversized line must set truncated")
	}
	if got := sink.entries[1].GetLine(); got != "next" || sink.entries[1].GetTruncated() {
		t.Fatalf("following line corrupted: %q truncated=%v", got, sink.entries[1].GetTruncated())
	}
}
