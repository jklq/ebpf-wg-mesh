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
	reason, buildID, requestedBySubject, requestedByEmail string,
	now time.Time,
) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO service_rollouts(
			service_id, rollout_generation, spec_revision, reason, build_id, requested_by_subject, requested_by_email, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		serviceID, rolloutGeneration, specRevision, reason, buildID, requestedBySubject, requestedByEmail, now,
	)
	return err
}

func (s *Store) createVolume(ctx context.Context, subject, projectID, name string, sizeBytes int64, agentID string) (volumeRecord, error) {
	var rec volumeRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		rec, err = s.createVolumeTx(ctx, tx, subject, projectID, name, sizeBytes, agentID)
		return err
	})
	if err != nil {
		return volumeRecord{}, err
	}
	return rec, nil
}

func (s *Store) createScheduledVolume(ctx context.Context, subject, projectID, name string, sizeBytes int64) (volumeRecord, error) {
	var rec volumeRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		agentID, err := s.chooseAgentForVolumeTx(ctx, tx)
		if err != nil {
			return err
		}
		rec, err = s.createVolumeTx(ctx, tx, subject, projectID, name, sizeBytes, agentID)
		return err
	})
	if err != nil {
		return volumeRecord{}, err
	}
	return rec, nil
}

func (s *Store) chooseAgentForVolume(ctx context.Context) (string, error) {
	return s.chooseAgentForPlacementQuerier(ctx, s.db, nil)
}

func (s *Store) chooseAgentForService(ctx context.Context, projectID string, spec *platformv1.ServiceSpec) (string, error) {
	if volumeName := serviceVolumeName(spec); volumeName != "" {
		return s.boundAgentForVolume(ctx, projectID, volumeName)
	}
	return s.chooseAgentForPlacementQuerier(ctx, s.db, spec)
}

