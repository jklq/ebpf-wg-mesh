package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/restartpolicy"

	"github.com/google/uuid"
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
	reasonBuildReused          = "BUILD_REUSED"
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
	ArtifactID        string
	RolloutGeneration int64
	BuildID           string
	HasSpecRevision   bool
	HasArtifactID     bool
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
	fromIdx := slices.Index(postSchedule, from)
	toIdx := slices.Index(postSchedule, to)
	if fromIdx >= 0 && toIdx > fromIdx {
		return true
	}
	return from == DeploymentStateDraining && to == DeploymentStateCompleted
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
	if input.HasArtifactID {
		rec.ArtifactID = input.ArtifactID
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
	rec.ReasonCode = firstNonEmpty(input.ReasonCode, reasonCodeForState(toState))
	rec.Detail = firstNonEmpty(sanitizeDeploymentDetail(input.Detail), DefaultDetailForState(toState))
	rec.UpdatedAt = now

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
		ReasonCode:       firstNonEmpty(reasonCodeForState(target), reasonAgentObservation),
		Detail:           firstNonEmpty(sanitizeDeploymentDetail(observation.Message), DefaultDetailForState(target)),
		IgnoreIfTerminal: true,
	}, true
}

const deploymentSelectColumns = `id, service_id, spec_revision, rollout_generation, build_id, COALESCE(artifact_id, ''),
	        COALESCE((SELECT image_ref FROM build_artifacts WHERE id = deployments.artifact_id), ''),
	        state, cause_kind, cause_id, reason_code, detail, is_current, requested_by_user_id, created_at, updated_at, COALESCE(source_revision_id, '')`

