package logpipeline

import (
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestDropSetCoalescesByIdentity(t *testing.T) {
	t.Parallel()

	set := NewDropSet()
	base := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	limited := DropKey{AllocationID: "alloc-1", Reason: ReasonRateLimited}
	overflow := DropKey{AllocationID: "alloc-1", Reason: ReasonSpoolOverflow}

	set.Add(limited, 3, base, base.Add(time.Second))
	set.Add(limited, 4, base.Add(2*time.Second), base.Add(3*time.Second))
	set.Add(overflow, 5, base, base)

	if set.Len() != 2 {
		t.Fatalf("expected 2 coalesced identities, got %d", set.Len())
	}
	for _, summary := range set.Summaries() {
		switch summary.GetReason() {
		case ReasonRateLimited:
			if summary.GetDroppedCount() != 7 {
				t.Fatalf("counts must sum, got %d", summary.GetDroppedCount())
			}
			if got := summary.GetWindowStart().AsTime(); !got.Equal(base) {
				t.Fatalf("window start must widen to the earliest, got %v", got)
			}
			if got := summary.GetWindowEnd().AsTime(); !got.Equal(base.Add(3 * time.Second)) {
				t.Fatalf("window end must widen to the latest, got %v", got)
			}
		case ReasonSpoolOverflow:
			if summary.GetDroppedCount() != 5 {
				t.Fatalf("distinct identity must keep its own count, got %d", summary.GetDroppedCount())
			}
		}
	}
}

func TestDropSetTakeRestoreKeepsAccountingExact(t *testing.T) {
	t.Parallel()

	set := NewDropSet()
	key := DropKey{
		ServiceID:    "svc-1",
		AllocationID: "alloc-1",
		LogType:      platformv1.ServiceLogType_SERVICE_LOG_TYPE_RUNTIME,
		Reason:       ReasonRateLimited,
	}
	base := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	set.Add(key, 3, base, base)

	taken := set.Take()
	if set.Len() != 0 {
		t.Fatalf("Take must empty the set, %d left", set.Len())
	}
	if len(taken) != 1 || taken[0].GetDroppedCount() != 3 {
		t.Fatalf("Take lost summaries: %+v", taken)
	}

	// Drops arriving while the send is in flight must not be lost
	// when the failed send restores its taken summaries.
	set.Add(key, 4, base.Add(time.Minute), base.Add(time.Minute))
	set.Restore(taken)

	summaries := set.Summaries()
	if len(summaries) != 1 {
		t.Fatalf("restore must coalesce into one identity, got %d", len(summaries))
	}
	if summaries[0].GetDroppedCount() != 7 {
		t.Fatalf("restore lost counts, got %d", summaries[0].GetDroppedCount())
	}
	if got := summaries[0].GetWindowStart().AsTime(); !got.Equal(base) {
		t.Fatalf("restore lost the earlier window start, got %v", got)
	}
	if got := summaries[0].GetWindowEnd().AsTime(); !got.Equal(base.Add(time.Minute)) {
		t.Fatalf("restore lost the later window end, got %v", got)
	}
}

func TestDropSetSummaryIDStableAcrossGrowthAndRetry(t *testing.T) {
	t.Parallel()

	set := NewDropSet()
	key := DropKey{AllocationID: "alloc-1", Reason: ReasonRateLimited}
	base := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	set.Add(key, 3, base, base)

	original := set.Summaries()[0].GetSummaryId()
	if original == "" {
		t.Fatal("summary must carry a stable identity")
	}

	// A failed send restores the summary and later drops grow it. The
	// identity must survive the growth, or at-least-once retries
	// double-count server-side.
	taken := set.Take()
	set.Add(key, 4, base.Add(time.Minute), base.Add(time.Minute))
	set.Restore(taken)

	grown := set.Summaries()[0]
	if grown.GetSummaryId() != original {
		t.Fatalf("summary identity changed across retry: %q vs %q", grown.GetSummaryId(), original)
	}
	if grown.GetDroppedCount() != 7 {
		t.Fatalf("restore lost counts, got %d", grown.GetDroppedCount())
	}

	// A fresh lineage after a confirmed send gets its own identity.
	set.Take()
	set.Add(key, 2, base.Add(2*time.Minute), base.Add(2*time.Minute))
	if fresh := set.Summaries()[0].GetSummaryId(); fresh == original {
		t.Fatal("fresh lineage must not reuse a sent summary identity")
	}
}
