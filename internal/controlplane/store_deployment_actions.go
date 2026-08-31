package controlplane

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/encoding/protojson"
)

const (
	deploymentActionRestart       = "restart"
	deploymentActionExactRedeploy = "exact_redeploy"
	deploymentActionRollback      = "rollback"
	deploymentActionCancel        = "cancel"
	deploymentActionRemove        = "remove"
	deploymentActionRetry         = "retry"
)

var (
	errDeploymentActionConflict = errors.New("idempotency key was already used for a different deployment action")
	errDeploymentActionInvalid  = errors.New("deployment action is not valid for the selected deployment")
	errDeploymentStale          = errors.New("selected deployment is no longer current")
)

func deploymentActionName(action platformv1.DeploymentAction) string {
	switch action {
	case platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART:
		return deploymentActionRestart
	case platformv1.DeploymentAction_DEPLOYMENT_ACTION_EXACT_REDEPLOY:
		return deploymentActionExactRedeploy
	case platformv1.DeploymentAction_DEPLOYMENT_ACTION_ROLLBACK:
		return deploymentActionRollback
	case platformv1.DeploymentAction_DEPLOYMENT_ACTION_CANCEL:
		return deploymentActionCancel
	case platformv1.DeploymentAction_DEPLOYMENT_ACTION_REMOVE:
		return deploymentActionRemove
	case platformv1.DeploymentAction_DEPLOYMENT_ACTION_RETRY:
		return deploymentActionRetry
	default:
		return ""
	}
}

func toProtoDeploymentAction(action string) platformv1.DeploymentAction {
	switch action {
	case deploymentActionRestart:
		return platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART
	case deploymentActionExactRedeploy:
		return platformv1.DeploymentAction_DEPLOYMENT_ACTION_EXACT_REDEPLOY
	case deploymentActionRollback:
		return platformv1.DeploymentAction_DEPLOYMENT_ACTION_ROLLBACK
	case deploymentActionCancel:
		return platformv1.DeploymentAction_DEPLOYMENT_ACTION_CANCEL
	case deploymentActionRemove:
		return platformv1.DeploymentAction_DEPLOYMENT_ACTION_REMOVE
	case deploymentActionRetry:
		return platformv1.DeploymentAction_DEPLOYMENT_ACTION_RETRY
	default:
		return platformv1.DeploymentAction_DEPLOYMENT_ACTION_UNSPECIFIED
	}
}

func (s *Store) applyDeploymentAction(
	ctx context.Context,
	userID, serviceID, deploymentID string,
	action platformv1.DeploymentAction,
	idempotencyKey, allocationID string,
) (serviceRecord, deploymentActionRecord, error) {
	actionName := deploymentActionName(action)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	allocationID = strings.TrimSpace(allocationID)
	if actionName == "" || idempotencyKey == "" || strings.TrimSpace(deploymentID) == "" {
		return serviceRecord{}, deploymentActionRecord{}, errDeploymentActionInvalid
	}
	if actionName != deploymentActionRestart && allocationID != "" {
		return serviceRecord{}, deploymentActionRecord{}, errDeploymentActionInvalid
	}

	var actionRecord deploymentActionRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		service, err := s.serviceByIDQuerier(ctx, tx, userID, "", serviceID)
		if err != nil {
			return err
		}
		if _, err := s.authorizeEnvironmentWriteQuerier(ctx, tx, userID, service.EnvironmentID); err != nil {
			return err
		}
		if err := s.lockServiceTx(ctx, tx, serviceID); err != nil {
			return err
		}

		existing, ok, err := s.deploymentActionByKeyTx(ctx, tx, serviceID, userID, idempotencyKey)
		if err != nil {
			return err
		}
		if ok {
			if existing.Action != actionName || existing.TargetDeploymentID != deploymentID || existing.AllocationID != allocationID {
				return errDeploymentActionConflict
			}
			actionRecord = existing
			return nil
		}

		target, err := s.deploymentByIDTx(ctx, tx, deploymentID)
		if err != nil {
			return err
		}
		if target.ServiceID != serviceID {
			return sql.ErrNoRows
		}

		resultDeploymentID, err := s.applyDeploymentActionTx(ctx, tx, service, target, actionName, allocationID, userID)
		if err != nil {
			return err
		}
		actionRecord = deploymentActionRecord{
			ID:                 mustID(),
			ServiceID:          serviceID,
			TargetDeploymentID: deploymentID,
			ResultDeploymentID: resultDeploymentID,
			Action:             actionName,
			AllocationID:       allocationID,
			IdempotencyKey:     idempotencyKey,
			RequestedByUserID:  userID,
			CreatedAt:          time.Now().UTC(),
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO deployment_actions(
				id, service_id, target_deployment_id, result_deployment_id, action,
				allocation_id, idempotency_key, requested_by_user_id, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			actionRecord.ID, actionRecord.ServiceID, actionRecord.TargetDeploymentID,
			actionRecord.ResultDeploymentID, actionRecord.Action, actionRecord.AllocationID,
			actionRecord.IdempotencyKey, actionRecord.RequestedByUserID, actionRecord.CreatedAt,
		)
		if err == nil {
			return nil
		}
		existing, ok, lookupErr := s.deploymentActionByKeyTx(ctx, tx, serviceID, userID, idempotencyKey)
		if lookupErr != nil {
			return err
		}
		if ok && existing.Action == actionName && existing.TargetDeploymentID == deploymentID && existing.AllocationID == allocationID {
			actionRecord = existing
			return nil
		}
		return err
	})
	if err != nil {
		return serviceRecord{}, deploymentActionRecord{}, err
	}
	service, err := s.serviceByID(ctx, userID, "", serviceID)
	return service, actionRecord, err
}