func (s *persistence) lockServiceTx(ctx context.Context, tx *sql.Tx, serviceID string) error {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM services WHERE id = $1 FOR UPDATE`, serviceID).Scan(&id)
	if err != nil {
		return err
	}
	return nil
}

func (s *persistence) currentDeploymentTx(ctx context.Context, tx *sql.Tx, serviceID string) (DeploymentRecord, bool, error) {
	rec, err := s.queryDeploymentRow(ctx, tx,
		`SELECT `+deploymentSelectColumns+`
		   FROM deployments
		  WHERE service_id = $1 AND is_current = TRUE
		  FOR UPDATE`,
		serviceID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return DeploymentRecord{}, false, nil
	}
	if err != nil {
		return DeploymentRecord{}, false, err
	}
	return rec, true, nil
}

func (s *persistence) deploymentByIDTx(ctx context.Context, tx *sql.Tx, deploymentID string) (DeploymentRecord, error) {
	return s.queryDeploymentRow(ctx, tx,
		`SELECT `+deploymentSelectColumns+`
		   FROM deployments
		  WHERE id = $1
		  FOR UPDATE`,
		deploymentID,
	)
}

func (s *persistence) deploymentByBuildIDTx(ctx context.Context, tx *sql.Tx, serviceID, buildID string) (DeploymentRecord, bool, error) {
	if buildID == "" {
		return DeploymentRecord{}, false, nil
	}
	rec, err := s.queryDeploymentRow(ctx, tx,
		`SELECT `+deploymentSelectColumns+`
		   FROM deployments
		  WHERE service_id = $1 AND build_id = $2
		  ORDER BY is_current DESC, updated_at DESC, id DESC
		  LIMIT 1
		  FOR UPDATE`,
		serviceID, buildID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return DeploymentRecord{}, false, nil
	}
	if err != nil {
		return DeploymentRecord{}, false, err
	}
	return rec, true, nil
}

func (s *persistence) deploymentByRolloutTx(ctx context.Context, tx *sql.Tx, serviceID string, rolloutGeneration int64) (DeploymentRecord, bool, error) {
	if rolloutGeneration <= 0 {
		return DeploymentRecord{}, false, nil
	}
	rec, err := s.queryDeploymentRow(ctx, tx,
		`SELECT `+deploymentSelectColumns+`
		   FROM deployments
		  WHERE service_id = $1 AND rollout_generation = $2
		  ORDER BY is_current DESC, created_at DESC, id DESC
		  LIMIT 1
		  FOR UPDATE`,
		serviceID, rolloutGeneration,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return DeploymentRecord{}, false, nil
	}
	if err != nil {
		return DeploymentRecord{}, false, err
	}
	return rec, true, nil
}

func (s *persistence) attachLatestDeploymentQuerier(ctx context.Context, q ServiceQueryer, rec *ServiceRecord) error {
	dep, err := s.queryDeploymentRow(ctx, q,
		`SELECT `+deploymentSelectColumns+`
		   FROM deployments
		  WHERE service_id = $1 AND is_current = TRUE`,
		rec.ID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	transitions, err := s.loadDeploymentTransitions(ctx, q, dep.ID)
	if err != nil {
		return err
	}
	dep.Transitions = transitions
	dep.Actions, err = s.loadDeploymentActions(ctx, q, dep.ID)
	if err != nil {
		return err
	}
	if dep.BuildID != "" {
		build, err := s.buildRunByIDQuerier(ctx, q, dep.BuildID)
		if err == nil {
			buildCopy := build
			dep.Build = &buildCopy
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if dep.ArtifactID != "" {
		artifact, err := s.buildArtifactByIDQuerier(ctx, q, dep.ArtifactID)
		if err != nil {
			return err
		}
		dep.Artifact = &artifact
	}
	rec.LatestDeployment = &dep
	return nil
}

func (s *persistence) insertDeploymentTx(
	ctx context.Context,
	tx *sql.Tx,
	serviceID string,
	state string,
	actor deploymentActor,
	reasonCode, detail string,
	specRevision, rolloutGeneration int64,
	buildID, artifactID, requestedByUserID string,
	now time.Time,
) (DeploymentRecord, error) {
	if err := s.lockServiceTx(ctx, tx, serviceID); err != nil {
		return DeploymentRecord{}, err
	}
	if err := s.retireCurrentDeploymentTx(ctx, tx, serviceID, actor, now); err != nil {
		return DeploymentRecord{}, err
	}
	if reasonCode == "" {
		reasonCode = reasonCodeForState(state)
	}
	detail = firstNonEmpty(sanitizeDeploymentDetail(detail), DefaultDetailForState(state))
	rec := DeploymentRecord{
		ID:                uuid.NewString(),
		ServiceID:         serviceID,
		SpecRevision:      specRevision,
		RolloutGeneration: rolloutGeneration,
		BuildID:           buildID,
		ArtifactID:        artifactID,
		State:             state,
		CauseKind:         normalizeDeploymentCauseKind(actor.Kind),
		CauseID:           actor.ID,
		ReasonCode:        reasonCode,
		Detail:            detail,
		IsCurrent:         true,
		RequestedByUserID: requestedByUserID,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	resolvedSpec, err := s.loadServiceDetailsQuerier(ctx, tx, serviceID, specRevision)
	if err != nil {
		return DeploymentRecord{}, err
	}
	rec.ResolvedSpec = resolvedSpec
	if _, err := journal.DeploymentRow(rec.ID).Exec(ctx, tx,
		`INSERT INTO deployments(
			id, service_id, spec_revision, rollout_generation, build_id, artifact_id, source_revision_id,
			state, cause_kind, cause_id, reason_code, detail, is_current, requested_by_user_id, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), (SELECT source_revision_id FROM build_runs WHERE id = $5), $7, $8, $9, $10, $11, TRUE, $12, $13, $13)`,
		rec.ID, rec.ServiceID, rec.SpecRevision, rec.RolloutGeneration, rec.BuildID, rec.ArtifactID,
		rec.State, rec.CauseKind, rec.CauseID, rec.ReasonCode, rec.Detail, rec.RequestedByUserID, rec.CreatedAt,
	); err != nil {
		return DeploymentRecord{}, err
	}
	if err := s.insertDeploymentTransitionTx(ctx, tx, rec.ID, "", rec.State, rec.CauseKind, rec.CauseID, rec.ReasonCode, rec.Detail, rec.SpecRevision, rec.ArtifactID, rec.RolloutGeneration, now); err != nil {
		return DeploymentRecord{}, err
	}
	if rec.ArtifactID != "" {
		artifact, err := s.buildArtifactByIDQuerier(ctx, tx, rec.ArtifactID)
		if err != nil {
			return DeploymentRecord{}, err
		}
		rec.Artifact = &artifact
		rec.ImageDigest = artifact.ImageRef
	}
	return rec, nil
}

func (s *persistence) retireCurrentDeploymentTx(ctx context.Context, tx *sql.Tx, serviceID string, actor deploymentActor, now time.Time) error {
	current, ok, err := s.currentDeploymentTx(ctx, tx, serviceID)
	if err != nil || !ok {
		return err
	}
	if deploymentStateTerminal(current.State) {
		if _, err := journal.DeploymentRow(current.ID).Exec(ctx, tx, `UPDATE deployments SET is_current = FALSE, updated_at = $1 WHERE id = $2`, now, current.ID); err != nil {
			return err
		}
		return nil
	}
	if current.State == DeploymentStateActive {
		if _, err := journal.DeploymentRow(current.ID).Exec(ctx, tx, `UPDATE deployments SET is_current = FALSE, updated_at = $1 WHERE id = $2`, now, current.ID); err != nil {
			return err
		}
		return nil
	}
	nextState := DeploymentStateSuperseded
	reasonCode := reasonDeploymentSuperseded
	detail := "Superseded by a newer deployment"
	if current.State == DeploymentStateDraining {
		nextState = DeploymentStateDraining
		reasonCode = reasonDeploymentDraining
		detail = "Draining after a newer deployment started"
	}
	_, err = s.applyDeploymentTransitionTx(ctx, tx, current.ID, deploymentTransitionInput{
		ToState:    nextState,
		Actor:      actor,
		ReasonCode: reasonCode,
		Detail:     detail,
	})
	if err != nil {
		return err
	}
	if _, err := journal.DeploymentRow(current.ID).Exec(ctx, tx, `UPDATE deployments SET is_current = FALSE, updated_at = $1 WHERE id = $2`, now, current.ID); err != nil {
		return err
	}
	return nil
}

func (s *persistence) applyDeploymentTransitionTx(ctx context.Context, tx *sql.Tx, deploymentID string, input deploymentTransitionInput) (DeploymentRecord, error) {
	rec, err := s.deploymentByIDTx(ctx, tx, deploymentID)
	if err != nil {
		return DeploymentRecord{}, err
	}
	fromState := rec.State
	now := time.Now().UTC()
	rec, changed, err := decideDeploymentTransition(rec, input, now)
	if err != nil || !changed {
		return rec, err
	}

	if _, err := journal.DeploymentRow(rec.ID).Exec(ctx, tx,
		`UPDATE deployments
		    SET spec_revision = $1,
		        rollout_generation = $2,
		        build_id = $3,
		        artifact_id = NULLIF($4, ''),
		        state = $5,
		        cause_kind = $6,
		        cause_id = $7,
		        reason_code = $8,
		        detail = $9,
		        updated_at = $10
		  WHERE id = $11`,
		rec.SpecRevision, rec.RolloutGeneration, rec.BuildID, rec.ArtifactID,
		rec.State, rec.CauseKind, rec.CauseID, rec.ReasonCode, rec.Detail, rec.UpdatedAt, rec.ID,
	); err != nil {
		return DeploymentRecord{}, err
	}
	if err := s.insertDeploymentTransitionTx(ctx, tx, rec.ID, fromState, rec.State, rec.CauseKind, rec.CauseID, rec.ReasonCode, rec.Detail, rec.SpecRevision, rec.ArtifactID, rec.RolloutGeneration, now); err != nil {
		return DeploymentRecord{}, err
	}
	if input.HasArtifactID {
		if rec.ArtifactID == "" {
			rec.Artifact = nil
			rec.ImageDigest = ""
		} else {
			artifact, err := s.buildArtifactByIDQuerier(ctx, tx, rec.ArtifactID)
			if err != nil {
				return DeploymentRecord{}, err
			}
			rec.Artifact = &artifact
			rec.ImageDigest = artifact.ImageRef
		}
	}
	return rec, nil
}

func (s *persistence) insertDeploymentTransitionTx(
	ctx context.Context,
	tx *sql.Tx,
	deploymentID, fromState, toState, causeKind, causeID, reasonCode, detail string,
	specRevision int64,
	artifactID string,
	rolloutGeneration int64,
	now time.Time,
) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO deployment_transitions(
			id, deployment_id, from_state, to_state, cause_kind, cause_id, reason_code, detail,
			spec_revision, artifact_id, rollout_generation, occurred_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, ''), $11, $12)`,
		uuid.NewString(), deploymentID, fromState, toState, causeKind, causeID, reasonCode, detail,
		specRevision, artifactID, rolloutGeneration, now,
	)
	return err
}

