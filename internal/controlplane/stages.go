package controlplane

import (
	"fmt"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/restartpolicy"
)

// deploymentStages projects the persisted lifecycle onto the four-stage
// console timeline. The deployment row is the source of truth; build and
// allocation records only supply timestamps and detail when the lifecycle
// has not recorded them yet.
func deploymentStages(service serviceRecord, build *buildRunRecord, alloc allocationRecord) []*platformv1.DeploymentStage {
	if service.LatestDeployment != nil {
		return deploymentStagesFromLifecycle(*service.LatestDeployment, service, build)
	}
	return deploymentStagesFromLifecycle(inferDeploymentFromLegacy(service, build, alloc), service, build)
}

func deploymentStagesFromLifecycle(dep deploymentRecord, service serviceRecord, build *buildRunRecord) []*platformv1.DeploymentStage {
	stages := make([]*platformv1.DeploymentStage, 0, 4)
	if includeInitializationStage(service, build) && dep.State != deploymentStateQueuedBuild && dep.State != deploymentStateBuilding {
		stages = append(stages, lifecycleInitializationStage(dep, service))
	}
	stages = append(stages, lifecycleBuildStage(dep, service, build))
	stages = append(stages, lifecycleDeployStage(dep, service, build))
	stages = append(stages, lifecyclePostDeployStage(dep, build))
	return stages
}

func summarizeAllocations(allocs []allocationRecord, desired int32) allocationRecord {
	if len(allocs) == 0 {
		return allocationRecord{}
	}
	summary := allocs[0]
	ready := 0
	unapplied := 0
	failed := 0
	for _, alloc := range allocs {
		if allocationReady(alloc) {
			ready++
		}
		if alloc.AppliedRolloutGeneration < alloc.DesiredRolloutGeneration || alloc.AppliedSpecRevision < alloc.DesiredSpecRevision {
			unapplied++
			if summary.AppliedRolloutGeneration >= summary.DesiredRolloutGeneration {
				summary = alloc
			}
		}
		switch alloc.Phase {
		case "Error", "Failed", "Unhealthy", allocationPhaseUnavailable:
			failed++
			if allocationReady(summary) {
				summary = alloc
			}
		}
	}
	if desired <= 0 {
		desired = int32(len(allocs))
	}
	switch {
	case unapplied > 0:
		summary.Message = firstNonEmpty(summary.Message, fmt.Sprintf("Rolling out %d of %d replicas", int32(len(allocs))-int32(unapplied), desired))
	case ready == int(desired) && desired > 0:
		summary.Healthy = true
		summary.Message = firstNonEmpty(summary.Message, fmt.Sprintf("%d of %d replicas ready", ready, desired))
	case failed > 0:
		summary.Message = firstNonEmpty(summary.Message, fmt.Sprintf("%d of %d replicas ready", ready, desired))
	default:
		summary.Message = firstNonEmpty(summary.Message, fmt.Sprintf("%d of %d replicas ready", ready, desired))
	}
	return summary
}

