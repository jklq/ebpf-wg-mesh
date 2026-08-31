package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/encoding/protojson"
)

const deploymentSelectColumns = `id, service_id, spec_revision, rollout_generation, build_id, image_digest,
	        state, cause_kind, cause_id, reason_code, detail, resolved_spec_json, variable_versions_json,
	        is_current, requested_by_user_id, created_at, updated_at`

func (s *Store) lockServiceTx(ctx context.Context, tx *sql.Tx, serviceID string) error {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM services WHERE id = $1 FOR UPDATE`, serviceID).Scan(&id)
	if err != nil {
		return err
	}
	return nil
}

func (s *Store) currentDeploymentTx(ctx context.Context, tx *sql.Tx, serviceID string) (deploymentRecord, bool, error) {
	rec, err := scanDeploymentRow(tx.QueryRowContext(ctx,
		`SELECT `+deploymentSelectColumns+`
		   FROM deployments
		  WHERE service_id = $1 AND is_current = TRUE
		  FOR UPDATE`,
		serviceID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return deploymentRecord{}, false, nil
	}
	if err != nil {
		return deploymentRecord{}, false, err
	}
	return rec, true, nil
}

func (s *Store) deploymentByIDTx(ctx context.Context, tx *sql.Tx, deploymentID string) (deploymentRecord, error) {
	return scanDeploymentRow(tx.QueryRowContext(ctx,
		`SELECT `+deploymentSelectColumns+`
		   FROM deployments
		  WHERE id = $1
		  FOR UPDATE`,
		deploymentID,
	))
}

func (s *Store) deploymentByBuildIDTx(ctx context.Context, tx *sql.Tx, serviceID, buildID string) (deploymentRecord, bool, error) {
	if buildID == "" {
		return deploymentRecord{}, false, nil
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
		return deploymentRecord{}, false, nil
	}
	if err != nil {
		return deploymentRecord{}, false, err
	}
	return rec, true, nil
}

func (s *Store) deploymentByRolloutTx(ctx context.Context, tx *sql.Tx, serviceID string, rolloutGeneration int64) (deploymentRecord, bool, error) {
	if rolloutGeneration <= 0 {
		return deploymentRecord{}, false, nil
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
		return deploymentRecord{}, false, nil
	}
	if err != nil {
		return deploymentRecord{}, false, err
	}
	return rec, true, nil
}

func (s *Store) attachLatestDeploymentQuerier(ctx context.Context, q serviceQueryer, rec *serviceRecord) error {
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
	rec.LatestDeployment = &dep
	return nil
}

func (s *Store) currentDeploymentForService(ctx context.Context, serviceID string) (deploymentRecord, bool, error) {
	rec, err := scanDeploymentRow(s.db.QueryRowContext(ctx,
		`SELECT `+deploymentSelectColumns+`
		   FROM deployments
		  WHERE service_id = $1 AND is_current = TRUE`,
		serviceID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return deploymentRecord{}, false, nil
	}
	if err != nil {
		return deploymentRecord{}, false, err
	}
	return rec, true, nil
}

func (s *Store) insertDeploymentTx(
	ctx context.Context,
	tx *sql.Tx,
	serviceID string,
	state string,
	actor deploymentActor,
	reasonCode, detail string,
	specRevision, rolloutGeneration int64,
	buildID, imageDigest, requestedByUserID string,
	now time.Time,
) (deploymentRecord, error) {
	if err := s.lockServiceTx(ctx, tx, serviceID); err != nil {
		return deploymentRecord{}, err
	}
	if err := s.retireCurrentDeploymentTx(ctx, tx, serviceID, actor, now); err != nil {
		return deploymentRecord{}, err
	}
	if reasonCode == "" {
		reasonCode = reasonCodeForState(state)
	}
	detail = firstNonEmpty(sanitizeDeploymentDetail(detail), defaultDetailForState(state))
	rec := deploymentRecord{
		ID:                mustID(),
		ServiceID:         serviceID,
		SpecRevision:      specRevision,
		RolloutGeneration: rolloutGeneration,
		BuildID:           buildID,
		ImageDigest:       imageDigest,
		State:             state,
		CauseKind:         normalizeDeploymentCauseKind(actor.Kind),
		CauseID:           actor.ID,
		ReasonCode:        reasonCode,
		Detail:            detail,
		IsCurrent:         true,
		RequestedByUserID: requestedByUserID,
		CreatedAt:         now,
		UpdatedAt:         now,
		Reason:            reasonCode,
	}
	resolvedSpec, err := s.loadServiceDetailsQuerier(ctx, tx, serviceID, specRevision)
	if err != nil {
		return deploymentRecord{}, err
	}
	resolvedSpecJSON, err := protojson.Marshal(resolvedSpec)
	if err != nil {
		return deploymentRecord{}, err
	}
	rec.ResolvedSpec = resolvedSpec
	rec.VariableVersions = deploymentVariableVersions(resolvedSpec, specRevision)
	variableVersionsJSON, err := json.Marshal(rec.VariableVersions)
	if err != nil {
		return deploymentRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO deployments(
			id, service_id, spec_revision, rollout_generation, build_id, image_digest,
			state, cause_kind, cause_id, reason_code, detail, resolved_spec_json, variable_versions_json,
			is_current, requested_by_user_id, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, TRUE, $14, $15, $15)`,
		rec.ID, rec.ServiceID, rec.SpecRevision, rec.RolloutGeneration, rec.BuildID, rec.ImageDigest,
		rec.State, rec.CauseKind, rec.CauseID, rec.ReasonCode, rec.Detail, resolvedSpecJSON, variableVersionsJSON,
		rec.RequestedByUserID, rec.CreatedAt,
	); err != nil {
		return deploymentRecord{}, err
	}
	if err := s.insertDeploymentTransitionTx(ctx, tx, rec.ID, "", rec.State, rec.CauseKind, rec.CauseID, rec.ReasonCode, rec.Detail, rec.SpecRevision, rec.ImageDigest, rec.RolloutGeneration, now); err != nil {
		return deploymentRecord{}, err
	}
	return rec, nil
}

