package delivery

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"ebof-wg-mesh/internal/restartpolicy"
)

const (
	DeploymentStateStaged      = "staged"
	DeploymentStateQueuedBuild = "queued_build"
	DeploymentStateBuilding    = "building"
	DeploymentStateScheduling  = "scheduling"
	DeploymentStateImagePull   = "image_pull"
	DeploymentStateStarting    = "starting"
	DeploymentStateReadiness   = "readiness"
	DeploymentStateActive      = "active"
	DeploymentStateDraining    = "draining"
	DeploymentStateCompleted   = "completed"
	DeploymentStateFailed      = "failed"
	DeploymentStateCancelled   = "cancelled"
	DeploymentStateCrashed     = "crashed"
	DeploymentStateRemoved     = "removed"
	DeploymentStateSuperseded  = "superseded"
)

const (
	DeploymentCauseUser    = "user"
	DeploymentCauseSystem  = "system"
	DeploymentCauseAgent   = "agent"
	DeploymentCauseBuilder = "builder"
	DeploymentCauseWebhook = "webhook"
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
	reasonEnvironmentRelease   = "ENVIRONMENT_RELEASE"
	reasonExactRedeploy        = "EXACT_REDEPLOY"
	reasonRollback             = "ROLLBACK"
	reasonUserRetry            = "USER_RETRY"
	reasonOperatorRestart      = "OPERATOR_RESTART"
	reasonUserCancel           = "USER_CANCEL"
	reasonUserRemove           = "USER_REMOVE"
	reasonWebhookPush          = "WEBHOOK_PUSH"
	reasonFailoverRescheduled  = "FAILOVER_RESCHEDULED"
	reasonAgentDrain           = "AGENT_DRAIN"
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
	case DeploymentStateCompleted, DeploymentStateFailed, DeploymentStateCancelled, DeploymentStateCrashed, DeploymentStateRemoved, DeploymentStateSuperseded:
		return true
	default:
		return false
	}
}

func deploymentStatePreActive(state string) bool {
	switch state {
	case DeploymentStateStaged, DeploymentStateQueuedBuild, DeploymentStateBuilding, DeploymentStateScheduling, DeploymentStateImagePull, DeploymentStateStarting, DeploymentStateReadiness:
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
	case DeploymentCauseUser, DeploymentCauseSystem, DeploymentCauseAgent, DeploymentCauseBuilder, DeploymentCauseWebhook:
		return kind
	default:
		return DeploymentCauseSystem
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
	case DeploymentStateFailed, DeploymentStateCancelled, DeploymentStateRemoved, DeploymentStateSuperseded:
		return true
	case DeploymentStateCrashed:
		switch from {
		case DeploymentStateStarting, DeploymentStateReadiness, DeploymentStateActive, DeploymentStateDraining:
			return true
		default:
			return false
		}
	}

	if from == DeploymentStateBuilding && to == DeploymentStateQueuedBuild {
		return true
	}
	if from == DeploymentStateActive && (to == DeploymentStateScheduling || to == DeploymentStateImagePull) {
		return true
	}
	if from == DeploymentStateStaged && (to == DeploymentStateQueuedBuild || to == DeploymentStateScheduling) {
		return true
	}
	if from == DeploymentStateQueuedBuild && to == DeploymentStateBuilding {
		return true
	}
	if from == DeploymentStateBuilding && to == DeploymentStateScheduling {
		return true
	}

	postSchedule := []string{
		DeploymentStateScheduling,
		DeploymentStateImagePull,
		DeploymentStateStarting,
		DeploymentStateReadiness,
		DeploymentStateActive,
		DeploymentStateDraining,
	}
	fromIdx := IndexOfString(postSchedule, from)
	toIdx := IndexOfString(postSchedule, to)
	if fromIdx >= 0 && toIdx > fromIdx {
		return true
	}
	return from == DeploymentStateDraining && to == DeploymentStateCompleted
}

func IndexOfString(values []string, want string) int {
	for i, value := range values {
		if value == want {
			return i
		}
	}
	return -1
}

func agentObservedDeploymentState(phase string, healthy bool, applied, desired int64) (string, bool) {
	phase = strings.TrimSpace(phase)
	if healthy || strings.EqualFold(phase, "Healthy") {
		return DeploymentStateActive, true
	}
	switch {
	case strings.EqualFold(phase, restartpolicy.PhaseCrashLoop):
		return DeploymentStateCrashed, true
	case strings.EqualFold(phase, restartpolicy.PhaseStopped):
		return DeploymentStateFailed, true
	case strings.EqualFold(phase, restartpolicy.PhaseBackoff):
		return DeploymentStateStarting, true
	case strings.EqualFold(phase, "Error"), strings.EqualFold(phase, "Failed"), strings.EqualFold(phase, "Unhealthy"):
		return DeploymentStateFailed, true
	case strings.EqualFold(phase, "Starting"):
		if desired > 0 && applied >= desired {
			return DeploymentStateReadiness, true
		}
		return DeploymentStateStarting, true
	case strings.EqualFold(phase, "Ready"):
		return DeploymentStateReadiness, true
	case strings.EqualFold(phase, "Pending"):
		if desired > 0 && applied < desired {
			return DeploymentStateImagePull, true
		}
		return DeploymentStateScheduling, true
	default:
		return "", false
	}
}

func resolveAgentTargetState(current, observed string) string {
	if observed == DeploymentStateFailed {
		switch current {
		case DeploymentStateStarting, DeploymentStateReadiness, DeploymentStateActive, DeploymentStateDraining:
			return DeploymentStateCrashed
		}
	}
	return observed
}