func inferDeploymentFromLegacy(service serviceRecord, build *buildRunRecord, alloc allocationRecord) deploymentRecord {
	rec := deploymentRecord{
		ServiceID:         service.ID,
		SpecRevision:      service.SpecRevision,
		RolloutGeneration: service.RolloutGeneration,
		CreatedAt:         service.CreatedAt,
		UpdatedAt:         service.UpdatedAt,
		State:             deploymentStateStaged,
		Detail:            defaultDetailForState(deploymentStateStaged),
	}
	if alloc.DesiredRolloutGeneration > 0 {
		rec.RolloutGeneration = alloc.DesiredRolloutGeneration
	}
	switch {
	case build != nil && build.State == buildStateQueued:
		rec.State = deploymentStateQueuedBuild
	case build != nil && build.State == buildStateRunning:
		rec.State = deploymentStateBuilding
	case build != nil && build.State == buildStateFailed:
		rec.State = deploymentStateFailed
		rec.Detail = firstNonEmpty(build.FailureReason, defaultDetailForState(deploymentStateFailed))
	case build != nil && build.State == buildStateSuperseded:
		rec.State = deploymentStateSuperseded
	case alloc.ID != "" && (alloc.Phase == "Error" || alloc.Phase == "Failed" || alloc.Phase == "Unhealthy"):
		if alloc.AppliedRolloutGeneration >= alloc.DesiredRolloutGeneration && alloc.DesiredRolloutGeneration > 0 {
			rec.State = deploymentStateCrashed
		} else {
			rec.State = deploymentStateFailed
		}
		rec.Detail = firstNonEmpty(alloc.Message, defaultDetailForState(rec.State))
	case alloc.Healthy && alloc.AppliedRolloutGeneration >= alloc.DesiredRolloutGeneration && alloc.DesiredRolloutGeneration > 0:
		rec.State = deploymentStateActive
	case alloc.ID != "" && alloc.AppliedRolloutGeneration >= alloc.DesiredRolloutGeneration && alloc.DesiredRolloutGeneration > 0:
		rec.State = deploymentStateReadiness
		rec.Detail = firstNonEmpty(alloc.Message, defaultDetailForState(deploymentStateReadiness))
	case alloc.ID != "" && alloc.AppliedRolloutGeneration < alloc.DesiredRolloutGeneration:
		if alloc.Phase == "Starting" {
			rec.State = deploymentStateStarting
		} else {
			rec.State = deploymentStateScheduling
		}
		rec.Detail = firstNonEmpty(alloc.Message, defaultDetailForState(rec.State))
	case build != nil && build.State == buildStateSucceeded:
		rec.State = deploymentStateScheduling
	case desiredSourceSpec(service.Spec) == nil && service.AllocatedAgentID != "":
		rec.State = deploymentStateScheduling
	}
	return rec
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
	case "Error", "Failed", "Unhealthy", restartpolicy.PhaseCrashLoop:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = firstNonEmpty(alloc.Message, "Deploy failed")
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
		stage.Detail = firstNonEmpty(alloc.Message, "Deployment ready")
		stage.StartedAt = ts(alloc.UpdatedAt)
		stage.FinishedAt = ts(alloc.UpdatedAt)
		return stage
	}
	// An explicit HTTP check gates readiness during rollout. Once it passes,
	// the agent latches the result and does not continuously monitor it.
	switch alloc.Phase {
	case "Error", "Failed", "Unhealthy", restartpolicy.PhaseCrashLoop:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = firstNonEmpty(alloc.Message, "Workload unhealthy")
	case restartpolicy.PhaseStopped, restartpolicy.PhaseBackoff:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = firstNonEmpty(alloc.Message, alloc.Phase)
	default:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = "Waiting for HTTP readiness check"
	}
	stage.StartedAt = ts(alloc.UpdatedAt)
	return stage
}

func lifecycleInitializationStage(dep deploymentRecord, service serviceRecord) *platformv1.DeploymentStage {
	stage := &platformv1.DeploymentStage{
		Key:       StageInitialization,
		Label:     "Initialization",
		StartedAt: ts(firstTransitionTime(dep, deploymentStateStaged, dep.CreatedAt)),
	}
	switch dep.State {
	case deploymentStateStaged:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = firstNonEmpty(dep.Detail, "Selecting a host")
	case deploymentStateFailed:
		if !passedState(dep, deploymentStateScheduling) {
			stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
			stage.Detail = firstNonEmpty(dep.Detail, "Initialization failed")
			stage.FinishedAt = ts(dep.UpdatedAt)
			break
		}
		fallthrough
	default:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SUCCEEDED
		stage.Detail = firstNonEmpty(detailForState(dep, deploymentStateScheduling), "Service scheduled")
		if service.AllocatedAgentID != "" {
			stage.Detail = "Service scheduled on " + service.AllocatedAgentID
		}
		finished := firstTransitionTime(dep, deploymentStateScheduling, dep.UpdatedAt)
		if finished.IsZero() {
			finished = dep.CreatedAt
		}
		stage.FinishedAt = ts(finished)
	}
	return stage
}