func (s *Store) retireCurrentDeploymentTx(ctx context.Context, tx *sql.Tx, serviceID string, actor deploymentActor, now time.Time) error {
	current, ok, err := s.currentDeploymentTx(ctx, tx, serviceID)
	if err != nil || !ok {
		return err
	}
	if deploymentStateTerminal(current.State) {
		if _, err := tx.ExecContext(ctx, `UPDATE deployments SET is_current = FALSE, updated_at = $1 WHERE id = $2`, now, current.ID); err != nil {
			return err
		}
		return nil
	}
	// The last active deployment remains active while its allocations keep
	// serving. It becomes draining only after healthy replacements enter
	// ingress; merely requesting a rollout must not lie about that cutover.
	if current.State == deploymentStateActive {
		_, err := tx.ExecContext(ctx, `UPDATE deployments SET is_current = FALSE, updated_at = $1 WHERE id = $2`, now, current.ID)
		return err
	}
	nextState := deploymentStateSuperseded
	reasonCode := reasonDeploymentSuperseded
	detail := "Superseded by a newer deployment"
	if current.State == deploymentStateDraining {
		nextState = deploymentStateDraining
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
	return nil
}

func (s *Store) applyDeploymentTransitionTx(ctx context.Context, tx *sql.Tx, deploymentID string, input deploymentTransitionInput) (deploymentRecord, error) {
	rec, err := s.deploymentByIDTx(ctx, tx, deploymentID)
	if err != nil {
		return deploymentRecord{}, err
	}
	toState := input.ToState
	if toState == "" {
		return rec, errors.New("deployment transition target is required")
	}
	if rec.State == toState {
		return rec, nil
	}
	if deploymentStateTerminal(rec.State) {
		if input.IgnoreIfTerminal || input.Actor.Kind == deploymentCauseAgent {
			return rec, nil
		}
		return rec, fmt.Errorf("%w: %s", errDeploymentTerminal, rec.State)
	}
	if !deploymentTransitionAllowed(rec.State, toState) {
		if input.IgnoreIfTerminal || input.Actor.Kind == deploymentCauseAgent {
			return rec, nil
		}
		return rec, illegalTransitionError(rec.State, toState)
	}

	now := time.Now().UTC()
	fromState := rec.State
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
	rec.ReasonCode = firstNonEmpty(input.ReasonCode, reasonCodeForState(toState))
	rec.Detail = firstNonEmpty(sanitizeDeploymentDetail(input.Detail), defaultDetailForState(toState))
	rec.UpdatedAt = now
	rec.Reason = rec.ReasonCode

	if _, err := tx.ExecContext(ctx,
		`UPDATE deployments
		    SET spec_revision = $1,
		        rollout_generation = $2,
		        build_id = $3,
		        image_digest = $4,
		        state = $5,
		        cause_kind = $6,
		        cause_id = $7,
		        reason_code = $8,
		        detail = $9,
		        updated_at = $10
		  WHERE id = $11`,
		rec.SpecRevision, rec.RolloutGeneration, rec.BuildID, rec.ImageDigest,
		rec.State, rec.CauseKind, rec.CauseID, rec.ReasonCode, rec.Detail, rec.UpdatedAt, rec.ID,
	); err != nil {
		return deploymentRecord{}, err
	}
	if err := s.insertDeploymentTransitionTx(ctx, tx, rec.ID, fromState, toState, rec.CauseKind, rec.CauseID, rec.ReasonCode, rec.Detail, rec.SpecRevision, rec.ImageDigest, rec.RolloutGeneration, now); err != nil {
		return deploymentRecord{}, err
	}
	return rec, nil
}

func (s *Store) insertDeploymentTransitionTx(
	ctx context.Context,
	tx *sql.Tx,
	deploymentID, fromState, toState, causeKind, causeID, reasonCode, detail string,
	specRevision int64,
	imageDigest string,
	rolloutGeneration int64,
	now time.Time,
) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO deployment_transitions(
			id, deployment_id, from_state, to_state, cause_kind, cause_id, reason_code, detail,
			spec_revision, image_digest, rollout_generation, occurred_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		mustID(), deploymentID, fromState, toState, causeKind, causeID, reasonCode, detail,
		specRevision, imageDigest, rolloutGeneration, now,
	)
	return err
}