func reasonCodeForState(state string) string {
	switch state {
	case DeploymentStateStaged:
		return reasonServiceStaged
	case DeploymentStateQueuedBuild:
		return reasonBuildQueued
	case DeploymentStateBuilding:
		return reasonBuildStarted
	case DeploymentStateScheduling:
		return reasonRolloutScheduled
	case DeploymentStateImagePull:
		return reasonImagePulling
	case DeploymentStateStarting:
		return reasonContainerStarting
	case DeploymentStateReadiness:
		return reasonReadinessWaiting
	case DeploymentStateActive:
		return reasonDeploymentActive
	case DeploymentStateDraining:
		return reasonDeploymentDraining
	case DeploymentStateCompleted:
		return reasonDeploymentCompleted
	case DeploymentStateFailed:
		return reasonDeploymentFailed
	case DeploymentStateCancelled:
		return reasonDeploymentCancelled
	case DeploymentStateCrashed:
		return reasonDeploymentCrashed
	case DeploymentStateRemoved:
		return reasonDeploymentRemoved
	case DeploymentStateSuperseded:
		return reasonDeploymentSuperseded
	default:
		return "DEPLOYMENT_STATE_CHANGED"
	}
}

func DefaultDetailForState(state string) string {
	switch state {
	case DeploymentStateStaged:
		return "Configuration staged"
	case DeploymentStateQueuedBuild:
		return "Build queued"
	case DeploymentStateBuilding:
		return "Building image"
	case DeploymentStateScheduling:
		return "Scheduling rollout"
	case DeploymentStateImagePull:
		return "Pulling image"
	case DeploymentStateStarting:
		return "Starting container"
	case DeploymentStateReadiness:
		return "Waiting for readiness"
	case DeploymentStateActive:
		return "Serving traffic"
	case DeploymentStateDraining:
		return "Draining traffic"
	case DeploymentStateCompleted:
		return "Deployment completed"
	case DeploymentStateFailed:
		return "Deployment failed"
	case DeploymentStateCancelled:
		return "Deployment cancelled"
	case DeploymentStateCrashed:
		return "Workload crashed"
	case DeploymentStateRemoved:
		return "Deployment removed"
	case DeploymentStateSuperseded:
		return "Superseded by a newer deployment"
	default:
		return ""
	}
}

func illegalTransitionError(from, to string) error {
	return fmt.Errorf("%w: %s -> %s", errIllegalDeploymentTransition, from, to)
}

// decideDeploymentTransition applies lifecycle policy to a copy of the locked record.
func decideDeploymentTransition(rec DeploymentRecord, input deploymentTransitionInput, now time.Time) (DeploymentRecord, bool, error) {
	toState := input.ToState
	if toState == "" {
		return rec, false, errors.New("deployment transition target is required")
	}
	if rec.State == toState {
		return rec, false, nil
	}
	if deploymentStateTerminal(rec.State) {
		if input.IgnoreIfTerminal || input.Actor.Kind == DeploymentCauseAgent {
			return rec, false, nil
		}
		return rec, false, fmt.Errorf("%w: %s", errDeploymentTerminal, rec.State)
	}
	if !deploymentTransitionAllowed(rec.State, toState) {
		if input.IgnoreIfTerminal || input.Actor.Kind == DeploymentCauseAgent {
			return rec, false, nil
		}
		return rec, false, illegalTransitionError(rec.State, toState)
	}

	if input.HasSpecRevision {
		rec.SpecRevision = input.SpecRevision
	}
	if input.HasImageDigest {
		rec.ImageDigest = input.ImageDigest
	}
	if input.HasRollout {
		rec.RolloutGeneration = input.RolloutGeneration
	}
	if input.HasBuildID {
		rec.BuildID = input.BuildID
	}
	rec.State = toState
	rec.CauseKind = normalizeDeploymentCauseKind(input.Actor.Kind)
	rec.CauseID = input.Actor.ID
	rec.ReasonCode = FirstNonEmpty(input.ReasonCode, reasonCodeForState(toState))
	rec.Detail = FirstNonEmpty(sanitizeDeploymentDetail(input.Detail), DefaultDetailForState(toState))
	rec.UpdatedAt = now
	rec.Reason = rec.ReasonCode

	return rec, true, nil
}

type deploymentAgentObservation struct {
	Phase, Message, AgentID              string
	Healthy                              bool
	AppliedGeneration, DesiredGeneration int64
}

func decideAgentDeploymentTransition(rec DeploymentRecord, observation deploymentAgentObservation) (deploymentTransitionInput, bool) {
	if deploymentStateTerminal(rec.State) {
		return deploymentTransitionInput{}, false
	}
	// Removal is durable operator intent. A late status from an allocation that
	// is being withdrawn must not turn the current deployment into active or
	// crashed and strand the persisted drain.
	if rec.State == DeploymentStateDraining && rec.ReasonCode == reasonUserRemove {
		return deploymentTransitionInput{}, false
	}
	observed, recognized := agentObservedDeploymentState(observation.Phase, observation.Healthy, observation.AppliedGeneration, observation.DesiredGeneration)
	if !recognized {
		return deploymentTransitionInput{}, false
	}
	target := resolveAgentTargetState(rec.State, observed)
	if rec.State == target {
		return deploymentTransitionInput{}, false
	}
	return deploymentTransitionInput{
		ToState:          target,
		Actor:            deploymentActor{Kind: DeploymentCauseAgent, ID: observation.AgentID},
		ReasonCode:       FirstNonEmpty(reasonCodeForState(target), reasonAgentObservation),
		Detail:           FirstNonEmpty(sanitizeDeploymentDetail(observation.Message), DefaultDetailForState(target)),
		IgnoreIfTerminal: true,
	}, true
}