func (s *Store) applyDeploymentActionTx(ctx context.Context, tx *sql.Tx, service serviceRecord, target deploymentRecord, action, allocationID, userID string) (string, error) {
	switch action {
	case deploymentActionRestart:
		return s.restartDeploymentTx(ctx, tx, service, target, allocationID, userID)
	case deploymentActionExactRedeploy:
		if !immutableImageReference(target.ImageDigest) {
			return "", fmt.Errorf("%w: selected deployment image is not digest-pinned", errDeploymentActionInvalid)
		}
		return s.copyDeploymentRolloutTx(ctx, tx, service, target, userID, reasonExactRedeploy, "Exact redeploy scheduled")
	case deploymentActionRollback:
		if target.IsCurrent || !deploymentReusableForRollback(target.State) {
			return "", errDeploymentActionInvalid
		}
		if !immutableImageReference(target.ImageDigest) {
			return "", fmt.Errorf("%w: selected deployment image is not digest-pinned", errDeploymentActionInvalid)
		}
		return s.copyDeploymentRolloutTx(ctx, tx, service, target, userID, reasonRollback, "Rollback scheduled")
	case deploymentActionCancel:
		return s.cancelDeploymentTx(ctx, tx, service, target, userID)
	case deploymentActionRemove:
		return "", s.removeDeploymentTx(ctx, tx, service, target, userID)
	case deploymentActionRetry:
		return s.retryDeploymentTx(ctx, tx, service, target, userID)
	default:
		return "", errDeploymentActionInvalid
	}
}

func deploymentReusableForRollback(state string) bool {
	switch state {
	case deploymentStateActive, deploymentStateCompleted, deploymentStateDraining, deploymentStateRemoved:
		return true
	default:
		return false
	}
}

