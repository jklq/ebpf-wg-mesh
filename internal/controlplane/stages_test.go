package controlplane

import (
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
