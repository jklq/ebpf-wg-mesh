package agent

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

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

func TestContainerLogWriterShipsCappedPrefixWithoutNewline(t *testing.T) {
	t.Parallel()
	sink := &recordingLogSink{}
	var seq uint64
	writer := &containerLogWriter{
		allocationID: "alloc-1",
		stream:       "stdout",
		nextSequence: func() uint64 {
			seq++
			return seq
		},
		sink: func() LogSink { return sink },
	}
	if _, err := writer.Write([]byte(strings.Repeat("x", logpipeline.MaxLogLineBytes))); err != nil {
		t.Fatalf("Write capped prefix: %v", err)
	}
	if len(sink.entries) != 1 || len(sink.entries[0].GetLine()) != logpipeline.MaxLogLineBytes || !sink.entries[0].GetTruncated() {
		t.Fatalf("capped prefix was not shipped immediately: %+v", sink.entries)
	}
	if _, err := writer.Write([]byte("discarded\nnext\n")); err != nil {
		t.Fatalf("Write following lines: %v", err)
	}
	if len(sink.entries) != 2 || sink.entries[1].GetLine() != "next" {
		t.Fatalf("continued line was emitted twice or hid the next line: %+v", sink.entries)
	}
}

func TestContainerLogWriterTruncatedLineStaysValidUTF8(t *testing.T) {
	t.Parallel()

	sink := &recordingLogSink{}
	var seq uint64
	writer := &containerLogWriter{
		allocationID: "alloc-1",
		stream:       "stdout",
		nextSequence: func() uint64 {
			seq++
			return seq
		},
		sink: func() LogSink { return sink },
	}
	// A long multibyte line crosses the size cap mid-rune; the
	// emitted prefix must stay valid UTF-8 or protobuf rejects the
	// whole line instead of just its tail.
	line := append([]byte{'x'}, bytes.Repeat([]byte("é"), logpipeline.MaxLogLineBytes)...)
	if _, err := writer.Write(append(line, '\n')); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(sink.entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(sink.entries))
	}
	if got := sink.entries[0].GetLine(); !utf8.ValidString(got) {
		t.Fatal("truncated line must stay valid UTF-8")
	}
	if !sink.entries[0].GetTruncated() {
		t.Fatal("line must be flagged truncated")
	}
}