func immutableImageReference(image string) bool {
	const separator = "@sha256:"
	index := strings.LastIndex(strings.TrimSpace(image), separator)
	if index <= 0 {
		return false
	}
	digest := image[index+len(separator):]
	if len(digest) != 64 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func (s *Store) copyDeploymentRolloutTx(ctx context.Context, tx *sql.Tx, service serviceRecord, target deploymentRecord, userID, reasonCode, detail string) (string, error) {
	return s.copyDeploymentRolloutTargetTx(ctx, tx, service, target, userID, reasonCode, detail, "")
}

func (s *Store) copyDeploymentRolloutTargetTx(ctx context.Context, tx *sql.Tx, service serviceRecord, target deploymentRecord, userID, reasonCode, detail, targetAllocationID string) (string, error) {
	if target.ResolvedSpec == nil || strings.TrimSpace(target.ImageDigest) == "" {
		return "", fmt.Errorf("%w: selected deployment has no reusable image snapshot", errDeploymentActionInvalid)
	}
	existing, err := s.listAllocationsByServiceIDQuerier(ctx, tx, service.ID, true)
	if err != nil {
		return "", err
	}
	if targetAllocationID != "" {
		found := false
		for _, alloc := range existing {
			if alloc.ID == targetAllocationID && alloc.RolloutState == allocationRolloutServing {
				found = true
				break
			}
		}
		if !found {
			return "", sql.ErrNoRows
		}
	}
	now := time.Now().UTC()
	rolloutService := service
	rolloutService.Spec = target.ResolvedSpec
	if _, err := s.prepareReplacementRolloutTx(ctx, tx, rolloutService, existing, now); err != nil {
		return "", err
	}
	nextSpecRevision, err := s.insertCopiedServiceRevisionTx(ctx, tx, service.ID, target.ResolvedSpec)
	if err != nil {
		return "", err
	}
	desiredReplicas := specReplicaCount(target.ResolvedSpec, service.DesiredReplicaCount)
	if err := validateDesiredReplicaCount(desiredReplicas); err != nil {
		return "", err
	}
	if err := validateVolumeReplicaCompatibility(target.ResolvedSpec, desiredReplicas); err != nil {
		return "", err
	}
	nextRollout := service.RolloutGeneration + 1
	result, err := tx.ExecContext(ctx,
		`UPDATE services
		    SET current_spec_revision = $1,
		        current_rollout_generation = $2,
		        current_resolved_image = $3,
		        desired_replica_count = $4,
		        latest_build_id = $5,
		        updated_at = $6
		  WHERE id = $7 AND current_spec_revision = $8 AND current_rollout_generation = $9`,
		nextSpecRevision, nextRollout, target.ImageDigest, desiredReplicas, target.BuildID, now,
		service.ID, service.SpecRevision, service.RolloutGeneration,
	)
	if err != nil {
		return "", err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if affected != 1 {
		return "", errConcurrentUpdate
	}
	if err := s.insertServiceRolloutTx(ctx, tx, service.ID, nextRollout, nextSpecRevision, strings.ToLower(reasonCode), target.BuildID, userID, now); err != nil {
		return "", err
	}
	if targetAllocationID != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE service_rollouts SET target_allocation_id = $1 WHERE service_id = $2 AND rollout_generation = $3`,
			targetAllocationID, service.ID, nextRollout,
		); err != nil {
			return "", err
		}
	}
	dep, err := s.insertDeploymentTx(ctx, tx, service.ID, deploymentStateScheduling,
		deploymentActor{Kind: deploymentCauseUser, ID: userID}, reasonCode, detail,
		nextSpecRevision, nextRollout, target.BuildID, target.ImageDigest, userID, now)
	if err != nil {
		return "", err
	}
	variableVersionsJSON, err := json.Marshal(target.VariableVersions)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deployments SET variable_versions_json = $1 WHERE id = $2`, variableVersionsJSON, dep.ID); err != nil {
		return "", err
	}
	service.Spec = target.ResolvedSpec
	service.SpecRevision = nextSpecRevision
	service.RolloutGeneration = nextRollout
	service.ResolvedImage = target.ImageDigest
	service.DesiredReplicaCount = desiredReplicas
	if _, err := s.advanceRolloutTx(ctx, tx, service.ID, now); err != nil {
		return "", err
	}
	if err := s.bumpAllDesiredRevisionsTx(ctx, tx); err != nil {
		return "", err
	}
	return dep.ID, nil
}

func (s *Store) insertCopiedServiceRevisionTx(ctx context.Context, tx *sql.Tx, serviceID string, spec *platformv1.ServiceSpec) (int64, error) {
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(spec_revision), 0) + 1 FROM service_revisions WHERE service_id = $1`, serviceID).Scan(&revision); err != nil {
		return 0, err
	}
	raw, err := protojson.Marshal(canonicalServiceSpec(spec))
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at) VALUES ($1, $2, $3, $4)`,
		serviceID, revision, raw, time.Now().UTC(),
	)
	return revision, err
}

func (s *Store) restartDeploymentTx(ctx context.Context, tx *sql.Tx, service serviceRecord, target deploymentRecord, allocationID, userID string) (string, error) {
	if !target.IsCurrent || target.State != deploymentStateActive {
		return "", errDeploymentStale
	}
	detail := "Rolling restart scheduled for all replicas"
	if allocationID != "" {
		detail = "Rolling restart scheduled for allocation " + allocationID
	}
	return s.copyDeploymentRolloutTargetTx(ctx, tx, service, target, userID, reasonOperatorRestart, detail, allocationID)
}

