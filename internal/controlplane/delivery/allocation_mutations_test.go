package delivery

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAllocationMutationsRequireTimestamp(t *testing.T) {
	d := &Delivery{}
	err := d.applyAllocationMutationsTx(context.Background(), nil, time.Time{},
		AllocationMutation{Kind: MutationCompleteDrain, AllocationID: "alloc-1"})
	if err == nil {
		t.Fatal("mutations without a timestamp were accepted")
	}
}

func TestAllocationMutationsRequireRecordedAllocationReservation(t *testing.T) {
	d := &Delivery{}
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	err := d.applyAllocationMutationsTx(context.Background(), nil, now,
		AllocationMutation{Kind: MutationCreateAllocation, Allocation: AllocationAssignment{ID: "alloc-1", AgentID: "agent-1"}})
	if err == nil {
		t.Fatal("mutation without reserved addresses was accepted")
	}
}

func TestAllocationMutationsRejectUnknownKind(t *testing.T) {
	d := &Delivery{}
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	err := d.applyAllocationMutationsTx(context.Background(), nil, now,
		AllocationMutation{Kind: AllocationMutationKind("advance_deployment")})
	if err == nil || !strings.Contains(err.Error(), `unknown kind "advance_deployment"`) {
		t.Fatalf("expected unknown-kind error, got %v", err)
	}
}
