package controlplane

import (
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

// projectDeploymentStages synthesizes the deployment timeline that
// the web console renders in the right-hand panel. We derive it from the
// build run + allocation records instead of tracking an explicit state
// machine: the inputs already express the ground truth (build state, phase,
// rollout-generation parity) and projecting them keeps the read side free of
// drift that a separate store would otherwise introduce.
//
// The canonical stages, in order, are:
//
//  1. Initialization — the service row exists and has been scheduled.
//  2. Build          — the most recent build run, if any.
//  3. Deploy         — the agent is applying the new rollout generation.
//  4. Post-deploy    — the applied rollout is healthy and serving traffic.
//
// Build-driven source deployments (for example webhook-triggered rebuilds)
// start at Build because the service has already been initialized in an
// earlier rollout. Direct-image services still include Build as SKIPPED so the
// UI can keep a consistent shape without pretending a build ran.
func projectDeploymentStages(service serviceRecord, build *buildRunRecord, alloc allocationRecord) []*platformv1.DeploymentStage {
	stages := make([]*platformv1.DeploymentStage, 0, 4)
	if includeInitializationStage(service, build) {
		stages = append(stages, initializationStage(service))
	}
	stages = append(stages, buildStage(service, build))
	stages = append(stages, deployStage(service, build, alloc))
	stages = append(stages, postDeployStage(service, build, alloc))
	return stages
}

func includeInitializationStage(service serviceRecord, build *buildRunRecord) bool {
	return !(desiredSourceSpec(service.Spec) != nil && build != nil)
}

func initializationStage(service serviceRecord) *platformv1.DeploymentStage {
	stage := &platformv1.DeploymentStage{
		Key:       StageInitialization,
		Label:     "Initialization",
		State:     platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SUCCEEDED,
		StartedAt: ts(service.CreatedAt),
		// Initialization is essentially instantaneous from the UI's point of
		// view — as soon as the service row exists, scheduling is done — so
		// we clamp finished_at to created_at to show a zero-duration success.
		FinishedAt: ts(service.CreatedAt),
	}
	if service.AllocatedAgentID == "" {
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = "Selecting a host"
		stage.FinishedAt = nil
	} else {
		stage.Detail = "Service scheduled on " + service.AllocatedAgentID
	}
	return stage
}

func buildStage(service serviceRecord, build *buildRunRecord) *platformv1.DeploymentStage {
	stage := &platformv1.DeploymentStage{
		Key:   StageBuild,
		Label: "Build",
	}
	// If the service uses a direct image we skip the build step entirely.
	// We still emit the stage so the UI can render a consistent shape, but
	// it is marked SKIPPED and has no timestamps.
	if desiredSourceSpec(service.Spec) == nil && build == nil {
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SKIPPED
		stage.Detail = "No build required — using prebuilt image"
		return stage
	}
	if build == nil {
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = "Waiting for source"
		return stage
	}
	stage.StartedAt = ts(build.QueuedAt)
	if build.StartedAt.Valid {
		stage.StartedAt = ts(build.StartedAt.Time)
	}
	if build.FinishedAt.Valid {
		stage.FinishedAt = ts(build.FinishedAt.Time)
	}
	switch build.State {
	case buildStateQueued:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = "Waiting for a builder"
	case buildStateRunning:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = "Building the image…"
	case buildStateSucceeded:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SUCCEEDED
		stage.Detail = "Image ready"
	case buildStateFailed:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = firstNonEmpty(build.FailureReason, "Build failed")
	case buildStateSuperseded:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SKIPPED
		stage.Detail = "Superseded by a newer build"
	default:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_UNSPECIFIED
	}
	return stage
}

func deployStage(service serviceRecord, build *buildRunRecord, alloc allocationRecord) *platformv1.DeploymentStage {
	stage := &platformv1.DeploymentStage{
		Key:   StageDeploy,
		Label: "Deploy",
	}
	// Deploy cannot start until the build stage has produced an image. For
	// direct-image services, that's immediate; for source services we wait
	// for the build to succeed.
	if desiredSourceSpec(service.Spec) != nil {
		if build == nil || build.State != buildStateSucceeded {
			stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
			stage.Detail = "Waiting for build to finish"
			return stage
		}
		stage.StartedAt = ts(build.FinishedAt.Time)
	} else {
		stage.StartedAt = ts(service.CreatedAt)
	}
	if alloc.ID == "" {
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = "Waiting for scheduler"
		return stage
	}
	// A deploy is "in progress" whenever the agent has not yet applied the
	// desired rollout generation, regardless of the reported phase string.
	// This makes the stage monotonic: the applied generation only catches up
	// once the workload is actually running.
	if alloc.AppliedRolloutGeneration < alloc.DesiredRolloutGeneration {
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = firstNonEmpty(alloc.Message, "Rolling out to "+alloc.AgentID)
		return stage
	}
	switch alloc.Phase {
	case "Failed", "Unhealthy":
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = firstNonEmpty(alloc.Message, "Deploy failed")
	case "Pending", "":
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = firstNonEmpty(alloc.Message, "Queued for rollout")
	default:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SUCCEEDED
		stage.Detail = "Running on " + alloc.AgentID
		stage.FinishedAt = ts(alloc.UpdatedAt)
	}
	return stage
}

func postDeployStage(service serviceRecord, build *buildRunRecord, alloc allocationRecord) *platformv1.DeploymentStage {
	stage := &platformv1.DeploymentStage{
		Key:   StagePostDeploy,
		Label: "Post-deploy",
	}
	if desiredSourceSpec(service.Spec) != nil && build != nil && build.State != buildStateSucceeded {
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = "Waiting for build to finish"
		return stage
	}
	if alloc.ID == "" || alloc.AppliedRolloutGeneration < alloc.DesiredRolloutGeneration {
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = "Waiting for rollout"
		return stage
	}
	if alloc.Healthy {
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SUCCEEDED
		stage.Detail = "Health checks passing"
		stage.StartedAt = ts(alloc.UpdatedAt)
		stage.FinishedAt = ts(alloc.UpdatedAt)
		return stage
	}
	// When the rollout is applied but health checks have not reported a
	// green yet we stay in running. If the phase explicitly says Failed we
	// surface the failure here too so the user sees "crash looping" style
	// errors without drilling into the deploy stage again.
	switch alloc.Phase {
	case "Failed", "Unhealthy":
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = firstNonEmpty(alloc.Message, "Workload unhealthy")
	default:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = "Waiting for health checks"
	}
	stage.StartedAt = ts(alloc.UpdatedAt)
	return stage
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