func (s *Store) applyDeploymentTransitionByBuildTx(ctx context.Context, tx *sql.Tx, serviceID, buildID string, input deploymentTransitionInput) (deploymentRecord, error) {
	rec, ok, err := s.deploymentByBuildIDTx(ctx, tx, serviceID, buildID)
	if err != nil {
		return deploymentRecord{}, err
	}
	if !ok {
		return deploymentRecord{}, sql.ErrNoRows
	}
	return s.applyDeploymentTransitionTx(ctx, tx, rec.ID, input)
}

func (s *Store) completeDrainedPredecessorsTx(ctx context.Context, tx *sql.Tx, serviceID, activeDeploymentID string, actor deploymentActor) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM deployments
		  WHERE service_id = $1
		    AND id != $2
		    AND state = $3
		  FOR UPDATE`,
		serviceID, activeDeploymentID, deploymentStateDraining,
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
			ToState:    deploymentStateCompleted,
			Actor:      actor,
			ReasonCode: reasonDeploymentCompleted,
			Detail:     "Replaced by an active deployment",
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyAgentDeploymentObservationTx(
	ctx context.Context,
	tx *sql.Tx,
	serviceID string,
	desiredRolloutGeneration int64,
	phase string,
	message string,
	healthy bool,
	appliedRolloutGeneration int64,
	agentID string,
) error {
	rec, ok, err := s.deploymentByRolloutTx(ctx, tx, serviceID, desiredRolloutGeneration)
	if err != nil {
		return err
	}
	if !ok {
		rec, ok, err = s.currentDeploymentTx(ctx, tx, serviceID)
		if err != nil || !ok {
			return err
		}
	}
	if deploymentStateTerminal(rec.State) {
		return nil
	}
	observed, recognized := agentObservedDeploymentState(phase, healthy, appliedRolloutGeneration, desiredRolloutGeneration)
	if !recognized {
		return nil
	}
	target := resolveAgentTargetState(rec.State, observed)
	if rec.State == target {
		return nil
	}
	updated, err := s.applyDeploymentTransitionTx(ctx, tx, rec.ID, deploymentTransitionInput{
		ToState:          target,
		Actor:            deploymentActor{Kind: deploymentCauseAgent, ID: agentID},
		ReasonCode:       firstNonEmpty(reasonCodeForState(target), reasonAgentObservation),
		Detail:           firstNonEmpty(sanitizeDeploymentDetail(message), defaultDetailForState(target)),
		IgnoreIfTerminal: true,
	})
	if err != nil {
		return err
	}
	if updated.State == deploymentStateActive {
		return s.completeDrainedPredecessorsTx(ctx, tx, serviceID, updated.ID, deploymentActor{Kind: deploymentCauseAgent, ID: agentID})
	}
	return nil
}

func (s *Store) markCurrentDeploymentRemovedTx(ctx context.Context, tx *sql.Tx, serviceID string, actor deploymentActor) error {
	current, ok, err := s.currentDeploymentTx(ctx, tx, serviceID)
	if err != nil || !ok {
		return err
	}
	_, err = s.applyDeploymentTransitionTx(ctx, tx, current.ID, deploymentTransitionInput{
		ToState:    deploymentStateRemoved,
		Actor:      actor,
		ReasonCode: reasonDeploymentRemoved,
		Detail:     "Service removed",
	})
	return err
}

func (s *Store) cancelCurrentDeployment(ctx context.Context, serviceID, userID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.lockServiceTx(ctx, tx, serviceID); err != nil {
			return err
		}
		current, ok, err := s.currentDeploymentTx(ctx, tx, serviceID)
		if err != nil {
			return err
		}
		if !ok {
			return sql.ErrNoRows
		}
		_, err = s.applyDeploymentTransitionTx(ctx, tx, current.ID, deploymentTransitionInput{
			ToState:    deploymentStateCancelled,
			Actor:      deploymentActor{Kind: deploymentCauseUser, ID: userID},
			ReasonCode: reasonUserCancel,
			Detail:     "Cancelled by user",
		})
		return err
	})
}

func scanDeploymentRow(scanner interface{ Scan(...any) error }) (deploymentRecord, error) {
	var rec deploymentRecord
	var resolvedSpecJSON, variableVersionsJSON []byte
	if err := scanner.Scan(
		&rec.ID,
		&rec.ServiceID,
		&rec.SpecRevision,
		&rec.RolloutGeneration,
		&rec.BuildID,
		&rec.ImageDigest,
		&rec.State,
		&rec.CauseKind,
		&rec.CauseID,
		&rec.ReasonCode,
		&rec.Detail,
		&resolvedSpecJSON,
		&variableVersionsJSON,
		&rec.IsCurrent,
		&rec.RequestedByUserID,
		&rec.CreatedAt,
		&rec.UpdatedAt,
	); err != nil {
		return deploymentRecord{}, err
	}
	rec.ResolvedSpec = &platformv1.ServiceSpec{}
	if err := protojson.Unmarshal(resolvedSpecJSON, rec.ResolvedSpec); err != nil {
		return deploymentRecord{}, fmt.Errorf("decode deployment resolved spec: %w", err)
	}
	if err := json.Unmarshal(variableVersionsJSON, &rec.VariableVersions); err != nil {
		return deploymentRecord{}, fmt.Errorf("decode deployment variable versions: %w", err)
	}
	if rec.VariableVersions == nil {
		rec.VariableVersions = map[string]int64{}
	}
	rec.Reason = rec.ReasonCode
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

func (s *Store) loadDeploymentTransitions(ctx context.Context, q serviceQueryer, deploymentID string) ([]deploymentTransitionRecord, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, deployment_id, from_state, to_state, cause_kind, cause_id, reason_code, detail,
		        spec_revision, image_digest, rollout_generation, occurred_at
		   FROM deployment_transitions
		  WHERE deployment_id = $1
		  ORDER BY occurred_at ASC, id ASC`,
		deploymentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []deploymentTransitionRecord
	for rows.Next() {
		var rec deploymentTransitionRecord
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
