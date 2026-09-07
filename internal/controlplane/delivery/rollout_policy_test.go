package delivery

import (
	"database/sql"

	"reflect"
	"testing"
	"time"
)

func policyAllocation(id, state string, generation int64, now time.Time) AllocationRecord {
	return AllocationRecord{ID: id, AgentID: id, RolloutState: state, DesiredRolloutGeneration: generation, AppliedRolloutGeneration: generation, Healthy: true, AllocationIPv4: "10.0.0.1", AllocationIPv6: "fd00::1", CreatedAt: now}
}

func TestRolloutDecisionPromotionAndBarrier(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	rollout := rolloutRecord{State: rolloutStateInProgress, Generation: 2, DesiredReplicaCount: 2, Strategy: canonicalRollingStrategy(nil), ProgressAt: now}
	allocations := []AllocationRecord{policyAllocation("old-b", AllocationRolloutServing, 1, now), policyAllocation("new", AllocationRolloutStarting, 2, now), policyAllocation("old-a", AllocationRolloutServing, 1, now)}
	snapshot := rolloutSnapshot{Rollout: rollout, Allocations: allocations}
	original := append([]AllocationRecord(nil), allocations...)
	plan := decideRollout(snapshot, now)
	if len(plan.Promote) != 1 || len(plan.Withdraw) != 1 || plan.Withdraw[0].AllocationID != "old-a" || !plan.Result.NeedsIngressConvergence || plan.Complete || plan.PlacementSlots != 0 {
		t.Fatalf("promotion plan: %+v", plan)
	}
	if !reflect.DeepEqual(allocations, original) || !reflect.DeepEqual(plan, decideRollout(snapshot, now)) {
		t.Fatal("decision mutated input or was not deterministic")
	}
	// Even an expired scheduling timeout cannot bypass a persisted withdrawal.
	barrier := decideRollout(rolloutSnapshot{Rollout: rollout, Allocations: plan.Allocations}, now.Add(time.Hour))
	if !barrier.Result.NeedsIngressConvergence || barrier.Continue || barrier.Failure != "" || len(barrier.Remove) != 0 {
		t.Fatalf("barrier: %+v", barrier)
	}
}

func TestRolloutDecisionDrainAndCompletion(t *testing.T) {
	now := time.Now().UTC()
	old := policyAllocation("old", AllocationRolloutDraining, 1, now)
	old.DrainDeadline = sql.NullTime{Valid: true, Time: now}
	target := policyAllocation("new", AllocationRolloutServing, 2, now)
	snapshot := rolloutSnapshot{Rollout: rolloutRecord{State: rolloutStateInProgress, Generation: 2, DesiredReplicaCount: 1}, Allocations: []AllocationRecord{old, target}}
	if plan := decideRollout(snapshot, now.Add(-time.Nanosecond)); plan.Complete || len(plan.Remove) != 0 {
		t.Fatalf("early removal: %+v", plan)
	}
	if plan := decideRollout(snapshot, now); !plan.Complete || len(plan.Remove) != 1 {
		t.Fatalf("completion: %+v", plan)
	}
	snapshot.Removing = true
	snapshot.Allocations = []AllocationRecord{old}
	if plan := decideRollout(snapshot, now); !plan.CompleteRemoval || plan.Complete {
		t.Fatalf("removal: %+v", plan)
	}
	snapshot.Removing = false
	snapshot.Rollout.State = rolloutStateFailed
	if plan := decideRollout(snapshot, now); len(plan.Remove) != 1 || plan.Continue || plan.Complete {
		t.Fatalf("terminal cleanup: %+v", plan)
	}
}

func TestRolloutDecisionReadinessDeadline(t *testing.T) {
	now := time.Now().UTC()
	alloc := policyAllocation("new", AllocationRolloutStarting, 2, now)
	alloc.Healthy = false
	snapshot := rolloutSnapshot{Rollout: rolloutRecord{State: rolloutStateInProgress, Generation: 2, DesiredReplicaCount: 1, Strategy: canonicalRollingStrategy(nil)}, Allocations: []AllocationRecord{alloc}}
	if plan := decideRollout(snapshot, now.Add(defaultHealthcheckTimeout-time.Nanosecond)); plan.Failure != "" {
		t.Fatal(plan.Failure)
	}
	if plan := decideRollout(snapshot, now.Add(defaultHealthcheckTimeout)); plan.Failure == "" || len(plan.FailureTargets) != 1 {
		t.Fatalf("deadline: %+v", plan)
	}
	snapshot.Allocations[0].Phase = "Failed"
	if plan := decideRollout(snapshot, now); plan.Failure == "" {
		t.Fatal("failed replacement was not rejected")
	}
}

func TestRolloutPlacementDecision(t *testing.T) {
	now := time.Now().UTC()
	rollout := rolloutRecord{State: rolloutStateInProgress, Generation: 2, DesiredReplicaCount: 2, ProgressAt: now}
	snapshot := rolloutSnapshot{Rollout: rollout}
	for _, tc := range []struct {
		name    string
		created int
		pending bool
		elapsed time.Duration
		failure bool
		message bool
	}{
		{"waiting", 0, false, defaultRolloutSchedulingWait - time.Nanosecond, false, true},
		{"timeout", 0, false, defaultRolloutSchedulingWait, true, true},
		{"partial placement", 1, false, defaultRolloutSchedulingWait, false, true},
		{"placed", 2, false, defaultRolloutSchedulingWait, false, false},
		{"ingress pending", 0, true, defaultRolloutSchedulingWait, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := decideRolloutPlacement(snapshot, tc.created, tc.pending, now.Add(tc.elapsed))
			if (p.Failure != "") != tc.failure || (p.PlacementMessage != "") != tc.message {
				t.Fatalf("outcome: %+v", p)
			}
		})
	}
	// A targeted replacement counts unaffected healthy replicas toward availability,
	// but must never select them for withdrawal.
	snapshot.Rollout.TargetAllocationID = "selected"
	snapshot.Allocations = []AllocationRecord{policyAllocation("selected", AllocationRolloutServing, 1, now), policyAllocation("unaffected", AllocationRolloutServing, 1, now)}
	if p := decideRollout(snapshot, now); p.PlacementSlots != 1 {
		t.Fatalf("targeted slots: %+v", p)
	}
	if p := decideRolloutPlacement(snapshot, 0, false, now); len(p.Withdraw) != 0 {
		t.Fatalf("availability floor: %+v", p)
	}
	snapshot.Rollout.DesiredReplicaCount = 1
	snapshot.Rollout.TargetAllocationID = ""
	if p := decideRolloutPlacement(snapshot, 0, false, now); len(p.Withdraw) != 1 {
		t.Fatalf("scale down: %+v", p)
	}
}

func TestRolloutFailurePreservesServingReplacements(t *testing.T) {
	now := time.Now().UTC()
	serving := policyAllocation("serving", AllocationRolloutServing, 2, now)
	failed := policyAllocation("failed", AllocationRolloutStarting, 2, now)
	failed.Healthy = false
	failed.Phase = "Failed"
	snapshot := rolloutSnapshot{Rollout: rolloutRecord{State: rolloutStateInProgress, Generation: 2, DesiredReplicaCount: 2, Strategy: canonicalRollingStrategy(nil)}, Allocations: []AllocationRecord{serving, failed}}
	plan := decideRollout(snapshot, now)
	if plan.Failure == "" || len(plan.FailureTargets) != 1 || plan.FailureTargets[0].ID != failed.ID {
		t.Fatalf("failure cleanup: %+v", plan)
	}
}