func lifecycleBuildStage(dep deploymentRecord, service serviceRecord, build *buildRunRecord) *platformv1.DeploymentStage {
	stage := &platformv1.DeploymentStage{
		Key:   StageBuild,
		Label: "Build",
	}
	if desiredSourceSpec(service.Spec) == nil && dep.BuildID == "" && build == nil {
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SKIPPED
		stage.Detail = "No build required — using prebuilt image"
		return stage
	}
	started := firstTransitionTime(dep, deploymentStateQueuedBuild, time.Time{})
	if started.IsZero() && build != nil {
		started = build.QueuedAt
		if build.StartedAt.Valid {
			started = build.StartedAt.Time
		}
	}
	if !started.IsZero() {
		stage.StartedAt = ts(started)
	}
	switch {
	case dep.State == deploymentStateQueuedBuild:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = firstNonEmpty(dep.Detail, "Waiting for a builder")
	case dep.State == deploymentStateBuilding:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = firstNonEmpty(dep.Detail, "Building the image…")
	case dep.State == deploymentStateFailed && !passedState(dep, deploymentStateScheduling):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = firstNonEmpty(dep.Detail, buildFailureDetail(build), "Build failed")
		stage.FinishedAt = ts(dep.UpdatedAt)
	case dep.State == deploymentStateSuperseded && !passedState(dep, deploymentStateScheduling):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SKIPPED
		stage.Detail = firstNonEmpty(dep.Detail, "Superseded by a newer build")
		stage.FinishedAt = ts(dep.UpdatedAt)
	case dep.State == deploymentStateCancelled && !passedState(dep, deploymentStateScheduling):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = firstNonEmpty(dep.Detail, "Build cancelled")
		stage.FinishedAt = ts(dep.UpdatedAt)
	case passedState(dep, deploymentStateScheduling) || deploymentProgressAtLeast(dep.State, deploymentStateScheduling):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SUCCEEDED
		stage.Detail = "Image ready"
		finished := firstTransitionTime(dep, deploymentStateScheduling, dep.UpdatedAt)
		if build != nil && build.FinishedAt.Valid {
			finished = build.FinishedAt.Time
		}
		stage.FinishedAt = ts(finished)
	default:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = "Waiting for source"
	}
	return stage
}

func lifecycleDeployStage(dep deploymentRecord, service serviceRecord, build *buildRunRecord) *platformv1.DeploymentStage {
	stage := &platformv1.DeploymentStage{
		Key:   StageDeploy,
		Label: "Deploy",
	}
	if !reachedDeployPhase(dep) {
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = "Waiting for build to finish"
		return stage
	}
	started := firstTransitionTime(dep, deploymentStateScheduling, dep.CreatedAt)
	if build != nil && build.FinishedAt.Valid {
		started = build.FinishedAt.Time
	}
	stage.StartedAt = ts(started)
	switch {
	case dep.State == deploymentStateFailed && !passedState(dep, deploymentStateReadiness):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = firstNonEmpty(dep.Detail, "Deploy failed")
		stage.FinishedAt = ts(dep.UpdatedAt)
	case dep.State == deploymentStateCrashed && !passedState(dep, deploymentStateReadiness):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = firstNonEmpty(dep.Detail, "Deploy failed")
		stage.FinishedAt = ts(dep.UpdatedAt)
	case dep.State == deploymentStateScheduling, dep.State == deploymentStateImagePull, dep.State == deploymentStateStarting:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = firstNonEmpty(dep.Detail, defaultDetailForState(dep.State))
	case deploymentProgressAtLeast(dep.State, deploymentStateReadiness) || passedState(dep, deploymentStateReadiness):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SUCCEEDED
		stage.Detail = firstNonEmpty(detailForState(dep, deploymentStateStarting), "Running")
		if service.AllocatedAgentID != "" {
			stage.Detail = "Running on " + service.AllocatedAgentID
		}
		stage.FinishedAt = ts(firstTransitionTime(dep, deploymentStateReadiness, dep.UpdatedAt))
	default:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = "Waiting for scheduler"
	}
	return stage
}

