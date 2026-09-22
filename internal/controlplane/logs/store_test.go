package logs

import (
	"strings"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/logpipeline"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestLogStoreSchemaUsesReplacingMergeTreeAndRowTTL(t *testing.T) {
	t.Parallel()

	stmts := logStoreSchema()
	if len(stmts) != 2 {
		t.Fatalf("expected 2 schema statements, got %d", len(stmts))
	}
	lines, gaps := stmts[0], stmts[1]
	for _, want := range []string{
		"ENGINE = ReplacingMergeTree(ingested_at)",
		"ORDER BY (service_id, observed_at, line_id)",
		"TTL expires_at",
		"line_id String",
		"project_id String",
		"attributes Map(String, String)",
		"truncated UInt8",
	} {
		if !strings.Contains(lines, want) {
			t.Fatalf("service_logs schema missing %q:\n%s", want, lines)
		}
	}
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS service_log_gaps",
		"ENGINE = ReplacingMergeTree(ingested_at)",
		"gap_id String",
		"expires_at DateTime",
		"TTL expires_at",
	} {
		if !strings.Contains(gaps, want) {
			t.Fatalf("service_log_gaps schema missing %q:\n%s", want, gaps)
		}
	}
	if strings.Contains(gaps, "INTERVAL 90 DAY") {
		t.Fatalf("service_log_gaps must expire by project retention, not a fixed TTL:\n%s", gaps)
	}
}

func TestConvertAgentBatchEnforcesCaps(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	oversized := strings.Repeat("x", logpipeline.MaxLogLineBytes+100)
	batch := &agentv1.LogBatch{
		AgentId: "agent-1",
		Entries: []*agentv1.LogEntry{
			{
				ObservedAt:    timestamppb.New(now),
				EnvironmentId: "env-1",
				ServiceId:     "svc-1",
				AllocationId:  "alloc-1",
				Stream:        "STDOUT",
				Line:          oversized,
				LineId:        "ag:1",
				LogType:       platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME,
				Attributes:    map[string]string{"phase": "running"},
				Event:         "allocation.crash_loop",
			},
			{
				// Missing allocation: skipped.
				EnvironmentId: "env-1",
				ServiceId:     "svc-1",
				Line:          "orphan",
			},
		},
		Drops: []*platformv1.LogDropSummary{
			{
				ServiceId:    "svc-1",
				AllocationId: "alloc-1",
				LogType:      platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME,
				Stream:       "stdout",
				DroppedCount: 7,
				Reason:       logpipeline.ReasonRateLimited,
				WindowStart:  timestamppb.New(now),
				WindowEnd:    timestamppb.New(now),
			},
			{ServiceId: "svc-1"}, // Zero count: skipped.
		},
	}
	inputs, gaps := convertAgentBatch("agent-1", batch)
	if len(inputs) != 1 {
		t.Fatalf("expected 1 input, got %d", len(inputs))
	}
	in := inputs[0]
	if len(in.Line) != logpipeline.MaxLogLineBytes || !in.Truncated {
		t.Fatalf("oversized line not truncated: len=%d truncated=%v", len(in.Line), in.Truncated)
	}
	if in.Stream != "STDOUT" || in.AgentID != "agent-1" || in.Event != "allocation.crash_loop" {
		t.Fatalf("conversion mangled input: stream=%q agent=%q event=%q", in.Stream, in.AgentID, in.Event)
	}
	if in.Attributes["phase"] != "running" {
		t.Fatalf("attributes dropped: %+v", in.Attributes)
	}
	if len(gaps) != 1 || gaps[0].DroppedCount != 7 || gaps[0].Reporter != "agent-1" {
		t.Fatalf("gaps not converted: %+v", gaps)
	}
}