func (s *Store) cancelDeploymentTx(ctx context.Context, tx *sql.Tx, service serviceRecord, target deploymentRecord, userID string) (string, error) {
	if !target.IsCurrent {
		return "", errDeploymentStale
	}
	if !deploymentStatePreActive(target.State) {
		return "", errDeploymentActionInvalid
	}
	now := time.Now().UTC()
	if target.BuildID != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_runs SET state = $1, failure_reason = $2, finished_at = $3
			  WHERE id = $4 AND state IN ($5, $6)`,
			buildStateCancelled, "cancelled by user", now, target.BuildID, buildStateQueued, buildStateRunning,
		); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE builder_workers SET current_build_id = '', updated_at = $1 WHERE current_build_id = $2`, now, target.BuildID,
		); err != nil {
			return "", err
		}
	}
	if _, err := s.applyDeploymentTransitionTx(ctx, tx, target.ID, deploymentTransitionInput{
		ToState: deploymentStateCancelled, Actor: deploymentActor{Kind: deploymentCauseUser, ID: userID},
		ReasonCode: reasonUserCancel, Detail: "Cancelled by user",
	}); err != nil {
		return "", err
	}
	fallback, ok, err := s.latestSuccessfulDeploymentTx(ctx, tx, service.ID, target.ID)
	if err != nil {
		return "", err
	}
	if ok && immutableImageReference(fallback.ImageDigest) && fallback.ResolvedSpec != nil {
		if err := s.supersedeCancelledRolloutTx(ctx, tx, service, now); err != nil {
			return "", err
		}
		return s.copyDeploymentRolloutTx(ctx, tx, service, fallback, userID, reasonUserCancel, "Restoring the last successful deployment after cancellation")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM allocations WHERE service_id = $1`, service.ID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE services SET current_resolved_image = '', updated_at = $1 WHERE id = $2`, now, service.ID); err != nil {
		return "", err
	}
	return "", s.bumpAllDesiredRevisionsTx(ctx, tx)
}

func (s *Store) supersedeCancelledRolloutTx(ctx context.Context, tx *sql.Tx, service serviceRecord, now time.Time) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM allocations
		  WHERE service_id = $1 AND desired_rollout_generation = $2 AND rollout_state = $3`,
		service.ID, service.RolloutGeneration, allocationRolloutStarting,
	); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE service_rollouts
		    SET state = $1, failure_reason = $2, completed_at = $3, progress_at = $3
		  WHERE service_id = $4 AND rollout_generation = $5 AND state IN ($6, $7)`,
		rolloutStateSuperseded, "cancelled by user", now, service.ID, service.RolloutGeneration,
		rolloutStatePendingBuild, rolloutStateInProgress,
	)
	return err
}

func (s *Store) removeDeploymentTx(ctx context.Context, tx *sql.Tx, service serviceRecord, target deploymentRecord, userID string) error {
	if !target.IsCurrent || (target.State != deploymentStateActive && target.State != deploymentStateDraining) {
		return errDeploymentStale
	}
	if target.State == deploymentStateDraining && target.ReasonCode != reasonUserRemove {
		return errDeploymentActionInvalid
	}
	actor := deploymentActor{Kind: deploymentCauseUser, ID: userID}
	now := time.Now().UTC()
	if target.State == deploymentStateActive {
		if _, err := s.applyDeploymentTransitionTx(ctx, tx, target.ID, deploymentTransitionInput{
			ToState: deploymentStateDraining, Actor: actor, ReasonCode: reasonUserRemove,
			Detail: "Withdrawal requested; draining allocations",
		}); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE allocations
		    SET rollout_state = $1, phase = 'Withdrawing',
		        message = 'removal requested; waiting for ingress withdrawal', updated_at = $2
		  WHERE service_id = $3 AND rollout_state NOT IN ($1, $4)`,
		allocationRolloutWithdrawing, now, service.ID, allocationRolloutDraining,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		var remaining int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM allocations WHERE service_id = $1`, service.ID).Scan(&remaining); err != nil {
			return err
		}
		if remaining == 0 {
			return s.finalizeDeploymentRemovalTx(ctx, tx, service.ID, target.ID, actor, now)
		}
	}
	return s.bumpAllDesiredRevisionsTx(ctx, tx)
}

