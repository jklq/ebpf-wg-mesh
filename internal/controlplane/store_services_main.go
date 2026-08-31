package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/encoding/protojson"
)

func (s *Store) insertServiceRolloutTx(
	ctx context.Context,
	tx *sql.Tx,
	serviceID string,
	rolloutGeneration int64,
	specRevision int64,
	reason, buildID, requestedByUserID string,
	now time.Time,
) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO service_rollouts(
			service_id, rollout_generation, spec_revision, reason, build_id, requested_by_user_id, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		serviceID, rolloutGeneration, specRevision, reason, buildID, requestedByUserID, now,
	)
	return err
}

func (s *Store) createVolume(ctx context.Context, userID, environmentID, name string, sizeBytes int64, _ string) (volumeRecord, error) {
	var rec volumeRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		rec, err = s.createVolumeTx(ctx, tx, userID, environmentID, name, sizeBytes)
		return err
	})
	if err != nil {
		return volumeRecord{}, err
	}
	return rec, nil
}

func (s *Store) createScheduledVolume(ctx context.Context, userID, environmentID, name string, sizeBytes int64) (volumeRecord, error) {
	var rec volumeRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		rec, err = s.createVolumeTx(ctx, tx, userID, environmentID, name, sizeBytes)
		return err
	})
	if err != nil {
		return volumeRecord{}, err
	}
	return rec, nil
}

func (s *Store) chooseAgentForService(ctx context.Context, environmentID string, spec *platformv1.ServiceSpec) (string, error) {
	if volumeName := serviceVolumeName(spec); volumeName != "" {
		if err := s.requireVolumeQuerier(ctx, s.db, environmentID, volumeName); err != nil {
			return "", err
		}
	}
	return s.chooseAgentForPlacementQuerier(ctx, s.db, spec)
}