func TestConvertAgentBatchTrimsOversizedBatches(t *testing.T) {
	t.Parallel()

	entries := make([]*agentv1.LogEntry, 0, maxEntriesPerBatch+10)
	for i := 0; i < maxEntriesPerBatch+10; i++ {
		entries = append(entries, &agentv1.LogEntry{
			EnvironmentId: "env-1",
			ServiceId:     "svc-1",
			AllocationId:  "alloc-1",
			Line:          "line",
		})
	}
	inputs, gaps := convertAgentBatch("agent-1", &agentv1.LogBatch{AgentId: "agent-1", Entries: entries})
	if len(inputs) != maxEntriesPerBatch {
		t.Fatalf("expected trimmed inputs %d, got %d", maxEntriesPerBatch, len(inputs))
	}
	if len(gaps) != 1 || gaps[0].DroppedCount != 10 || gaps[0].Reason != logpipeline.ReasonIngestOverflow {
		t.Fatalf("trimmed tail not counted as gap: %+v", gaps)
	}
	if gaps[0].ServiceID != "svc-1" || gaps[0].AllocationID != "alloc-1" {
		t.Fatalf("trimmed tail gap lost its attribution: %+v", gaps[0])
	}
}

func TestConvertAgentBatchTrimmedTailAggregatesPerService(t *testing.T) {
	t.Parallel()

	entries := make([]*agentv1.LogEntry, 0, maxEntriesPerBatch+12)
	for i := 0; i < maxEntriesPerBatch; i++ {
		entries = append(entries, &agentv1.LogEntry{
			EnvironmentId: "env-1",
			ServiceId:     "svc-1",
			AllocationId:  "alloc-1",
			Line:          "line",
		})
	}
	for i := 0; i < 7; i++ {
		entries = append(entries, &agentv1.LogEntry{
			EnvironmentId: "env-1",
			ServiceId:     "svc-1",
			AllocationId:  "alloc-1",
			Line:          "line",
		})
	}
	for i := 0; i < 5; i++ {
		entries = append(entries, &agentv1.LogEntry{
			EnvironmentId: "env-1",
			ServiceId:     "svc-2",
			AllocationId:  "alloc-2",
			Line:          "line",
		})
	}
	_, gaps := convertAgentBatch("agent-1", &agentv1.LogBatch{AgentId: "agent-1", Entries: entries})
	counts := make(map[string]uint64)
	for _, gap := range gaps {
		counts[gap.ServiceID] = gap.DroppedCount
		if gap.Reason != logpipeline.ReasonIngestOverflow {
			t.Fatalf("unexpected gap reason: %+v", gap)
		}
	}
	if counts["svc-1"] != 7 || counts["svc-2"] != 5 {
		t.Fatalf("trimmed tail not attributed per service: %v", counts)
	}
	if len(counts) != 2 {
		t.Fatalf("unexpected gap services: %v", counts)
	}
}

func TestGapIdentityIsStable(t *testing.T) {
	t.Parallel()

	window := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	first := gapIdentity("svc", "alloc", "", "runtime", "stdout", logpipeline.ReasonRateLimited, "agent-1", window, window, 5)
	second := gapIdentity("svc", "alloc", "", "runtime", "stdout", logpipeline.ReasonRateLimited, "agent-1", window, window, 5)
	if first != second {
		t.Fatal("identical windows must produce identical gap IDs for retry dedup")
	}
	other := gapIdentity("svc", "alloc", "", "runtime", "stdout", logpipeline.ReasonRateLimited, "agent-1", window, window, 6)
	if first == other {
		t.Fatal("distinct windows must not share a gap ID")
	}
}

func TestClampRetentionDays(t *testing.T) {
	t.Parallel()

	if got := clampRetentionDays(7, 14); got != 7 {
		t.Fatalf("project retention not honored: %d", got)
	}
	if got := clampRetentionDays(0, 14); got != 14 {
		t.Fatalf("platform default not used: %d", got)
	}
	if got := clampRetentionDays(500, 14); got != MaxProjectRetentionDays {
		t.Fatalf("project retention not capped: %d", got)
	}
	if got := clampRetentionDays(0, 0); got != 14 {
		t.Fatalf("zero default not floored: %d", got)
	}
}
