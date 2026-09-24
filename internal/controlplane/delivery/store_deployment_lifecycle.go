package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/journal"

	"google.golang.org/protobuf/encoding/protojson"
)

const deploymentSelectColumns = `id, service_id, spec_revision, rollout_generation, build_id, COALESCE(artifact_id, ''),
	        COALESCE((SELECT image_ref FROM build_artifacts WHERE id = deployments.artifact_id), ''),
	        state, cause_kind, cause_id, reason_code, detail, resolved_spec_json, variable_versions_json,
	        sealed_versions_json, is_current, requested_by_user_id, created_at, updated_at`

func (s *persistence) lockServiceTx(ctx context.Context, tx *sql.Tx, serviceID string) error {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM services WHERE id = $1 FOR UPDATE`, serviceID).Scan(&id)
	if err != nil {
		return err
	}
	return nil
}

func (s *persistence) currentDeploymentTx(ctx context.Context, tx *sql.Tx, serviceID string) (DeploymentRecord, bool, error) {
	rec, err := scanDeploymentRow(tx.QueryRowContext(ctx,
		`SELECT `+deploymentSelectColumns+`
		   FROM deployments
		  WHERE service_id = $1 AND is_current = TRUE
		  FOR UPDATE`,
		serviceID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return DeploymentRecord{}, false, nil
	}
	if err != nil {
		return DeploymentRecord{}, false, err
	}
	return rec, true, nil
}

func (s *persistence) deploymentByIDTx(ctx context.Context, tx *sql.Tx, deploymentID string) (DeploymentRecord, error) {
	return scanDeploymentRow(tx.QueryRowContext(ctx,
		`SELECT `+deploymentSelectColumns+`
		   FROM deployments
		  WHERE id = $1
		  FOR UPDATE`,
		deploymentID,
	))
}

func (s *persistence) deploymentByBuildIDTx(ctx context.Context, tx *sql.Tx, serviceID, buildID string) (DeploymentRecord, bool, error) {
	if buildID == "" {
		return DeploymentRecord{}, false, nil
	}
	rec, err := scanDeploymentRow(tx.QueryRowContext(ctx,
		`SELECT `+deploymentSelectColumns+`
		   FROM deployments
		  WHERE service_id = $1 AND build_id = $2
		  ORDER BY is_current DESC, updated_at DESC, id DESC
		  LIMIT 1
		  FOR UPDATE`,
		serviceID, buildID,
	))
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
	rec, err := scanDeploymentRow(tx.QueryRowContext(ctx,
		`SELECT `+deploymentSelectColumns+`
		   FROM deployments
		  WHERE service_id = $1 AND rollout_generation = $2
		  ORDER BY is_current DESC, created_at DESC, id DESC
		  LIMIT 1
		  FOR UPDATE`,
		serviceID, rolloutGeneration,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return DeploymentRecord{}, false, nil
	}
	if err != nil {
		return DeploymentRecord{}, false, err
	}
	return rec, true, nil
}

func (s *persistence) attachLatestDeploymentQuerier(ctx context.Context, q ServiceQueryer, rec *ServiceRecord) error {
	dep, err := scanDeploymentRow(q.QueryRowContext(ctx,
		`SELECT `+deploymentSelectColumns+`
		   FROM deployments
		  WHERE service_id = $1 AND is_current = TRUE`,
		rec.ID,
	))
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
	detail = FirstNonEmpty(sanitizeDeploymentDetail(detail), DefaultDetailForState(state))
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
	resolvedSpecJSON, err := protojson.Marshal(resolvedSpec)
	if err != nil {
		return DeploymentRecord{}, err
	}
	rec.ResolvedSpec = resolvedSpec
	rec.VariableVersions = deploymentVariableVersions(resolvedSpec, specRevision)
	rec.SealedVersions = map[string]int64{}
	if s.secrets != nil {
		live, err := s.secrets.Sealed().CurrentVersions(ctx, tx, serviceID)
		if err != nil {
			return DeploymentRecord{}, err
		}
		rec.SealedVersions = live
	}
	variableVersionsJSON, err := json.Marshal(rec.VariableVersions)
	if err != nil {
		return DeploymentRecord{}, err
	}
	sealedVersionsJSON, err := json.Marshal(rec.SealedVersions)
	if err != nil {
		return DeploymentRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO deployments(
			id, service_id, spec_revision, rollout_generation, build_id, artifact_id,
			state, cause_kind, cause_id, reason_code, detail, resolved_spec_json, variable_versions_json,
			sealed_versions_json, is_current, requested_by_user_id, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9, $10, $11, $12, $13, $14, TRUE, $15, $16, $16)`,
		rec.ID, rec.ServiceID, rec.SpecRevision, rec.RolloutGeneration, rec.BuildID, rec.ArtifactID,
		rec.State, rec.CauseKind, rec.CauseID, rec.ReasonCode, rec.Detail, resolvedSpecJSON, variableVersionsJSON,
		sealedVersionsJSON, rec.RequestedByUserID, rec.CreatedAt,
	); err != nil {
		return DeploymentRecord{}, err
	}
	journal.RecordDeployment(ctx, rec.ID)
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
		if _, err := tx.ExecContext(ctx, `UPDATE deployments SET is_current = FALSE, updated_at = $1 WHERE id = $2`, now, current.ID); err != nil {
			return err
		}
		journal.RecordDeployment(ctx, current.ID)
		return nil
	}
	if current.State == DeploymentStateActive {
		if _, err := tx.ExecContext(ctx, `UPDATE deployments SET is_current = FALSE, updated_at = $1 WHERE id = $2`, now, current.ID); err != nil {
			return err
		}
		journal.RecordDeployment(ctx, current.ID)
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
	if _, err := tx.ExecContext(ctx, `UPDATE deployments SET is_current = FALSE, updated_at = $1 WHERE id = $2`, now, current.ID); err != nil {
		return err
	}
	journal.RecordDeployment(ctx, current.ID)
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

	if _, err := tx.ExecContext(ctx,
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
	journal.RecordDeployment(ctx, rec.ID)
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

// applyAgentDeploymentObservationTx applies an agent observation to the
// deployment record and reports whether durable state changed. Terminal
// records never transition, so callers must not assume that reaching this
// path bumped the status revision.
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
	// Agent transitions always target a different state, so an applied
	// transition is visible as a state change; an ignored one (for example a
	// record that turned terminal) leaves the record untouched.
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

func scanDeploymentRow(scanner interface{ Scan(...any) error }) (DeploymentRecord, error) {
	var rec DeploymentRecord
	var resolvedSpecJSON, variableVersionsJSON, sealedVersionsJSON []byte
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
		&resolvedSpecJSON,
		&variableVersionsJSON,
		&sealedVersionsJSON,
		&rec.IsCurrent,
		&rec.RequestedByUserID,
		&rec.CreatedAt,
		&rec.UpdatedAt,
	); err != nil {
		return DeploymentRecord{}, err
	}
	rec.ResolvedSpec = &platformv1.ServiceSpec{}
	if err := protojson.Unmarshal(resolvedSpecJSON, rec.ResolvedSpec); err != nil {
		return DeploymentRecord{}, fmt.Errorf("decode deployment resolved spec: %w", err)
	}
	if err := json.Unmarshal(variableVersionsJSON, &rec.VariableVersions); err != nil {
		return DeploymentRecord{}, fmt.Errorf("decode deployment variable versions: %w", err)
	}
	if rec.VariableVersions == nil {
		rec.VariableVersions = map[string]int64{}
	}
	if err := json.Unmarshal(sealedVersionsJSON, &rec.SealedVersions); err != nil {
		return DeploymentRecord{}, fmt.Errorf("decode deployment sealed versions: %w", err)
	}
	if rec.SealedVersions == nil {
		rec.SealedVersions = map[string]int64{}
	}
	return rec, nil
}

func deploymentVariableVersions(spec *platformv1.ServiceSpec, specRevision int64) map[string]int64 {
	versions := make(map[string]int64)
	if spec == nil || spec.GetRuntime() == nil {
		return versions
	}
	for key := range spec.GetRuntime().GetEnv() {
		versions[key] = specRevision
	}
	return versions
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
