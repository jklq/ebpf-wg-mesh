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
			// The first read only resolves the environment to watch; by the time
			// the wait returns, the rollout has completed.
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