func (s *Store) listVolumes(ctx context.Context, subject, projectID string) ([]volumeRecord, error) {
	if _, err := s.projectByID(ctx, subject, projectID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, name, size_bytes, bound_agent_id, created_at
		   FROM volumes
		  WHERE project_id = $1
		  ORDER BY created_at ASC`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []volumeRecord
	for rows.Next() {
		var rec volumeRecord
		if err := rows.Scan(&rec.ID, &rec.ProjectID, &rec.Name, &rec.SizeBytes, &rec.BoundAgentID, &rec.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) deleteVolume(ctx context.Context, subject, projectID, volumeID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.projectByIDQuerier(ctx, tx, subject, projectID); err != nil {
			return err
		}
		var (
			volumeName   string
			boundAgentID string
		)
		err := tx.QueryRowContext(ctx, `SELECT name, bound_agent_id FROM volumes WHERE id = $1 AND project_id = $2`, volumeID, projectID).Scan(&volumeName, &boundAgentID)
		if err != nil {
			return err
		}

		rows, err := tx.QueryContext(ctx,
			`SELECT s.id, r.spec_json
			   FROM services s
			   JOIN service_revisions r ON r.service_id = s.id AND r.spec_revision = s.current_spec_revision
			  WHERE s.project_id = $1`,
			projectID,
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

		result, err := tx.ExecContext(ctx, `DELETE FROM volumes WHERE id = $1 AND project_id = $2`, volumeID, projectID)
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
		return s.bumpDesiredRevisionsTx(ctx, tx, []string{boundAgentID})
	})
}

func (s *Store) createService(ctx context.Context, subject, projectID, name string, spec *platformv1.ServiceSpec, agentID string) (serviceRecord, error) {
	var rec serviceRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		rec, err = s.createServiceTx(ctx, tx, subject, projectID, name, spec, agentID)
		return err
	})
	if err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
}

func (s *Store) createScheduledService(ctx context.Context, subject, projectID, name string, spec *platformv1.ServiceSpec) (serviceRecord, error) {
	var rec serviceRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		agentID, err := s.chooseAgentForServiceTx(ctx, tx, projectID, spec)
		if err != nil {
			return err
		}
		rec, err = s.createServiceTx(ctx, tx, subject, projectID, name, spec, agentID)
		return err
	})
	if err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
}

func (s *Store) updateService(ctx context.Context, subject, projectID, serviceID, name string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error) {
	var current serviceRecord
	var changed bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var sourceChanged bool
		var err error
		current, changed, sourceChanged, err = s.updateServiceTx(ctx, tx, subject, projectID, serviceID, name, spec)
		_ = sourceChanged
		return err
	})
	if err != nil {
		return serviceRecord{}, false, err
	}
	current, err = s.serviceByID(ctx, subject, projectID, serviceID)
	if err != nil {
		return serviceRecord{}, false, err
	}
	return current, changed, nil
}

func (s *Store) updateServiceTx(ctx context.Context, tx *sql.Tx, subject, projectID, serviceID, name string, spec *platformv1.ServiceSpec) (serviceRecord, bool, bool, error) {
	current, err := s.serviceByIDQuerier(ctx, tx, subject, projectID, serviceID)
	if err != nil {
		return serviceRecord{}, false, false, err
	}
	nextName := strings.TrimSpace(name)
	if nextName == "" {
		nextName = current.Name
	}
	if volumeName := serviceVolumeName(spec); volumeName != "" {
		volumeAgentID, err := s.boundAgentForVolumeQuerier(ctx, tx, projectID, volumeName)
		if err != nil {
			return serviceRecord{}, false, false, err
		}
		if volumeAgentID != current.AllocatedAgentID {
			return serviceRecord{}, false, false, fmt.Errorf("%w: volume %q is bound to %s, service is allocated to %s", errVolumeAgentMismatch, volumeName, volumeAgentID, current.AllocatedAgentID)
		}
	}
	spec = canonicalServiceSpec(spec)
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

func (s *Store) redeployService(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, error) {
	var current serviceRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		current, err = s.redeployServiceTx(ctx, tx, subject, projectID, serviceID)
		return err
	})
	if err != nil {
		return serviceRecord{}, err
	}
	return current, nil
}

func (s *Store) requestServiceSourceSync(ctx context.Context, subject, projectID, serviceID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		service, err := s.serviceByIDQuerier(ctx, tx, subject, projectID, serviceID)
		if err != nil {
			return err
		}
		if desiredSourceSpec(service.Spec) == nil {
			return errServiceNotBuildable
		}
		return s.enqueueSourceSpecChangedTx(ctx, tx, service.ID, service.SpecRevision, true)
	})
}

func (s *Store) redeployServiceTx(ctx context.Context, tx *sql.Tx, subject, projectID, serviceID string) (serviceRecord, error) {
	current, err := s.serviceByIDQuerier(ctx, tx, subject, projectID, serviceID)
	if err != nil {
		return serviceRecord{}, err
	}

	now := time.Now().UTC()
	nextRolloutGeneration := current.RolloutGeneration + 1
	result, err := tx.ExecContext(ctx,
		`UPDATE services
		    SET current_rollout_generation = $1,
		        updated_at = $2
		  WHERE id = $3
		    AND current_spec_revision = $4
		    AND current_rollout_generation = $5`,
		nextRolloutGeneration, now, serviceID, current.SpecRevision, current.RolloutGeneration,
	)
	if err != nil {
		return serviceRecord{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return serviceRecord{}, err
	}
	if affected == 0 {
		return serviceRecord{}, errConcurrentUpdate
	}
	if err := s.insertServiceRolloutTx(ctx, tx, serviceID, nextRolloutGeneration, current.SpecRevision, "redeploy", "", subject, "", now); err != nil {
		return serviceRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE allocations
		    SET desired_spec_revision = $1,
		        desired_rollout_generation = $2,
		        phase = $3,
		        message = $4,
		        healthy = $5,
		        updated_at = $6
		  WHERE service_id = $7`,
		current.SpecRevision, nextRolloutGeneration, "Pending", "", false, now, serviceID,
	); err != nil {
		return serviceRecord{}, err
	}
	if err := s.bumpDesiredRevisionsTx(ctx, tx, []string{current.AllocatedAgentID}); err != nil {
		return serviceRecord{}, err
	}

	current.RolloutGeneration = nextRolloutGeneration
	current.PendingChanges = false
	current.UpdatedAt = now
	return current, nil
}

