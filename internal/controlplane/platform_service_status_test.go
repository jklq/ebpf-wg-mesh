package controlplane

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestPlatformServiceGetServiceStatusProjectsStagesFromReturnedAllocation(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	service := NewPlatformService(&fakePlatformStore{
		serviceStatusFn: func(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, []allocationRecord, error) {
			return serviceRecord{
					ID:               serviceID,
					ProjectID:        projectID,
					AllocatedAgentID: "node-1",
					CreatedAt:        now.Add(-10 * time.Minute),
					Spec:             directImageServiceSpec("nginx:1.27", nil),
					LatestBuild: &platformv1.BuildStatus{
						BuildId: "build-1",
					},
				}, []allocationRecord{{
					ID:                       "alloc-status",
					ServiceID:                serviceID,
					AgentID:                  "node-1",
					DesiredRolloutGeneration: 2,
					AppliedRolloutGeneration: 1,
					Healthy:                  false,
					UpdatedAt:                now,
				}}, nil
		},
		allocationByServiceIDFn: func(ctx context.Context, serviceID string) (allocationRecord, error) {
			return allocationRecord{
				ID:                       "alloc-newer",
				ServiceID:                serviceID,
				AgentID:                  "node-1",
				DesiredRolloutGeneration: 2,
				AppliedRolloutGeneration: 2,
				Healthy:                  true,
				UpdatedAt:                now,
			}, nil
		},
	}, noopNotifier{}, noopIngress{})

	resp, err := service.GetServiceStatus(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.GetServiceStatusRequest{
		ServiceId: "service-1",
	})
	if err != nil {
		t.Fatalf("GetServiceStatus: %v", err)
	}
	if got := resp.GetAllocation().GetAppliedRolloutGeneration(); got != 1 {
		t.Fatalf("expected returned allocation snapshot to stay stale for test, got %d", got)
	}
	stages := resp.GetService().GetLatestBuild().GetStages()
	if len(stages) == 0 {
		t.Fatalf("expected projected stages")
	}
	postDeploy := stages[len(stages)-1]
	if postDeploy.GetKey() != StagePostDeploy {
		t.Fatalf("expected last stage post-deploy, got %q", postDeploy.GetKey())
	}
	if postDeploy.GetState() != platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING {
		t.Fatalf("expected post-deploy to use returned allocation snapshot, got %v", postDeploy.GetState())
	}
}

func TestPlatformServiceGetServiceStatusRereadsAfterWait(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	var reads atomic.Int32
	service := NewPlatformService(&fakePlatformStore{
		serviceStatusFn: func(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, []allocationRecord, error) {
			// The first read only resolves the environment to watch; by the time
			// the wait returns, the rollout has completed.
			applied := int64(1)
			healthy := false
			if reads.Add(1) > 1 {
				applied = 2
				healthy = true
			}
			return serviceRecord{
					ID:               serviceID,
					ProjectID:        projectID,
					AllocatedAgentID: "node-1",
					CreatedAt:        now.Add(-10 * time.Minute),
					Spec:             directImageServiceSpec("nginx:1.27", nil),
					LatestBuild: &platformv1.BuildStatus{
						BuildId: "build-1",
					},
				}, []allocationRecord{{
					ID:                       "alloc-status",
					ServiceID:                serviceID,
					AgentID:                  "node-1",
					DesiredRolloutGeneration: 2,
					AppliedRolloutGeneration: applied,
					Healthy:                  healthy,
					UpdatedAt:                now,
				}}, nil
		},
	}, noopNotifier{}, noopIngress{})

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