func (s *Store) finalizeDeploymentRemovalTx(ctx context.Context, tx *sql.Tx, serviceID, deploymentID string, actor deploymentActor, now time.Time) error {
	if _, err := s.applyDeploymentTransitionTx(ctx, tx, deploymentID, deploymentTransitionInput{
		ToState: deploymentStateRemoved, Actor: actor, ReasonCode: reasonUserRemove,
		Detail: "Deployment removed after all allocations drained",
	}); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE services SET current_resolved_image = '', placement_message = '', updated_at = $1 WHERE id = $2`,
		now, serviceID,
	)
	return err
}

func (s *Store) retryDeploymentTx(ctx context.Context, tx *sql.Tx, service serviceRecord, target deploymentRecord, userID string) (string, error) {
	if target.State != deploymentStateFailed && target.State != deploymentStateCancelled && target.State != deploymentStateCrashed {
		return "", errDeploymentActionInvalid
	}
	if target.BuildID == "" || target.ImageDigest != "" {
		return s.copyDeploymentRolloutTx(ctx, tx, service, target, userID, reasonUserRetry, "Deployment retry scheduled")
	}
	build, err := s.buildRunByIDQuerier(ctx, tx, target.BuildID)
	if err != nil {
		return "", err
	}
	revision, err := s.sourceRevisionByIDTx(ctx, tx, build.SourceRevisionID)
	if err != nil {
		return "", err
	}
	snapshot, err := s.sourceSnapshotByRevisionIDTx(ctx, tx, revision.ID)
	if err != nil {
		return "", err
	}
	nextSpecRevision, err := s.insertCopiedServiceRevisionTx(ctx, tx, service.ID, target.ResolvedSpec)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE services SET current_spec_revision = $1, updated_at = $2 WHERE id = $3`, nextSpecRevision, time.Now().UTC(), service.ID); err != nil {
		return "", err
	}
	service.Spec = target.ResolvedSpec
	service.SpecRevision = nextSpecRevision
	retried, err := s.enqueueBuildFromSourceStateTx(ctx, tx, service, revision, snapshot, build.BuildRecipe, deploymentActor{Kind: deploymentCauseUser, ID: userID})
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deployments SET reason_code = $1, detail = $2 WHERE build_id = $3`, reasonUserRetry, "Build retry queued from immutable source snapshot", retried.ID); err != nil {
		return "", err
	}
	dep, ok, err := s.deploymentByBuildIDTx(ctx, tx, service.ID, retried.ID)
	if err != nil || !ok {
		return "", err
	}
	return dep.ID, nil
}

func (s *Store) latestSuccessfulDeploymentTx(ctx context.Context, tx *sql.Tx, serviceID, excludeID string) (deploymentRecord, bool, error) {
	rec, err := scanDeploymentRow(tx.QueryRowContext(ctx,
		`SELECT `+deploymentSelectColumns+` FROM deployments
		  WHERE service_id = $1 AND id != $2 AND state IN ($3, $4, $5) AND image_digest != ''
		  ORDER BY rollout_generation DESC, created_at DESC LIMIT 1 FOR UPDATE`,
		serviceID, excludeID, deploymentStateActive, deploymentStateCompleted, deploymentStateDraining,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return deploymentRecord{}, false, nil
	}
	return rec, err == nil, err
}

func (s *Store) deploymentActionByKeyTx(ctx context.Context, tx *sql.Tx, serviceID, userID, key string) (deploymentActionRecord, bool, error) {
	var rec deploymentActionRecord
	err := tx.QueryRowContext(ctx,
		`SELECT id, service_id, target_deployment_id, result_deployment_id, action,
		        allocation_id, idempotency_key, requested_by_user_id, created_at
		   FROM deployment_actions
		  WHERE service_id = $1 AND requested_by_user_id = $2 AND idempotency_key = $3
		  FOR UPDATE`, serviceID, userID, key,
	).Scan(&rec.ID, &rec.ServiceID, &rec.TargetDeploymentID, &rec.ResultDeploymentID, &rec.Action,
		&rec.AllocationID, &rec.IdempotencyKey, &rec.RequestedByUserID, &rec.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return deploymentActionRecord{}, false, nil
	}
	return rec, err == nil, err
}

func (s *Store) loadDeploymentActions(ctx context.Context, q serviceQueryer, deploymentID string) ([]deploymentActionRecord, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, service_id, target_deployment_id, result_deployment_id, action,
		        allocation_id, idempotency_key, requested_by_user_id, created_at
		   FROM deployment_actions WHERE target_deployment_id = $1 ORDER BY created_at ASC, id ASC`, deploymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []deploymentActionRecord
	for rows.Next() {
		var rec deploymentActionRecord
		if err := rows.Scan(&rec.ID, &rec.ServiceID, &rec.TargetDeploymentID, &rec.ResultDeploymentID, &rec.Action,
			&rec.AllocationID, &rec.IdempotencyKey, &rec.RequestedByUserID, &rec.CreatedAt); err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}
