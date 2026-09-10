package controlplane

import (
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/source"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

// deploymentStages projects the persisted lifecycle onto the four-stage
// console timeline. The deployment row is the source of truth; build and
// allocation records only supply timestamps and detail when the lifecycle
// has not recorded them yet.
func deploymentStages(service deliverycore.ServiceRecord, build *deliverycore.BuildRunRecord) []*platformv1.DeploymentStage {
	if service.LatestDeployment == nil {
		return nil
	}
	return deploymentStagesFromLifecycle(*service.LatestDeployment, service, build)
}

func deploymentStagesFromLifecycle(dep deliverycore.DeploymentRecord, service deliverycore.ServiceRecord, build *deliverycore.BuildRunRecord) []*platformv1.DeploymentStage {
	stages := make([]*platformv1.DeploymentStage, 0, 4)
	if includeInitializationStage(service, build) && dep.State != deliverycore.DeploymentStateQueuedBuild && dep.State != deliverycore.DeploymentStateBuilding {
		stages = append(stages, lifecycleInitializationStage(dep, service))
	}
	stages = append(stages, lifecycleBuildStage(dep, service, build))
	stages = append(stages, lifecycleDeployStage(dep, service, build))
	stages = append(stages, lifecyclePostDeployStage(dep, build))
	return stages
}

func includeInitializationStage(service deliverycore.ServiceRecord, build *deliverycore.BuildRunRecord) bool {
	return !(source.DesiredSourceSpec(service.Spec) != nil && build != nil)
}

func lifecycleInitializationStage(dep deliverycore.DeploymentRecord, service deliverycore.ServiceRecord) *platformv1.DeploymentStage {
	stage := &platformv1.DeploymentStage{
		Key:       logs.StageInitialization,
		Label:     "Initialization",
		StartedAt: ts(firstTransitionTime(dep, deliverycore.DeploymentStateStaged, dep.CreatedAt)),
	}
	switch dep.State {
	case deliverycore.DeploymentStateStaged:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = deliverycore.FirstNonEmpty(dep.Detail, "Selecting a host")
	case deliverycore.DeploymentStateFailed:
		if !passedState(dep, deliverycore.DeploymentStateScheduling) {
			stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
			stage.Detail = deliverycore.FirstNonEmpty(dep.Detail, "Initialization failed")
			stage.FinishedAt = ts(dep.UpdatedAt)
			break
		}
		fallthrough
	default:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SUCCEEDED
		stage.Detail = deliverycore.FirstNonEmpty(detailForState(dep, deliverycore.DeploymentStateScheduling), "Service scheduled")
		if service.AllocatedAgentID != "" {
			stage.Detail = "Service scheduled on " + service.AllocatedAgentID
		}
		finished := firstTransitionTime(dep, deliverycore.DeploymentStateScheduling, dep.UpdatedAt)
		if finished.IsZero() {
			finished = dep.CreatedAt
		}
		stage.FinishedAt = ts(finished)
	}
	return stage
}

func lifecycleBuildStage(dep deliverycore.DeploymentRecord, service deliverycore.ServiceRecord, build *deliverycore.BuildRunRecord) *platformv1.DeploymentStage {
	stage := &platformv1.DeploymentStage{
		Key:   logs.StageBuild,
		Label: "Build",
	}
	if source.DesiredSourceSpec(service.Spec) == nil && dep.BuildID == "" && build == nil {
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SKIPPED
		stage.Detail = "No build required — using prebuilt image"
		return stage
	}
	started := firstTransitionTime(dep, deliverycore.DeploymentStateQueuedBuild, time.Time{})
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
	case dep.State == deliverycore.DeploymentStateQueuedBuild:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = deliverycore.FirstNonEmpty(dep.Detail, "Waiting for a builder")
	case dep.State == deliverycore.DeploymentStateBuilding:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = deliverycore.FirstNonEmpty(dep.Detail, "Building the image…")
	case dep.State == deliverycore.DeploymentStateFailed && !passedState(dep, deliverycore.DeploymentStateScheduling):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = deliverycore.FirstNonEmpty(dep.Detail, buildFailureDetail(build), "Build failed")
		stage.FinishedAt = ts(dep.UpdatedAt)
	case dep.State == deliverycore.DeploymentStateSuperseded && !passedState(dep, deliverycore.DeploymentStateScheduling):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SKIPPED
		stage.Detail = deliverycore.FirstNonEmpty(dep.Detail, "Superseded by a newer build")
		stage.FinishedAt = ts(dep.UpdatedAt)
	case dep.State == deliverycore.DeploymentStateCancelled && !passedState(dep, deliverycore.DeploymentStateScheduling):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = deliverycore.FirstNonEmpty(dep.Detail, "Build cancelled")
		stage.FinishedAt = ts(dep.UpdatedAt)
	case passedState(dep, deliverycore.DeploymentStateScheduling) || deploymentProgressAtLeast(dep.State, deliverycore.DeploymentStateScheduling):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SUCCEEDED
		stage.Detail = "Image ready"
		finished := firstTransitionTime(dep, deliverycore.DeploymentStateScheduling, dep.UpdatedAt)
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

func lifecycleDeployStage(dep deliverycore.DeploymentRecord, service deliverycore.ServiceRecord, build *deliverycore.BuildRunRecord) *platformv1.DeploymentStage {
	stage := &platformv1.DeploymentStage{
		Key:   logs.StageDeploy,
		Label: "Deploy",
	}
	if !reachedDeployPhase(dep) {
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = "Waiting for build to finish"
		return stage
	}
	started := firstTransitionTime(dep, deliverycore.DeploymentStateScheduling, dep.CreatedAt)
	if build != nil && build.FinishedAt.Valid {
		started = build.FinishedAt.Time
	}
	stage.StartedAt = ts(started)
	switch {
	case dep.State == deliverycore.DeploymentStateFailed && !passedState(dep, deliverycore.DeploymentStateReadiness):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = deliverycore.FirstNonEmpty(dep.Detail, "Deploy failed")
		stage.FinishedAt = ts(dep.UpdatedAt)
	case dep.State == deliverycore.DeploymentStateCrashed && !passedState(dep, deliverycore.DeploymentStateReadiness):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = deliverycore.FirstNonEmpty(dep.Detail, "Deploy failed")
		stage.FinishedAt = ts(dep.UpdatedAt)
	case dep.State == deliverycore.DeploymentStateScheduling, dep.State == deliverycore.DeploymentStateImagePull, dep.State == deliverycore.DeploymentStateStarting:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = deliverycore.FirstNonEmpty(dep.Detail, deliverycore.DefaultDetailForState(dep.State))
	case deploymentProgressAtLeast(dep.State, deliverycore.DeploymentStateReadiness) || passedState(dep, deliverycore.DeploymentStateReadiness):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SUCCEEDED
		stage.Detail = deliverycore.FirstNonEmpty(detailForState(dep, deliverycore.DeploymentStateStarting), "Running")
		if service.AllocatedAgentID != "" {
			stage.Detail = "Running on " + service.AllocatedAgentID
		}
		stage.FinishedAt = ts(firstTransitionTime(dep, deliverycore.DeploymentStateReadiness, dep.UpdatedAt))
	default:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = "Waiting for scheduler"
	}
	return stage
}

func lifecyclePostDeployStage(dep deliverycore.DeploymentRecord, _ *deliverycore.BuildRunRecord) *platformv1.DeploymentStage {
	stage := &platformv1.DeploymentStage{
		Key:   logs.StagePostDeploy,
		Label: "Post-deploy",
	}
	if !reachedPostDeployPhase(dep) {
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = "Waiting for rollout"
		return stage
	}
	stage.StartedAt = ts(firstTransitionTime(dep, deliverycore.DeploymentStateReadiness, dep.UpdatedAt))
	switch dep.State {
	case deliverycore.DeploymentStateReadiness:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = deliverycore.FirstNonEmpty(dep.Detail, "Waiting for HTTP readiness check")
	case deliverycore.DeploymentStateFailed, deliverycore.DeploymentStateCrashed:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = deliverycore.FirstNonEmpty(dep.Detail, "Workload unhealthy")
		stage.FinishedAt = ts(dep.UpdatedAt)
	case deliverycore.DeploymentStateActive, deliverycore.DeploymentStateDraining, deliverycore.DeploymentStateCompleted:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SUCCEEDED
		stage.Detail = deliverycore.FirstNonEmpty(detailForState(dep, deliverycore.DeploymentStateActive), "Deployment ready")
		stage.FinishedAt = ts(firstTransitionTime(dep, deliverycore.DeploymentStateActive, dep.UpdatedAt))
	case deliverycore.DeploymentStateCancelled, deliverycore.DeploymentStateRemoved, deliverycore.DeploymentStateSuperseded:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SKIPPED
		stage.Detail = deliverycore.FirstNonEmpty(dep.Detail, deliverycore.DefaultDetailForState(dep.State))
		stage.FinishedAt = ts(dep.UpdatedAt)
	default:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = deliverycore.FirstNonEmpty(dep.Detail, "Waiting for HTTP readiness check")
	}
	return stage
}

func passedState(dep deliverycore.DeploymentRecord, state string) bool {
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

func firstTransitionTime(dep deliverycore.DeploymentRecord, state string, fallback time.Time) time.Time {
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

func detailForState(dep deliverycore.DeploymentRecord, state string) string {
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

func buildFailureDetail(build *deliverycore.BuildRunRecord) string {
	if build == nil {
		return ""
	}
	return build.FailureReason
}

func reachedDeployPhase(dep deliverycore.DeploymentRecord) bool {
	if passedState(dep, deliverycore.DeploymentStateScheduling) || deploymentProgressAtLeast(dep.State, deliverycore.DeploymentStateScheduling) {
		return true
	}
	switch dep.State {
	case deliverycore.DeploymentStateFailed, deliverycore.DeploymentStateCrashed, deliverycore.DeploymentStateCancelled, deliverycore.DeploymentStateRemoved:
		return dep.RolloutGeneration > 0 && dep.BuildID == ""
	default:
		return false
	}
}

func reachedPostDeployPhase(dep deliverycore.DeploymentRecord) bool {
	if passedState(dep, deliverycore.DeploymentStateReadiness) || deploymentProgressAtLeast(dep.State, deliverycore.DeploymentStateReadiness) {
		return true
	}
	switch dep.State {
	case deliverycore.DeploymentStateFailed, deliverycore.DeploymentStateCrashed:
		return dep.RolloutGeneration > 0
	default:
		return false
	}
}

func deploymentProgressAtLeast(state, min string) bool {
	order := []string{
		deliverycore.DeploymentStateStaged,
		deliverycore.DeploymentStateQueuedBuild,
		deliverycore.DeploymentStateBuilding,
		deliverycore.DeploymentStateScheduling,
		deliverycore.DeploymentStateImagePull,
		deliverycore.DeploymentStateStarting,
		deliverycore.DeploymentStateReadiness,
		deliverycore.DeploymentStateActive,
		deliverycore.DeploymentStateDraining,
		deliverycore.DeploymentStateCompleted,
	}
	stateIdx := deliverycore.IndexOfString(order, state)
	minIdx := deliverycore.IndexOfString(order, min)
	return stateIdx >= 0 && minIdx >= 0 && stateIdx >= minIdx
}
