package delivery

import (
	"testing"
	"time"
)

func TestSchedulingPlanRequiresRecordedAllocationReservation(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	plan := allocationMutationPlan(now, SchedulingDecision{Kind: DecisionCreateAllocation, Allocation: AllocationAssignment{ID: "alloc-1", AgentID: "agent-1"}})
	if err := plan.validate(); err == nil {
		t.Fatal("plan without reserved addresses was accepted")
	}
}
