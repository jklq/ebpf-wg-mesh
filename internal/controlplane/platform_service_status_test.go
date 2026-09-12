package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"sync/atomic"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestPlatformServiceGetServiceStatusRereadsAfterWait(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	var reads atomic.Int32
	service := NewPlatformService(&fakePlatformStore{
		serviceStatusFn: func(ctx context.Context, userID, serviceID string) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, error) {
			applied := int64(1)
			healthy := false
			if reads.Add(1) > 1 {
				applied = 2
				healthy = true
			}
			return deliverycore.ServiceRecord{
				ID:               serviceID,
				ProjectID:        "project-1",
				AllocatedAgentID: "node-1",
				CreatedAt:        now.Add(-10 * time.Minute),
				Spec:             directImageServiceSpec("nginx:1.27", nil),
				LatestBuild: &platformv1.BuildStatus{
					BuildId: "build-1",
				},
			}, []deliverycore.AllocationRecord{{
				ID:                       "alloc-status",
				ServiceID:                serviceID,
				AgentID:                  "node-1",
				DesiredRolloutGeneration: 2,
				AppliedRolloutGeneration: applied,
				Healthy:                  healthy,
				UpdatedAt:                now,
			}}, nil
		},
	}, noopNotifier{}, noopIngress{}, nil)

	resp, err := service.GetServiceStatus(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.GetServiceStatusRequest{
		ServiceId: "service-1",
	})
	if err != nil {
		t.Fatalf("GetServiceStatus: %v", err)
	}
	if got := resp.GetAllocation().GetAppliedRolloutGeneration(); got != 2 {
		t.Fatalf("expected the post-wait snapshot, got applied generation %d", got)
	}
	if !resp.GetAllocation().GetHealthy() {
		t.Fatalf("expected the post-wait snapshot to report healthy")
	}
}

func TestListServicesDecoratesFromSingleLiveAllocationSnapshot(t *testing.T) {
	t.Parallel()

	var allocationReads atomic.Int32
	store := &fakePlatformStore{
		listServicesFn: func(context.Context, string, string) ([]deliverycore.ServiceRecord, error) {
			return []deliverycore.ServiceRecord{
				{ID: "service-a", EnvironmentID: "environment-1", Spec: directImageServiceSpec("nginx:1.27", nil)},
				{ID: "service-b", EnvironmentID: "environment-1", Spec: directImageServiceSpec("nginx:1.27", nil)},
			}, nil
		},
		listAllocationsByServiceIDFn: func(context.Context, string) ([]deliverycore.AllocationRecord, error) {
			allocationReads.Add(1)
			return nil, nil
		},
	}
	delivery := &fakePlatformDelivery{liveAllocationsFn: func(environmentID string) (map[string][]deliverycore.AllocationRecord, error) {
		if environmentID != "environment-1" {
			t.Fatalf("environment = %q", environmentID)
		}
		return map[string][]deliverycore.AllocationRecord{
			"service-a": {{
				Healthy: true, AllocationIPv4: "10.0.0.1", AllocationIPv6: "fd00::1",
				DesiredSpecRevision: 1, AppliedSpecRevision: 1,
				DesiredRolloutGeneration: 1, AppliedRolloutGeneration: 1,
			}},
		}, nil
	}}
	service := NewPlatformService(store, noopNotifier{}, noopIngress{}, delivery)

	resp, err := service.ListServices(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.ListServicesRequest{EnvironmentId: "environment-1"})
	if err != nil {
		t.Fatal(err)
	}
	if allocationReads.Load() != 0 {
		t.Fatalf("per-service allocation reads = %d, want 0", allocationReads.Load())
	}
	if len(resp.GetServices()) != 2 || resp.GetServices()[0].GetReadyReplicaCount() != 1 || resp.GetServices()[1].GetReadyReplicaCount() != 0 {
		t.Fatalf("decorated services = %#v", resp.GetServices())
	}
}
