package controlplane

import (
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func deploymentStateInProgress(state string) bool {
	switch state {
	case deliverycore.DeploymentStateStaged, deliverycore.DeploymentStateQueuedBuild, deliverycore.DeploymentStateBuilding, deliverycore.DeploymentStateScheduling, deliverycore.DeploymentStateImagePull, deliverycore.DeploymentStateStarting, deliverycore.DeploymentStateReadiness, deliverycore.DeploymentStateDraining:
		return true
	default:
		return false
	}
}

func toProtoDeploymentState(state string) platformv1.DeploymentState {
	switch state {
	case deliverycore.DeploymentStateStaged:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_STAGED
	case deliverycore.DeploymentStateQueuedBuild:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_QUEUED_BUILD
	case deliverycore.DeploymentStateBuilding:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_BUILDING
	case deliverycore.DeploymentStateScheduling:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_SCHEDULING
	case deliverycore.DeploymentStateImagePull:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_IMAGE_PULL
	case deliverycore.DeploymentStateStarting:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_STARTING
	case deliverycore.DeploymentStateReadiness:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_READINESS
	case deliverycore.DeploymentStateActive:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_ACTIVE
	case deliverycore.DeploymentStateDraining:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_DRAINING
	case deliverycore.DeploymentStateCompleted:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_COMPLETED
	case deliverycore.DeploymentStateFailed:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_FAILED
	case deliverycore.DeploymentStateCancelled:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_CANCELLED
	case deliverycore.DeploymentStateCrashed:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_CRASHED
	case deliverycore.DeploymentStateRemoved:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_REMOVED
	case deliverycore.DeploymentStateSuperseded:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_SUPERSEDED
	default:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_UNSPECIFIED
	}
}

func toProtoDeploymentCauseKind(kind string) platformv1.DeploymentCauseKind {
	switch kind {
	case deliverycore.DeploymentCauseUser:
		return platformv1.DeploymentCauseKind_DEPLOYMENT_CAUSE_KIND_USER
	case deliverycore.DeploymentCauseSystem:
		return platformv1.DeploymentCauseKind_DEPLOYMENT_CAUSE_KIND_SYSTEM
	case deliverycore.DeploymentCauseAgent:
		return platformv1.DeploymentCauseKind_DEPLOYMENT_CAUSE_KIND_AGENT
	case deliverycore.DeploymentCauseBuilder:
		return platformv1.DeploymentCauseKind_DEPLOYMENT_CAUSE_KIND_BUILDER
	case deliverycore.DeploymentCauseWebhook:
		return platformv1.DeploymentCauseKind_DEPLOYMENT_CAUSE_KIND_WEBHOOK
	default:
		return platformv1.DeploymentCauseKind_DEPLOYMENT_CAUSE_KIND_UNSPECIFIED
	}
}
