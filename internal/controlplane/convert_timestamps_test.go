package controlplane

import (
	"testing"
	"time"

	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

func TestDeploymentBuildAllocationTimestampsUseAbsentUTC(t *testing.T) {
	moment := time.Date(2026, 8, 13, 10, 0, 22, 0, time.FixedZone("CET", 3600))

	t.Run("allocation without observation stays absent", func(t *testing.T) {
		got := toProtoAllocation(deliverycore.AllocationRecord{ID: "alloc-1", ServiceID: "svc-1"})
		if got.UpdatedAt != nil {
			t.Fatalf("allocation updated_at = %v, want nil", got.UpdatedAt)
		}
	})

	t.Run("allocation instant round-trips in UTC", func(t *testing.T) {
		got := toProtoAllocation(deliverycore.AllocationRecord{ID: "alloc-1", UpdatedAt: moment})
		if got.UpdatedAt == nil {
			t.Fatal("allocation updated_at is nil")
		}
		if !got.UpdatedAt.AsTime().Equal(moment) {
			t.Fatalf("allocation updated_at = %v, want %v", got.UpdatedAt.AsTime(), moment)
		}
	})

	t.Run("deployment without transition stays absent", func(t *testing.T) {
		got := toProtoDeploymentStatus(&deliverycore.DeploymentRecord{ID: "dep-1", State: deliverycore.DeploymentStateStaged})
		if got == nil {
			t.Fatal("deployment status is nil")
		}
		if got.TransitionedAt != nil {
			t.Fatalf("transitioned_at = %v, want nil", got.TransitionedAt)
		}
	})

	t.Run("deployment transition round-trips in UTC", func(t *testing.T) {
		got := toProtoDeploymentStatus(&deliverycore.DeploymentRecord{
			ID: "dep-1", State: deliverycore.DeploymentStateBuilding, UpdatedAt: moment,
		})
		if got.TransitionedAt == nil || !got.TransitionedAt.AsTime().Equal(moment) {
			t.Fatalf("transitioned_at = %v, want %v", got.TransitionedAt, moment)
		}
	})

	t.Run("unknown agent heartbeat stays absent", func(t *testing.T) {
		got := toProtoAgent(deliverycore.AgentRecord{ID: "agent", Name: "agent", LifecycleState: deliverycore.AgentStateActive})
		if got.LastSeenAt != nil {
			t.Fatalf("last_seen_at = %v, want nil", got.LastSeenAt)
		}
		if got.Healthy {
			t.Fatal("agent with unknown heartbeat reports healthy")
		}
	})
}
