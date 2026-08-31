package controlplane

import (
	"errors"
	"fmt"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

const (
	deploymentStateStaged      = "staged"
	deploymentStateQueuedBuild = "queued_build"
	deploymentStateBuilding    = "building"
	deploymentStateScheduling  = "scheduling"
	deploymentStateImagePull   = "image_pull"
	deploymentStateStarting    = "starting"
	deploymentStateReadiness   = "readiness"
	deploymentStateActive      = "active"
	deploymentStateDraining    = "draining"
	deploymentStateCompleted   = "completed"
	deploymentStateFailed      = "failed"
	deploymentStateCancelled   = "cancelled"
	deploymentStateCrashed     = "crashed"
	deploymentStateRemoved     = "removed"
	deploymentStateSuperseded  = "superseded"
)

const (
	deploymentCauseUser    = "user"
	deploymentCauseSystem  = "system"
	deploymentCauseAgent   = "agent"
	deploymentCauseBuilder = "builder"
	deploymentCauseWebhook = "webhook"
)

const (
	reasonServiceStaged        = "SERVICE_STAGED"
	reasonServiceCreated       = "SERVICE_CREATED"
	reasonBuildQueued          = "BUILD_QUEUED"
	reasonBuildStarted         = "BUILD_STARTED"
	reasonBuildSucceeded       = "BUILD_SUCCEEDED"
	reasonBuildFailed          = "BUILD_FAILED"
	reasonBuildSuperseded      = "BUILD_SUPERSEDED"
	reasonBuildRequeued        = "BUILD_REQUEUED"
	reasonRolloutScheduled     = "ROLLOUT_SCHEDULED"
	reasonImagePulling         = "IMAGE_PULLING"
	reasonContainerStarting    = "CONTAINER_STARTING"
	reasonReadinessWaiting     = "READINESS_WAITING"
	reasonReadinessPassed      = "READINESS_PASSED"
	reasonDeploymentActive     = "DEPLOYMENT_ACTIVE"
	reasonDeploymentDraining   = "DEPLOYMENT_DRAINING"
	reasonDeploymentCompleted  = "DEPLOYMENT_COMPLETED"
	reasonDeploymentFailed     = "DEPLOYMENT_FAILED"
	reasonDeploymentCancelled  = "DEPLOYMENT_CANCELLED"
	reasonDeploymentCrashed    = "DEPLOYMENT_CRASHED"
	reasonDeploymentRemoved    = "DEPLOYMENT_REMOVED"
	reasonDeploymentSuperseded = "DEPLOYMENT_SUPERSEDED"
	reasonAgentObservation     = "AGENT_OBSERVATION"
	reasonUserRedeploy         = "USER_REDEPLOY"
	reasonExactRedeploy        = "EXACT_REDEPLOY"
	reasonRollback             = "ROLLBACK"
	reasonUserRetry            = "USER_RETRY"
	reasonOperatorRestart      = "OPERATOR_RESTART"
	reasonUserCancel           = "USER_CANCEL"
	reasonUserRemove           = "USER_REMOVE"
	reasonWebhookPush          = "WEBHOOK_PUSH"
	reasonFailoverRescheduled  = "FAILOVER_RESCHEDULED"
	reasonManagedSync          = "MANAGED_SYNC"
)

var (
	errIllegalDeploymentTransition = errors.New("illegal deployment transition")
	errDeploymentTerminal          = errors.New("deployment is terminal")
)

type deploymentActor struct {
	Kind string
	ID   string
}

type deploymentTransitionInput struct {
	ToState           string
	Actor             deploymentActor
	ReasonCode        string
	Detail            string
	SpecRevision      int64
	ImageDigest       string
	RolloutGeneration int64
	BuildID           string
	HasSpecRevision   bool
	HasImageDigest    bool
	HasRollout        bool
	HasBuildID        bool
	IgnoreIfTerminal  bool
}

func deploymentStateTerminal(state string) bool {
	switch state {
	case deploymentStateCompleted, deploymentStateFailed, deploymentStateCancelled, deploymentStateCrashed, deploymentStateRemoved, deploymentStateSuperseded:
		return true
	default:
		return false
	}
}

func deploymentStateInProgress(state string) bool {
	switch state {
	case deploymentStateStaged, deploymentStateQueuedBuild, deploymentStateBuilding, deploymentStateScheduling, deploymentStateImagePull, deploymentStateStarting, deploymentStateReadiness, deploymentStateDraining:
		return true
	default:
		return false
	}
}

func deploymentStatePreActive(state string) bool {
	switch state {
	case deploymentStateStaged, deploymentStateQueuedBuild, deploymentStateBuilding, deploymentStateScheduling, deploymentStateImagePull, deploymentStateStarting, deploymentStateReadiness:
		return true
	default:
		return false
	}
}

func sanitizeDeploymentDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return ""
	}
	const maxDetail = 512
	if len(detail) > maxDetail {
		return detail[:maxDetail]
	}
	return detail
}