func (s *Store) listVolumes(ctx context.Context, userID, environmentID string) ([]volumeRecord, error) {
	if _, err := s.environmentByID(ctx, userID, environmentID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, environment_id, name, size_bytes, created_at
		   FROM volumes
		  WHERE environment_id = $1
		  ORDER BY created_at ASC`,
		environmentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []volumeRecord
	for rows.Next() {
		var rec volumeRecord
		if err := rows.Scan(&rec.ID, &rec.EnvironmentID, &rec.Name, &rec.SizeBytes, &rec.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) deleteVolume(ctx context.Context, userID, _ string, volumeID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var (
			volumeName    string
			environmentID string
		)
		err := tx.QueryRowContext(ctx, `SELECT name, environment_id FROM volumes WHERE id = $1`, volumeID).Scan(&volumeName, &environmentID)
		if err != nil {
			return err
		}
		if _, err := s.authorizeEnvironmentWriteQuerier(ctx, tx, userID, environmentID); err != nil {
			return err
		}

		rows, err := tx.QueryContext(ctx,
			`SELECT s.id, r.spec_json
			   FROM services s
			   JOIN service_revisions r ON r.service_id = s.id AND r.spec_revision = s.current_spec_revision
			  WHERE s.environment_id = $1`,
			environmentID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var serviceID string
			var rawSpec []byte
			if err := rows.Scan(&serviceID, &rawSpec); err != nil {
				return err
			}
			spec, err := loadServiceSpec(rawSpec)
			if err != nil {
				return err
			}
			if serviceVolumeName(spec) == volumeName {
				return fmt.Errorf("%w: service %s references volume %s", errVolumeInUse, serviceID, volumeID)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}

		result, err := tx.ExecContext(ctx, `DELETE FROM volumes WHERE id = $1`, volumeID)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

func (s *Store) createService(ctx context.Context, userID, environmentID, name string, spec *platformv1.ServiceSpec, agentID string) (serviceRecord, error) {
	var rec serviceRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		rec, err = s.createServiceTx(ctx, tx, userID, environmentID, name, spec, agentID)
		return err
	})
	if err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
}

func (s *Store) createScheduledService(ctx context.Context, userID, environmentID, name string, spec *platformv1.ServiceSpec) (serviceRecord, error) {
	var rec serviceRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		environment, err := s.authorizeEnvironmentWriteQuerier(ctx, tx, userID, environmentID)
		if err != nil {
			return err
		}
		rec, err = s.createStagedServiceTx(ctx, tx, environment, name, spec)
		return err
	})
	if err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
}

func (s *Store) updateService(ctx context.Context, userID, projectID, serviceID, name string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error) {
	var current serviceRecord
	var changed bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		current, changed, _, err = s.updateServiceTx(ctx, tx, userID, projectID, serviceID, name, spec)
		return err
	})
	if err != nil {
		return serviceRecord{}, false, err
	}
	current, err = s.serviceByID(ctx, userID, projectID, serviceID)
	if err != nil {
		return serviceRecord{}, false, err
	}
	return current, changed, nil
}

func (s *Store) updateServiceTx(ctx context.Context, tx *sql.Tx, userID, projectID, serviceID, name string, spec *platformv1.ServiceSpec) (serviceRecord, bool, bool, error) {
	current, err := s.serviceByIDQuerier(ctx, tx, userID, projectID, serviceID)
	if err != nil {
		return serviceRecord{}, false, false, err
	}
	if _, err := s.authorizeEnvironmentWriteQuerier(ctx, tx, userID, current.EnvironmentID); err != nil {
		return serviceRecord{}, false, false, err
	}
	nextName := strings.TrimSpace(name)
	if nextName == "" {
		nextName = current.Name
	}
	if volumeName := serviceVolumeName(spec); volumeName != "" {
		if err := s.requireVolumeQuerier(ctx, tx, current.EnvironmentID, volumeName); err != nil {
			return serviceRecord{}, false, false, err
		}
	}
	pendingReplicas := specReplicaCount(spec, current.DesiredReplicaCount)
	if live := current.DesiredReplicaCount; live > pendingReplicas {
		pendingReplicas = live
	}
	if err := validateVolumeReplicaCompatibility(spec, pendingReplicas); err != nil {
		return serviceRecord{}, false, false, err
	}
	spec = canonicalServiceSpec(spec)
	if err := validateServicePlacement(spec); err != nil {
		return serviceRecord{}, false, false, err
	}
	nameChanged := nextName != current.Name
	if sameServiceSpec(current.Spec, spec) {
		if !nameChanged {
			return current, false, false, nil
		}
		now := time.Now().UTC()
		result, err := tx.ExecContext(ctx,
			`UPDATE services
			    SET name = $1,
			        updated_at = $2
			  WHERE id = $3
			    AND current_spec_revision = $4
			    AND current_rollout_generation = $5`,
			nextName, now, serviceID, current.SpecRevision, current.RolloutGeneration,
		)
		if err != nil {
			return serviceRecord{}, false, false, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return serviceRecord{}, false, false, err
		}
		if affected == 0 {
			return serviceRecord{}, false, false, errConcurrentUpdate
		}
		current.Name = nextName
		current.UpdatedAt = now
		if err := s.bumpAllDesiredRevisionsTx(ctx, tx); err != nil {
			return serviceRecord{}, false, false, err
		}
		return current, false, false, nil
	}

	now := time.Now().UTC()
	nextSpecRevision := current.SpecRevision + 1
	sourceChanged := desiredSourceSpec(spec) != nil && !sameDesiredSourceSpec(current.Spec, spec)
	specJSON, err := protojson.Marshal(spec)
	if err != nil {
		return serviceRecord{}, false, false, err
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE services
		    SET name = $1,
		        current_spec_revision = $2,
		        updated_at = $3
		  WHERE id = $4
		    AND current_spec_revision = $5
		    AND current_rollout_generation = $6`,
		nextName, nextSpecRevision, now, serviceID, current.SpecRevision, current.RolloutGeneration,
	)
	if err != nil {
		return serviceRecord{}, false, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return serviceRecord{}, false, false, err
	}
	if affected == 0 {
		return serviceRecord{}, false, false, errConcurrentUpdate
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at) VALUES ($1, $2, $3, $4)`,
		serviceID, nextSpecRevision, specJSON, now,
	); err != nil {
		return serviceRecord{}, false, false, err
	}
	nextRecord := current
	nextRecord.Name = nextName
	nextRecord.Spec = spec
	if source := desiredSourceSpec(spec); source != nil {
		nextRecord.SourceSummary = toProtoSourceStateSummary(source, nil, nil, nil)
	} else {
		nextRecord.SourceSummary = buildSourceSummary(spec)
	}
	nextRecord.SpecRevision = nextSpecRevision
	nextRecord.UpdatedAt = now
	nextRecord.PendingChanges = true
	return nextRecord, false, sourceChanged, nil
}

func (s *Store) redeployService(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, error) {
	var (
		current                serviceRecord
		identityCatalogChanged bool
	)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		current, identityCatalogChanged, err = s.redeployServiceTx(ctx, tx, userID, projectID, serviceID)
		if err != nil {
			return err
		}
		if identityCatalogChanged {
			return s.bumpAllDesiredRevisionsTx(ctx, tx)
		}
		return s.bumpDesiredRevisionsTx(ctx, tx, []string{current.AllocatedAgentID})
	})
	if err != nil {
		return serviceRecord{}, err
	}
	return current, nil
}

func (s *Store) restartService(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, error) {
	var current serviceRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		current, err = s.serviceByIDQuerier(ctx, tx, userID, projectID, serviceID)
		if err != nil {
			return err
		}
		if _, err := s.authorizeEnvironmentWriteQuerier(ctx, tx, userID, current.EnvironmentID); err != nil {
			return err
		}
		existing, err := s.listAllocationsByServiceIDQuerier(ctx, tx, serviceID, true)
		if err != nil {
			return err
		}
		if len(existing) == 0 {
			return fmt.Errorf("service %s has no allocation to restart", serviceID)
		}
		now := time.Now().UTC()
		result, err := tx.ExecContext(ctx,
			`UPDATE allocations
			    SET operator_restart_nonce = operator_restart_nonce + 1,
			        phase = 'Pending',
			        message = 'operator restart requested',
			        healthy = FALSE,
			        updated_at = $1
			  WHERE service_id = $2`,
			now, serviceID,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return sql.ErrNoRows
		}
		dep, err := s.insertDeploymentTx(
			ctx, tx, serviceID, deploymentStateStarting,
			deploymentActor{Kind: deploymentCauseUser, ID: userID},
			reasonOperatorRestart,
			"Operator restart requested",
			current.SpecRevision, current.RolloutGeneration,
			current.LatestBuildID, current.ResolvedImage, userID, now,
		)
		if err != nil {
			return err
		}
		current.LatestDeployment = &dep
		return s.bumpDesiredRevisionsTx(ctx, tx, []string{current.AllocatedAgentID})
	})
	if err != nil {
		return serviceRecord{}, err
	}
	return current, nil
}

func (s *Store) requestServiceSourceSync(ctx context.Context, userID, projectID, serviceID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		service, err := s.serviceByIDQuerier(ctx, tx, userID, projectID, serviceID)
		if err != nil {
			return err
		}
		if desiredSourceSpec(service.Spec) == nil {
			return errServiceNotBuildable
		}
		return s.enqueueSourceSpecChangedTx(ctx, tx, service.ID, service.SpecRevision, true)
	})
}

func (s *Store) redeployServiceTx(ctx context.Context, tx *sql.Tx, userID, projectID, serviceID string) (serviceRecord, bool, error) {
	current, err := s.serviceByIDQuerier(ctx, tx, userID, projectID, serviceID)
	if err != nil {
		return serviceRecord{}, false, err
	}
	if _, err := s.authorizeEnvironmentWriteQuerier(ctx, tx, userID, current.EnvironmentID); err != nil {
		return serviceRecord{}, false, err
	}
	if current.DesiredReplicaCount <= 0 {
		current.DesiredReplicaCount = defaultDesiredReplicaCount
	}
	if specHasDesiredReplicaCount(current.Spec) {
		desired := current.Spec.GetDesiredReplicaCount()
		if err := validateDesiredReplicaCount(desired); err != nil {
			return serviceRecord{}, false, err
		}
		if err := validateVolumeReplicaCompatibility(current.Spec, desired); err != nil {
			return serviceRecord{}, false, err
		}
		current.DesiredReplicaCount = desired
	}
	identityCatalogChanged := current.AllocatedAgentID == ""
	preferredAgentID := current.AllocatedAgentID

	now := time.Now().UTC()
	nextRolloutGeneration := current.RolloutGeneration + 1
	resolvedImage := current.ResolvedImage
	if directImage := directImageRef(current.Spec); directImage != "" {
		resolvedImage = directImage
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE services
		    SET current_rollout_generation = $1,
		        current_resolved_image = $2,
		        desired_replica_count = $3,
		        updated_at = $4
		  WHERE id = $5
		    AND current_spec_revision = $6
		    AND current_rollout_generation = $7`,
		nextRolloutGeneration, resolvedImage, current.DesiredReplicaCount, now, serviceID, current.SpecRevision, current.RolloutGeneration,
	)
	if err != nil {
		return serviceRecord{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return serviceRecord{}, false, err
	}
	if affected == 0 {
		return serviceRecord{}, false, errConcurrentUpdate
	}
	if err := s.insertServiceRolloutTx(ctx, tx, serviceID, nextRolloutGeneration, current.SpecRevision, "redeploy", "", userID, now); err != nil {
		return serviceRecord{}, false, err
	}
	redeployState := deploymentStateScheduling
	redeployDetail := "Redeploy scheduled"
	if desiredSourceSpec(current.Spec) != nil && resolvedImage == "" {
		redeployState = deploymentStateStaged
		redeployDetail = "Redeploy staged; waiting for source build"
	}
	dep, err := s.insertDeploymentTx(ctx, tx, serviceID, redeployState, deploymentActor{Kind: deploymentCauseUser, ID: userID}, reasonUserRedeploy, redeployDetail, current.SpecRevision, nextRolloutGeneration, "", resolvedImage, userID, now)
	if err != nil {
		return serviceRecord{}, false, err
	}
	current.LatestDeployment = &dep
	if _, err := tx.ExecContext(ctx,
		`UPDATE allocations
		    SET desired_spec_revision = $1,
		        desired_rollout_generation = $2,
		        phase = 'Pending',
		        message = '',
		        restart_observation_json = '{}',
		        updated_at = $3
		  WHERE service_id = $4`,
		current.SpecRevision, nextRolloutGeneration, now, serviceID,
	); err != nil {
		return serviceRecord{}, false, err
	}

	current.RolloutGeneration = nextRolloutGeneration
	current.ResolvedImage = resolvedImage
	current.PendingChanges = false
	current.UpdatedAt = now
	if _, err := s.reconcileServiceReplicasTx(ctx, tx, current, preferredAgentID, now); err != nil {
		return serviceRecord{}, false, err
	}
	if current.AllocatedAgentID == "" {
		identityCatalogChanged = true
	}
	return current, identityCatalogChanged, nil
}