func lifecyclePostDeployStage(dep deploymentRecord, _ *buildRunRecord) *platformv1.DeploymentStage {
	stage := &platformv1.DeploymentStage{
		Key:   StagePostDeploy,
		Label: "Post-deploy",
	}
	if !reachedPostDeployPhase(dep) {
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = "Waiting for rollout"
		return stage
	}
	stage.StartedAt = ts(firstTransitionTime(dep, deploymentStateReadiness, dep.UpdatedAt))
	switch dep.State {
	case deploymentStateReadiness:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = firstNonEmpty(dep.Detail, "Waiting for HTTP readiness check")
	case deploymentStateFailed, deploymentStateCrashed:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = firstNonEmpty(dep.Detail, "Workload unhealthy")
		stage.FinishedAt = ts(dep.UpdatedAt)
	case deploymentStateActive, deploymentStateDraining, deploymentStateCompleted:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SUCCEEDED
		stage.Detail = firstNonEmpty(detailForState(dep, deploymentStateActive), "Deployment ready")
		stage.FinishedAt = ts(firstTransitionTime(dep, deploymentStateActive, dep.UpdatedAt))
	case deploymentStateCancelled, deploymentStateRemoved, deploymentStateSuperseded:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SKIPPED
		stage.Detail = firstNonEmpty(dep.Detail, defaultDetailForState(dep.State))
		stage.FinishedAt = ts(dep.UpdatedAt)
	default:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = firstNonEmpty(dep.Detail, "Waiting for HTTP readiness check")
	}
	return stage
}

func passedState(dep deploymentRecord, state string) bool {
	if dep.State == state {
		return true
	}
	for _, transition := range dep.Transitions {
		if transition.ToState == state {
			return true
		}
	}
	return false
}

func firstTransitionTime(dep deploymentRecord, state string, fallback time.Time) time.Time {
	for _, transition := range dep.Transitions {
		if transition.ToState == state {
			return transition.OccurredAt
		}
	}
	if dep.State == state {
		return dep.UpdatedAt
	}
	return fallback
}

func detailForState(dep deploymentRecord, state string) string {
	for _, transition := range dep.Transitions {
		if transition.ToState == state && transition.Detail != "" {
			return transition.Detail
		}
	}
	if dep.State == state {
		return dep.Detail
	}
	return ""
}

func buildFailureDetail(build *buildRunRecord) string {
	if build == nil {
		return ""
	}
	return build.FailureReason
}

func reachedDeployPhase(dep deploymentRecord) bool {
	if passedState(dep, deploymentStateScheduling) || deploymentProgressAtLeast(dep.State, deploymentStateScheduling) {
		return true
	}
	switch dep.State {
	case deploymentStateFailed, deploymentStateCrashed, deploymentStateCancelled, deploymentStateRemoved:
		return dep.RolloutGeneration > 0 && dep.BuildID == ""
	default:
		return false
	}
}

func reachedPostDeployPhase(dep deploymentRecord) bool {
	if passedState(dep, deploymentStateReadiness) || deploymentProgressAtLeast(dep.State, deploymentStateReadiness) {
		return true
	}
	switch dep.State {
	case deploymentStateFailed, deploymentStateCrashed:
		return dep.RolloutGeneration > 0
	default:
		return false
	}
}

func deploymentProgressAtLeast(state, min string) bool {
	order := []string{
		deploymentStateStaged,
		deploymentStateQueuedBuild,
		deploymentStateBuilding,
		deploymentStateScheduling,
		deploymentStateImagePull,
		deploymentStateStarting,
		deploymentStateReadiness,
		deploymentStateActive,
		deploymentStateDraining,
		deploymentStateCompleted,
	}
	stateIdx := indexOfString(order, state)
	minIdx := indexOfString(order, min)
	return stateIdx >= 0 && minIdx >= 0 && stateIdx >= minIdx
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