func normalizeDeploymentCauseKind(kind string) string {
	switch strings.TrimSpace(kind) {
	case deploymentCauseUser, deploymentCauseSystem, deploymentCauseAgent, deploymentCauseBuilder, deploymentCauseWebhook:
		return kind
	default:
		return deploymentCauseSystem
	}
}

func deploymentTransitionAllowed(from, to string) bool {
	if from == "" || to == "" {
		return false
	}
	if from == to {
		return true
	}
	if deploymentStateTerminal(from) {
		return false
	}
	switch to {
	case deploymentStateFailed, deploymentStateCancelled, deploymentStateRemoved, deploymentStateSuperseded:
		return true
	case deploymentStateCrashed:
		switch from {
		case deploymentStateStarting, deploymentStateReadiness, deploymentStateActive, deploymentStateDraining:
			return true
		default:
			return false
		}
	}

	if from == deploymentStateBuilding && to == deploymentStateQueuedBuild {
		return true
	}
	if from == deploymentStateActive && (to == deploymentStateScheduling || to == deploymentStateImagePull) {
		return true
	}
	if from == deploymentStateStaged && (to == deploymentStateQueuedBuild || to == deploymentStateScheduling) {
		return true
	}
	if from == deploymentStateQueuedBuild && to == deploymentStateBuilding {
		return true
	}
	if from == deploymentStateBuilding && to == deploymentStateScheduling {
		return true
	}

	postSchedule := []string{
		deploymentStateScheduling,
		deploymentStateImagePull,
		deploymentStateStarting,
		deploymentStateReadiness,
		deploymentStateActive,
		deploymentStateDraining,
	}
	fromIdx := indexOfString(postSchedule, from)
	toIdx := indexOfString(postSchedule, to)
	if fromIdx >= 0 && toIdx > fromIdx {
		return true
	}
	return from == deploymentStateDraining && to == deploymentStateCompleted
}

func indexOfString(values []string, want string) int {
	for i, value := range values {
		if value == want {
			return i
		}
	}
	return -1
}

func toProtoDeploymentState(state string) platformv1.DeploymentState {
	switch state {
	case deploymentStateStaged:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_STAGED
	case deploymentStateQueuedBuild:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_QUEUED_BUILD
	case deploymentStateBuilding:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_BUILDING
	case deploymentStateScheduling:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_SCHEDULING
	case deploymentStateImagePull:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_IMAGE_PULL
	case deploymentStateStarting:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_STARTING
	case deploymentStateReadiness:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_READINESS
	case deploymentStateActive:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_ACTIVE
	case deploymentStateDraining:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_DRAINING
	case deploymentStateCompleted:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_COMPLETED
	case deploymentStateFailed:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_FAILED
	case deploymentStateCancelled:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_CANCELLED
	case deploymentStateCrashed:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_CRASHED
	case deploymentStateRemoved:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_REMOVED
	case deploymentStateSuperseded:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_SUPERSEDED
	default:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_UNSPECIFIED
	}
}

