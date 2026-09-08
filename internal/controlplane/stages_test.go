package controlplane

import (
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/logs"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestDeploymentStagesOmitsInitializationForBuildDrivenSourceDeployments(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	service := deliverycore.ServiceRecord{
		ID:                "service-1",
		AllocatedAgentID:  "node-1",
		CreatedAt:         now.Add(-10 * time.Minute),
		RolloutGeneration: 2,
		Spec:              repositoryServiceSpec(nil, nil),
		LatestDeployment: &deliverycore.DeploymentRecord{
			ID:                "dep-1",
			ServiceID:         "service-1",
			BuildID:           "build-1",
			RolloutGeneration: 3,
			State:             deliverycore.DeploymentStateBuilding,
			CreatedAt:         now.Add(-2 * time.Minute),
			UpdatedAt:         now,
		},
	}
	build := &deliverycore.BuildRunRecord{
		ID:                      "build-1",
		State:                   deliverycore.BuildStateRunning,
		QueuedAt:                now.Add(-2 * time.Minute),
		TargetRolloutGeneration: 3,
	}

	stages := deploymentStages(service, build)
	if len(stages) != 3 {
		t.Fatalf("expected 3 stages, got %d", len(stages))
	}
	if stages[0].GetKey() != logs.StageBuild {
		t.Fatalf("expected first stage to be build, got %q", stages[0].GetKey())
	}
	if stages[2].GetKey() != logs.StagePostDeploy {
		t.Fatalf("expected last stage to be post-deploy, got %q", stages[2].GetKey())
	}
	if stages[2].GetState() != platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING {
		t.Fatalf("expected post-deploy pending while build is still running, got %v", stages[2].GetState())
	}
	for _, stage := range stages {
		if stage.GetKey() == logs.StageInitialization {
			t.Fatalf("expected initialization stage to be omitted for build-driven source deployment")
		}
	}
}

func TestDeploymentStagesKeepsInitializationForDirectImageDeployments(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	service := deliverycore.ServiceRecord{
		ID:               "service-1",
		AllocatedAgentID: "node-1",
		CreatedAt:        now.Add(-10 * time.Minute),
		Spec:             directImageServiceSpec("nginx:1.27", nil),
		LatestDeployment: &deliverycore.DeploymentRecord{
			ID:                "dep-1",
			ServiceID:         "service-1",
			RolloutGeneration: 1,
			State:             deliverycore.DeploymentStateScheduling,
			CreatedAt:         now.Add(-10 * time.Minute),
			UpdatedAt:         now,
		},
	}

	stages := deploymentStages(service, nil)
	if len(stages) != 4 {
		t.Fatalf("expected 4 stages, got %d", len(stages))
	}
	if stages[0].GetKey() != logs.StageInitialization {
		t.Fatalf("expected first stage to be initialization, got %q", stages[0].GetKey())
	}
}

func TestDeploymentStagesDoesNotRegressDeployAfterRolloutApplied(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	service := deliverycore.ServiceRecord{
		ID:                "service-1",
		AllocatedAgentID:  "node-1",
		CreatedAt:         now.Add(-10 * time.Minute),
		RolloutGeneration: 2,
		Spec:              repositoryServiceSpec(nil, nil),
		LatestDeployment: &deliverycore.DeploymentRecord{
			ID:                "dep-1",
			ServiceID:         "service-1",
			BuildID:           "build-1",
			RolloutGeneration: 3,
			State:             deliverycore.DeploymentStateReadiness,
			CreatedAt:         now.Add(-2 * time.Minute),
			UpdatedAt:         now,
		},
	}
	build := &deliverycore.BuildRunRecord{
		ID:                      "build-1",
		State:                   deliverycore.BuildStateSucceeded,
		QueuedAt:                now.Add(-2 * time.Minute),
		FinishedAt:              sql.NullTime{Time: now.Add(-30 * time.Second), Valid: true},
		TargetRolloutGeneration: 3,
	}

	stages := deploymentStages(service, build)
	deploy := stages[1]
	if deploy.GetKey() != logs.StageDeploy {
		t.Fatalf("expected second stage to be deploy, got %q", deploy.GetKey())
	}
	if deploy.GetState() != platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SUCCEEDED {
		t.Fatalf("expected deploy to stay succeeded once rollout is applied, got %v", deploy.GetState())
	}
	postDeploy := stages[2]
	if postDeploy.GetKey() != logs.StagePostDeploy {
		t.Fatalf("expected third stage to be post-deploy, got %q", postDeploy.GetKey())
	}
	if postDeploy.GetState() != platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING {
		t.Fatalf("expected post-deploy to wait for health checks, got %v", postDeploy.GetState())
	}
}

func TestDeploymentStagesSurfacesFailedHealthProbe(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	stages := deploymentStages(deliverycore.ServiceRecord{
		AllocatedAgentID: "node-1",
		CreatedAt:        now,
		Spec:             directImageServiceSpec("nginx:1.27", nil),
		LatestDeployment: &deliverycore.DeploymentRecord{
			ID:                "dep-1",
			ServiceID:         "service-1",
			RolloutGeneration: 1,
			State:             deliverycore.DeploymentStateFailed,
			Detail:            "health probe failed: HTTP port 8080: status 503",
			CreatedAt:         now,
			UpdatedAt:         now,
		},
	}, nil)
	if got := stages[2]; got.GetState() != platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED || got.GetDetail() != "health probe failed: HTTP port 8080: status 503" {
		t.Fatalf("deploy stage did not surface probe failure: %+v", got)
	}
	if got := stages[3]; got.GetState() != platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED || got.GetDetail() != "health probe failed: HTTP port 8080: status 503" {
		t.Fatalf("post-deploy stage did not surface probe failure: %+v", got)
	}
}
