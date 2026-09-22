package controlplane

import (
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// A crash-loop observation resent after a reconnect must collapse
// into one event row: identity and observed_at derive from the
// content-stable facts of the observation, not the report wall clock.
func TestCrashLoopEventLineStableAcrossResends(t *testing.T) {
	t.Parallel()

	window := timestamppb.New(time.Date(2026, 9, 22, 1, 2, 3, 0, time.UTC))
	cond := func() *agentv1.ServiceCondition {
		return &agentv1.ServiceCondition{
			AllocationId:             "alloc-1",
			ServiceId:                "svc-1",
			DesiredRolloutGeneration: 7,
			Phase:                    "CrashLoop",
			Restart: &platformv1.RestartObservation{
				CrashLoop:                true,
				RestartCount:             5,
				WindowStartedAt:          window,
				AppliedRolloutGeneration: 7,
			},
		}
	}

	first := crashLoopEventLine("agent-1", "env-1", cond())
	resend := crashLoopEventLine("agent-1", "env-1", cond())
	if first.ID != resend.ID {
		t.Fatalf("resent observation changed event identity: %q vs %q", first.ID, resend.ID)
	}
	if !first.ObservedAt.Equal(resend.ObservedAt) {
		t.Fatalf("resent observation changed observed_at: %v vs %v", first.ObservedAt, resend.ObservedAt)
	}
	if first.Sequence == resend.Sequence {
		t.Fatalf("distinct emissions share sequence %d", first.Sequence)
	}

	// A new restart window is a new crash-loop episode and must get a
	// fresh identity.
	nextWindow := cond()
	nextWindow.Restart.WindowStartedAt = timestamppb.New(time.Date(2026, 9, 22, 2, 0, 0, 0, time.UTC))
	next := crashLoopEventLine("agent-1", "env-1", nextWindow)
	if first.ID == next.ID {
		t.Fatalf("new restart window reused event identity %q", first.ID)
	}

	// Another allocation in the same window is its own event.
	otherAlloc := cond()
	otherAlloc.AllocationId = "alloc-2"
	if first.ID == crashLoopEventLine("agent-1", "env-1", otherAlloc).ID {
		t.Fatalf("foreign allocation reused event identity %q", first.ID)
	}
}
