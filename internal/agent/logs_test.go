package agent

import (
	"strings"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
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
		projectID:         "project-1",
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
	if got := sink.entries[0].GetProjectId(); got != "project-1" {
		t.Fatalf("unexpected project id %q", got)
	}
	if got := sink.entries[0].GetRolloutGeneration(); got != 7 {
		t.Fatalf("unexpected rollout generation %d", got)
	}
	if got := sink.entries[1].GetSequence(); got != 2 {
		t.Fatalf("unexpected sequence %d", got)
	}
}

func TestContainerLogWriterBoundsPartialLines(t *testing.T) {
	t.Parallel()

	sink := &recordingLogSink{}
	var seq uint64
	writer := &containerLogWriter{
		projectID:         "project-1",
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

	if _, err := writer.Write([]byte(strings.Repeat("x", maxContainerLogLine+1))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(sink.entries) != 1 {
		t.Fatalf("expected one full chunk, got %d", len(sink.entries))
	}
	if got := len(sink.entries[0].GetLine()); got != maxContainerLogLine {
		t.Fatalf("unexpected line length %d", got)
	}
	if got := sink.entries[0].GetStream(); got != "stderr" {
		t.Fatalf("unexpected stream %q", got)
	}
}