func toProtoDeploymentCauseKind(kind string) platformv1.DeploymentCauseKind {
	switch kind {
	case deploymentCauseUser:
		return platformv1.DeploymentCauseKind_DEPLOYMENT_CAUSE_KIND_USER
	case deploymentCauseSystem:
		return platformv1.DeploymentCauseKind_DEPLOYMENT_CAUSE_KIND_SYSTEM
	case deploymentCauseAgent:
		return platformv1.DeploymentCauseKind_DEPLOYMENT_CAUSE_KIND_AGENT
	case deploymentCauseBuilder:
		return platformv1.DeploymentCauseKind_DEPLOYMENT_CAUSE_KIND_BUILDER
	case deploymentCauseWebhook:
		return platformv1.DeploymentCauseKind_DEPLOYMENT_CAUSE_KIND_WEBHOOK
	default:
		return platformv1.DeploymentCauseKind_DEPLOYMENT_CAUSE_KIND_UNSPECIFIED
	}
}

func agentObservedDeploymentState(phase string, healthy bool, applied, desired int64) (string, bool) {
	phase = strings.TrimSpace(phase)
	if healthy || strings.EqualFold(phase, "Healthy") {
		return deploymentStateActive, true
	}
	switch {
	case strings.EqualFold(phase, "Error"), strings.EqualFold(phase, "Failed"), strings.EqualFold(phase, "Unhealthy"):
		return deploymentStateFailed, true
	case strings.EqualFold(phase, "Starting"):
		if desired > 0 && applied >= desired {
			return deploymentStateReadiness, true
		}
		return deploymentStateStarting, true
	case strings.EqualFold(phase, "Ready"):
		return deploymentStateReadiness, true
	case strings.EqualFold(phase, "Pending"):
		if desired > 0 && applied < desired {
			return deploymentStateImagePull, true
		}
		return deploymentStateScheduling, true
	default:
		return "", false
	}
}

func resolveAgentTargetState(current, observed string) string {
	if observed == deploymentStateFailed {
		switch current {
		case deploymentStateStarting, deploymentStateReadiness, deploymentStateActive, deploymentStateDraining:
			return deploymentStateCrashed
		}
	}
	return observed
}

func reasonCodeForState(state string) string {
	switch state {
	case deploymentStateStaged:
		return reasonServiceStaged
	case deploymentStateQueuedBuild:
		return reasonBuildQueued
	case deploymentStateBuilding:
		return reasonBuildStarted
	case deploymentStateScheduling:
		return reasonRolloutScheduled
	case deploymentStateImagePull:
		return reasonImagePulling
	case deploymentStateStarting:
		return reasonContainerStarting
	case deploymentStateReadiness:
		return reasonReadinessWaiting
	case deploymentStateActive:
		return reasonDeploymentActive
	case deploymentStateDraining:
		return reasonDeploymentDraining
	case deploymentStateCompleted:
		return reasonDeploymentCompleted
	case deploymentStateFailed:
		return reasonDeploymentFailed
	case deploymentStateCancelled:
		return reasonDeploymentCancelled
	case deploymentStateCrashed:
		return reasonDeploymentCrashed
	case deploymentStateRemoved:
		return reasonDeploymentRemoved
	case deploymentStateSuperseded:
		return reasonDeploymentSuperseded
	default:
		return "DEPLOYMENT_STATE_CHANGED"
	}
}

func defaultDetailForState(state string) string {
	switch state {
	case deploymentStateStaged:
		return "Configuration staged"
	case deploymentStateQueuedBuild:
		return "Build queued"
	case deploymentStateBuilding:
		return "Building image"
	case deploymentStateScheduling:
		return "Scheduling rollout"
	case deploymentStateImagePull:
		return "Pulling image"
	case deploymentStateStarting:
		return "Starting container"
	case deploymentStateReadiness:
		return "Waiting for readiness"
	case deploymentStateActive:
		return "Serving traffic"
	case deploymentStateDraining:
		return "Draining traffic"
	case deploymentStateCompleted:
		return "Deployment completed"
	case deploymentStateFailed:
		return "Deployment failed"
	case deploymentStateCancelled:
		return "Deployment cancelled"
	case deploymentStateCrashed:
		return "Workload crashed"
	case deploymentStateRemoved:
		return "Deployment removed"
	case deploymentStateSuperseded:
		return "Superseded by a newer deployment"
	default:
		return ""
	}
}

func illegalTransitionError(from, to string) error {
	return fmt.Errorf("%w: %s -> %s", errIllegalDeploymentTransition, from, to)
}