func (s *persistence) applyDeploymentTransitionByBuildTx(ctx context.Context, tx *sql.Tx, serviceID, buildID string, input deploymentTransitionInput) (DeploymentRecord, error) {
	rec, ok, err := s.deploymentByBuildIDTx(ctx, tx, serviceID, buildID)
	if err != nil {
		return DeploymentRecord{}, err
	}
	if !ok {
		return DeploymentRecord{}, sql.ErrNoRows
	}
	return s.applyDeploymentTransitionTx(ctx, tx, rec.ID, input)
}

func (s *persistence) completeDrainedPredecessorsTx(ctx context.Context, tx *sql.Tx, serviceID, activeDeploymentID string, actor deploymentActor) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM deployments
		  WHERE service_id = $1
		    AND id != $2
		    AND state = $3
		  FOR UPDATE`,
		serviceID, activeDeploymentID, DeploymentStateDraining,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.applyDeploymentTransitionTx(ctx, tx, id, deploymentTransitionInput{
			ToState:    DeploymentStateCompleted,
			Actor:      actor,
			ReasonCode: reasonDeploymentCompleted,
			Detail:     "Replaced by an active deployment",
		}); err != nil {
			return err
		}
	}
	return nil
}

// applyAgentDeploymentObservationTx applies an agent observation and reports whether durable
// state changed. Terminal records never transition.
func (s *persistence) applyAgentDeploymentObservationTx(
	ctx context.Context,
	tx *sql.Tx,
	serviceID string,
	desiredRolloutGeneration int64,
	phase string,
	message string,
	healthy bool,
	appliedRolloutGeneration int64,
	agentID string,
) (bool, error) {
	rec, ok, err := s.deploymentByRolloutTx(ctx, tx, serviceID, desiredRolloutGeneration)
	if err != nil {
		return false, err
	}
	if !ok {
		rec, ok, err = s.currentDeploymentTx(ctx, tx, serviceID)
		if err != nil || !ok {
			return false, err
		}
	}
	input, recognized := decideAgentDeploymentTransition(rec, deploymentAgentObservation{
		Phase: phase, Message: message, AgentID: agentID, Healthy: healthy,
		AppliedGeneration: appliedRolloutGeneration, DesiredGeneration: desiredRolloutGeneration,
	})
	if !recognized {
		return false, nil
	}
	updated, err := s.applyDeploymentTransitionTx(ctx, tx, rec.ID, input)
	if err != nil {
		return false, err
	}
	// An applied transition shows as a state change; an ignored one (e.g. a record that
	// turned terminal) leaves the record untouched.
	if updated.State == rec.State {
		return false, nil
	}
	if updated.State == DeploymentStateActive {
		return true, s.completeDrainedPredecessorsTx(ctx, tx, serviceID, updated.ID, deploymentActor{Kind: DeploymentCauseAgent, ID: agentID})
	}
	return true, nil
}

func (s *persistence) markCurrentDeploymentRemovedTx(ctx context.Context, tx *sql.Tx, serviceID string, actor deploymentActor) error {
	current, ok, err := s.currentDeploymentTx(ctx, tx, serviceID)
	if err != nil || !ok {
		return err
	}
	_, err = s.applyDeploymentTransitionTx(ctx, tx, current.ID, deploymentTransitionInput{
		ToState:          DeploymentStateRemoved,
		Actor:            actor,
		ReasonCode:       reasonDeploymentRemoved,
		Detail:           "Service removed",
		IgnoreIfTerminal: true,
	})
	return err
}

// scanDeploymentRow scans deploymentSelectColumns. ResolvedSpec stays unset:
// it lives on the spec revision, loaded by queryDeploymentRow or the caller.
func scanDeploymentRow(scanner interface{ Scan(...any) error }) (DeploymentRecord, error) {
	var rec DeploymentRecord
	if err := scanner.Scan(
		&rec.ID,
		&rec.ServiceID,
		&rec.SpecRevision,
		&rec.RolloutGeneration,
		&rec.BuildID,
		&rec.ArtifactID,
		&rec.ImageDigest,
		&rec.State,
		&rec.CauseKind,
		&rec.CauseID,
		&rec.ReasonCode,
		&rec.Detail,
		&rec.IsCurrent,
		&rec.RequestedByUserID,
		&rec.CreatedAt,
		&rec.UpdatedAt,
		&rec.SourceRevisionID,
	); err != nil {
		return DeploymentRecord{}, err
	}
	return rec, nil
}

// queryDeploymentRow loads one deployment with its resolved spec revision.
func (s *persistence) queryDeploymentRow(ctx context.Context, q ServiceQueryer, query string, args ...any) (DeploymentRecord, error) {
	rec, err := scanDeploymentRow(q.QueryRowContext(ctx, query, args...))
	if err != nil {
		return DeploymentRecord{}, err
	}
	if rec.ResolvedSpec, err = s.loadServiceDetailsQuerier(ctx, q, rec.ServiceID, rec.SpecRevision); err != nil {
		return DeploymentRecord{}, fmt.Errorf("load deployment %s spec: %w", rec.ID, err)
	}
	return rec, nil
}

func (s *persistence) loadDeploymentTransitions(ctx context.Context, q ServiceQueryer, deploymentID string) ([]DeploymentTransitionRecord, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, deployment_id, from_state, to_state, cause_kind, cause_id, reason_code, detail,
		        spec_revision, COALESCE(artifact_id, ''),
		        COALESCE((SELECT image_ref FROM build_artifacts WHERE id = deployment_transitions.artifact_id), ''),
		        rollout_generation, occurred_at
		   FROM deployment_transitions
		  WHERE deployment_id = $1
		  ORDER BY occurred_at ASC, id ASC`,
		deploymentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeploymentTransitionRecord
	for rows.Next() {
		var rec DeploymentTransitionRecord
		if err := rows.Scan(
			&rec.ID,
			&rec.DeploymentID,
			&rec.FromState,
			&rec.ToState,
			&rec.CauseKind,
			&rec.CauseID,
			&rec.ReasonCode,
			&rec.Detail,
			&rec.SpecRevision,
			&rec.ArtifactID,
			&rec.ImageDigest,
			&rec.RolloutGeneration,
			&rec.OccurredAt,
		); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