func (s *Store) deleteService(ctx context.Context, userID, projectID, serviceID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		service, err := s.serviceByIDQuerier(ctx, tx, userID, projectID, serviceID)
		if err != nil {
			return err
		}
		if _, err := s.authorizeEnvironmentWriteQuerier(ctx, tx, userID, service.EnvironmentID); err != nil {
			return err
		}
		if err := s.markCurrentDeploymentRemovedTx(ctx, tx, serviceID, deploymentActor{Kind: deploymentCauseUser, ID: userID}); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM services WHERE id = $1`, serviceID)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return sql.ErrNoRows
		}
		return s.bumpAllDesiredRevisionsTx(ctx, tx)
	})
}

func (s *Store) listServices(ctx context.Context, userID, environmentID string) ([]serviceRecord, error) {
	if _, err := s.environmentByID(ctx, userID, environmentID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		serviceSelectSQL+`
		  WHERE s.environment_id = $1
		  ORDER BY s.created_at ASC`,
		environmentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []serviceRecord
	for rows.Next() {
		rec, err := scanServiceRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		spec, err := s.loadServiceDetails(ctx, out[i].ID, out[i].SpecRevision)
		if err != nil {
			return nil, err
		}
		out[i].Spec = spec
		out[i].SourceSummary, err = s.loadServiceSourceSummaryQuerier(ctx, s.db, spec, out[i].ID)
		if err != nil {
			return nil, err
		}
		if out[i].ResolvedImage == "" && out[i].RolloutGeneration > 0 {
			out[i].ResolvedImage = directImageRef(spec)
		}
		out[i].LatestBuild, err = s.latestBuildForServiceQuerier(ctx, s.db, out[i].LatestBuildID)
		if err != nil {
			return nil, err
		}
		if err := s.attachLatestDeploymentQuerier(ctx, s.db, &out[i]); err != nil {
			return nil, err
		}
		out[i].UnappliedChanges, _, err = s.loadServiceUnappliedChangesQuerier(ctx, s.db, out[i].ID, out[i].Spec, out[i].RolloutGeneration)
		if err != nil {
			return nil, err
		}
		out[i].PendingChanges = len(out[i].UnappliedChanges) > 0
	}
	return out, nil
}

func (s *Store) serviceByID(ctx context.Context, userID, projectID, serviceID string) (serviceRecord, error) {
	return s.serviceByIDQuerier(ctx, s.db, userID, projectID, serviceID)
}

func (s *Store) serviceByIDQuerier(ctx context.Context, q serviceQueryer, userID, _ string, serviceID string) (serviceRecord, error) {
	row := q.QueryRowContext(ctx,
		serviceSelectSQL+`
		   JOIN project_memberships m ON m.project_id = e.project_id
		  WHERE s.id = $1 AND m.user_id = $2 AND m.role IN ('owner', 'editor', 'viewer')`,
		serviceID, userID,
	)
	rec, err := scanServiceRow(row)
	if err != nil {
		return serviceRecord{}, err
	}
	rec.Spec, err = s.loadServiceDetailsQuerier(ctx, q, rec.ID, rec.SpecRevision)
	if err != nil {
		return serviceRecord{}, err
	}
	rec.SourceSummary, err = s.loadServiceSourceSummaryQuerier(ctx, q, rec.Spec, rec.ID)
	if err != nil {
		return serviceRecord{}, err
	}
	if rec.ResolvedImage == "" && rec.RolloutGeneration > 0 {
		rec.ResolvedImage = directImageRef(rec.Spec)
	}
	rec.LatestBuild, err = s.latestBuildForServiceQuerier(ctx, q, rec.LatestBuildID)
	if err != nil {
		return serviceRecord{}, err
	}
	if err := s.attachLatestDeploymentQuerier(ctx, q, &rec); err != nil {
		return serviceRecord{}, err
	}
	rec.UnappliedChanges, _, err = s.loadServiceUnappliedChangesQuerier(ctx, q, rec.ID, rec.Spec, rec.RolloutGeneration)
	if err != nil {
		return serviceRecord{}, err
	}
	rec.PendingChanges = len(rec.UnappliedChanges) > 0
	return rec, nil
}

func (s *Store) serviceByNameQuerier(ctx context.Context, q serviceQueryer, environmentID, name string) (serviceRecord, bool, error) {
	row := q.QueryRowContext(
		ctx,
		serviceSelectSQL+`
		  WHERE s.environment_id = $1 AND s.name = $2`,
		environmentID,
		name,
	)
	rec, err := scanServiceRow(row)
	switch {
	case err == nil:
		rec.Spec, err = s.loadServiceDetailsQuerier(ctx, q, rec.ID, rec.SpecRevision)
		if err != nil {
			return serviceRecord{}, false, err
		}
		return rec, true, nil
	case err == sql.ErrNoRows:
		return serviceRecord{}, false, nil
	default:
		return serviceRecord{}, false, err
	}
}

const serviceSelectSQL = `SELECT s.id, s.environment_id, e.project_id, s.name, s.current_spec_revision,
		        s.current_rollout_generation,
		        COALESCE((SELECT a.agent_id FROM allocations a WHERE a.service_id = s.id ORDER BY a.id LIMIT 1), ''),
		        s.current_resolved_image, s.last_successful_commit_sha, s.latest_build_id,
		        s.desired_replica_count, s.placement_message, s.created_at, s.updated_at
		   FROM services s
		   JOIN environments e ON e.id = s.environment_id`

func scanServiceRow(scanner interface{ Scan(...any) error }) (serviceRecord, error) {
	var rec serviceRecord
	if err := scanner.Scan(
		&rec.ID,
		&rec.EnvironmentID,
		&rec.ProjectID,
		&rec.Name,
		&rec.SpecRevision,
		&rec.RolloutGeneration,
		&rec.AllocatedAgentID,
		&rec.ResolvedImage,
		&rec.LastSuccessfulCommitSHA,
		&rec.LatestBuildID,
		&rec.DesiredReplicaCount,
		&rec.PlacementMessage,
		&rec.CreatedAt,
		&rec.UpdatedAt,
	); err != nil {
		return serviceRecord{}, err
	}
	if rec.DesiredReplicaCount <= 0 {
		rec.DesiredReplicaCount = defaultDesiredReplicaCount
	}
	rec.LatestBuild = nil
	return rec, nil
}

func (s *Store) loadServiceDetails(ctx context.Context, serviceID string, specRevision int64) (*platformv1.ServiceSpec, error) {
	return s.loadServiceDetailsQuerier(ctx, s.db, serviceID, specRevision)
}

func (s *Store) loadServiceDetailsQuerier(ctx context.Context, q serviceQueryer, serviceID string, specRevision int64) (*platformv1.ServiceSpec, error) {
	var rawSpec []byte
	if err := q.QueryRowContext(ctx, `SELECT spec_json FROM service_revisions WHERE service_id = $1 AND spec_revision = $2`, serviceID, specRevision).Scan(&rawSpec); err != nil {
		return nil, err
	}
	return loadServiceSpec(rawSpec)
}
