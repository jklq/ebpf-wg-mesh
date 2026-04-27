package controlplane

import (
	"database/sql"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestProjectDeploymentStagesOmitsInitializationForBuildDrivenSourceDeployments(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	service := serviceRecord{
		ID:                "service-1",
		AllocatedAgentID:  "node-1",
		CreatedAt:         now.Add(-10 * time.Minute),
		RolloutGeneration: 2,
		Spec:              repositoryServiceSpec(nil, nil),
	}
	build := &buildRunRecord{
		ID:                      "build-1",
		State:                   buildStateRunning,
		QueuedAt:                now.Add(-2 * time.Minute),
		TargetRolloutGeneration: 3,
	}
	alloc := allocationRecord{
		ID:                       "alloc-1",
		AgentID:                  "node-1",
		DesiredRolloutGeneration: 3,
		AppliedRolloutGeneration: 2,
		UpdatedAt:                now,
	}

	stages := projectDeploymentStages(service, build, alloc)
	if len(stages) != 3 {
		t.Fatalf("expected 3 stages, got %d", len(stages))
	}
	if stages[0].GetKey() != StageBuild {
		t.Fatalf("expected first stage to be build, got %q", stages[0].GetKey())
	}
	if stages[2].GetKey() != StagePostDeploy {
		t.Fatalf("expected last stage to be post-deploy, got %q", stages[2].GetKey())
	}
	if stages[2].GetState() != platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING {
		t.Fatalf("expected post-deploy pending while build is still running, got %v", stages[2].GetState())
	}
	for _, stage := range stages {
		if stage.GetKey() == StageInitialization {
			t.Fatalf("expected initialization stage to be omitted for build-driven source deployment")
		}
	}
}

func TestProjectDeploymentStagesKeepsInitializationForDirectImageDeployments(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	service := serviceRecord{
		ID:               "service-1",
		AllocatedAgentID: "node-1",
		CreatedAt:        now.Add(-10 * time.Minute),
		Spec:             directImageServiceSpec("nginx:1.27", nil),
	}
	alloc := allocationRecord{
		ID:                       "alloc-1",
		AgentID:                  "node-1",
		DesiredRolloutGeneration: 1,
		AppliedRolloutGeneration: 0,
		UpdatedAt:                now,
	}

	stages := projectDeploymentStages(service, nil, alloc)
	if len(stages) != 4 {
		t.Fatalf("expected 4 stages, got %d", len(stages))
	}
	if stages[0].GetKey() != StageInitialization {
		t.Fatalf("expected first stage to be initialization, got %q", stages[0].GetKey())
	}
}

func TestProjectDeploymentStagesDoesNotRegressDeployAfterRolloutApplied(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	service := serviceRecord{
		ID:                "service-1",
		AllocatedAgentID:  "node-1",
		CreatedAt:         now.Add(-10 * time.Minute),
		RolloutGeneration: 2,
		Spec:              repositoryServiceSpec(nil, nil),
	}
	build := &buildRunRecord{
		ID:                      "build-1",
		State:                   buildStateSucceeded,
		QueuedAt:                now.Add(-2 * time.Minute),
		FinishedAt:              sql.NullTime{Time: now.Add(-30 * time.Second), Valid: true},
		TargetRolloutGeneration: 3,
	}
	alloc := allocationRecord{
		ID:                       "alloc-1",
		AgentID:                  "node-1",
		DesiredRolloutGeneration: 3,
		AppliedRolloutGeneration: 3,
		Phase:                    "Pending",
		UpdatedAt:                now,
	}

	stages := projectDeploymentStages(service, build, alloc)
	deploy := stages[1]
	if deploy.GetKey() != StageDeploy {
		t.Fatalf("expected second stage to be deploy, got %q", deploy.GetKey())
	}
	if deploy.GetState() != platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SUCCEEDED {
		t.Fatalf("expected deploy to stay succeeded once rollout is applied, got %v", deploy.GetState())
	}
	postDeploy := stages[2]
	if postDeploy.GetKey() != StagePostDeploy {
		t.Fatalf("expected third stage to be post-deploy, got %q", postDeploy.GetKey())
	}
	if postDeploy.GetState() != platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING {
		t.Fatalf("expected post-deploy to wait for health checks, got %v", postDeploy.GetState())
	}
}