func (s *Store) deleteService(ctx context.Context, subject, projectID, serviceID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.projectByIDQuerier(ctx, tx, subject, projectID); err != nil {
			return err
		}
		var agentID string
		if err := tx.QueryRowContext(ctx, `SELECT allocated_agent_id FROM services WHERE id = $1 AND project_id = $2`, serviceID, projectID).Scan(&agentID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM services WHERE id = $1 AND project_id = $2`, serviceID, projectID)
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
		return s.bumpDesiredRevisionsTx(ctx, tx, []string{agentID})
	})
}

func (s *Store) listServices(ctx context.Context, subject, projectID string) ([]serviceRecord, error) {
	if _, err := s.projectByID(ctx, subject, projectID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, name, current_spec_revision, current_rollout_generation, allocated_agent_id, current_resolved_image, last_successful_commit_sha, latest_build_id, created_at, updated_at
		   FROM services
		  WHERE project_id = $1
		  ORDER BY created_at ASC`,
		projectID,
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
		if out[i].ResolvedImage == "" {
			out[i].ResolvedImage = directImageRef(spec)
		}
		out[i].LatestBuild, err = s.latestBuildForServiceQuerier(ctx, s.db, out[i].LatestBuildID)
		if err != nil {
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

func (s *Store) serviceByID(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, error) {
	return s.serviceByIDQuerier(ctx, s.db, subject, projectID, serviceID)
}

func (s *Store) serviceByIDQuerier(ctx context.Context, q serviceQueryer, subject, projectID, serviceID string) (serviceRecord, error) {
	if _, err := s.projectByIDQuerier(ctx, q, subject, projectID); err != nil {
		return serviceRecord{}, err
	}
	row := q.QueryRowContext(ctx,
		`SELECT id, project_id, name, current_spec_revision, current_rollout_generation, allocated_agent_id, current_resolved_image, last_successful_commit_sha, latest_build_id, created_at, updated_at
		   FROM services
		  WHERE id = $1 AND project_id = $2`,
		serviceID, projectID,
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
	if rec.ResolvedImage == "" {
		rec.ResolvedImage = directImageRef(rec.Spec)
	}
	rec.LatestBuild, err = s.latestBuildForServiceQuerier(ctx, q, rec.LatestBuildID)
	if err != nil {
		return serviceRecord{}, err
	}
	rec.UnappliedChanges, _, err = s.loadServiceUnappliedChangesQuerier(ctx, q, rec.ID, rec.Spec, rec.RolloutGeneration)
	if err != nil {
		return serviceRecord{}, err
	}
	rec.PendingChanges = len(rec.UnappliedChanges) > 0
	return rec, nil
}

func (s *Store) serviceByNameQuerier(ctx context.Context, q serviceQueryer, projectID, name string) (serviceRecord, bool, error) {
	row := q.QueryRowContext(
		ctx,
		`SELECT id, project_id, name, current_spec_revision, current_rollout_generation, allocated_agent_id, current_resolved_image, last_successful_commit_sha, latest_build_id, created_at, updated_at
		   FROM services
		  WHERE project_id = $1 AND name = $2`,
		projectID,
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

func scanServiceRow(scanner interface{ Scan(...any) error }) (serviceRecord, error) {
	var rec serviceRecord
	if err := scanner.Scan(
		&rec.ID,
		&rec.ProjectID,
		&rec.Name,
		&rec.SpecRevision,
		&rec.RolloutGeneration,
		&rec.AllocatedAgentID,
		&rec.ResolvedImage,
		&rec.LastSuccessfulCommitSHA,
		&rec.LatestBuildID,
		&rec.CreatedAt,
		&rec.UpdatedAt,
	); err != nil {
		return serviceRecord{}, err
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
