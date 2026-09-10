package delivery

import (
	"database/sql"
	"reflect"
	"testing"
	"time"
)

func TestEvaluateSchedulerIsDeterministicAndCompletesExpiredDrain(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	input := SchedulerEvaluation{
		Cause:     SchedulerDeadline,
		DecidedAt: now,
		Allocations: []AllocationRecord{
			{ID: "future", RolloutState: AllocationRolloutDraining, DrainDeadline: sql.NullTime{Time: now.Add(time.Second), Valid: true}},
			{ID: "expired", RolloutState: AllocationRolloutDraining, DrainDeadline: sql.NullTime{Time: now, Valid: true}},
		},
	}
	first := EvaluateScheduler(input)
	second := EvaluateScheduler(input)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("evaluation is not deterministic: %#v != %#v", first, second)
	}
	if len(first.Decisions) != 2 || first.Decisions[0].Kind != DecisionCompleteDrain || first.Decisions[0].AllocationID != "expired" ||
		first.Decisions[1].Kind != DecisionEvaluateAt || !first.Decisions[1].EvaluateAt.Equal(now.Add(time.Second)) {
		t.Fatalf("unexpected plan: %#v", first)
	}
}

func TestSchedulingPlanRequiresRecordedAllocationReservation(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	plan := EvaluateScheduler(SchedulerEvaluation{
		Cause: SchedulerDesiredChange, DecidedAt: now,
		Requested: []SchedulingDecision{{Kind: DecisionCreateAllocation, Allocation: AllocationAssignment{ID: "alloc-1", AgentID: "agent-1"}}},
	})
	if err := plan.validate(); err == nil {
		t.Fatal("plan without reserved addresses was accepted")
	}
}
