package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

const (
	buildStateQueued     = "queued"
	buildStateRunning    = "running"
	buildStateSucceeded  = "succeeded"
	buildStateFailed     = "failed"
	buildStateSuperseded = "superseded"
)

var errServiceNotBuildable = errors.New("service does not use a build source")
var errSourceStateNotReady = errors.New("source state is not ready")

func (s *Store) latestBuildForServiceQuerier(ctx context.Context, q serviceQueryer, buildID string) (*platformv1.BuildStatus, error) {
	if buildID == "" {
		return nil, nil
	}
	rec, err := s.buildRunByIDQuerier(ctx, q, buildID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return toProtoBuildStatus(rec), nil
}

func (s *Store) buildRunByIDQuerier(ctx context.Context, q serviceQueryer, buildID string) (buildRunRecord, error) {
	var (
		rec        buildRunRecord
		recipeJSON []byte
	)
	err := q.QueryRowContext(ctx,
		`SELECT id, service_id, project_id, commit_sha, state, builder_id, image_digest, failure_reason,
		        source_revision_id, source_snapshot_id, source_snapshot_digest, build_recipe_json,
		        queued_at, started_at, finished_at
		   FROM build_runs
		  WHERE id = $1`,
		buildID,
	).Scan(
		&rec.ID,
		&rec.ServiceID,
		&rec.ProjectID,
		&rec.CommitSHA,
		&rec.State,
		&rec.BuilderID,
		&rec.ImageDigest,
		&rec.FailureReason,
		&rec.SourceRevisionID,
		&rec.SourceSnapshotID,
		&rec.SourceSnapshotDigest,
		&recipeJSON,
		&rec.QueuedAt,
		&rec.StartedAt,
		&rec.FinishedAt,
	)
	if err != nil {
		return buildRunRecord{}, err
	}
	rec.BuildRecipe, err = unmarshalBuildRecipe(recipeJSON)
	if err != nil {
		return buildRunRecord{}, err
	}
	return rec, nil
}

func (s *Store) enqueueBuildForService(ctx context.Context, subject, projectID, serviceID, commitSHA string) (buildRunRecord, error) {
	var rec buildRunRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		service, err := s.serviceByIDQuerier(ctx, tx, subject, projectID, serviceID)
		if err != nil {
			return err
		}
		rec, err = s.enqueueBuildTx(ctx, tx, service, commitSHA)
		return err
	})
	if err != nil {
		return buildRunRecord{}, err
	}
	return rec, nil
}

func (s *Store) enqueueBuildTx(ctx context.Context, tx *sql.Tx, service serviceRecord, commitSHA string) (buildRunRecord, error) {
	spec := desiredSourceSpec(service.Spec)
	if spec == nil {
		return buildRunRecord{}, errServiceNotBuildable
	}
	if commitSHA == "" {
		return buildRunRecord{}, errors.New("commit sha is required")
	}
	binding, err := s.sourceBindingByServiceIDQuerier(ctx, tx, service.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return buildRunRecord{}, errSourceStateNotReady
		}
		return buildRunRecord{}, err
	}
	if binding.AccessState != sourceAccessStateAvailable {
		return buildRunRecord{}, errSourceStateNotReady
	}
	if time.Now().UTC().After(binding.FreshUntil) {
		return buildRunRecord{}, errSourceStateNotReady
	}
	revision, err := s.sourceRevisionByBindingAndCommitTx(ctx, tx, binding.ID, commitSHA)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return buildRunRecord{}, errSourceStateNotReady
		}
		return buildRunRecord{}, err
	}
	snapshot, err := s.sourceSnapshotByRevisionIDTx(ctx, tx, revision.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return buildRunRecord{}, errSourceStateNotReady
		}
		return buildRunRecord{}, err
	}
	return s.enqueueBuildFromSourceStateTx(ctx, tx, service, revision, snapshot, binding.BuildRecipe)
}

func (s *Store) enqueueBuildFromSourceStateTx(ctx context.Context, tx *sql.Tx, service serviceRecord, revision sourceRevisionRecord, snapshot sourceSnapshotRecord, buildRecipe *platformv1.BuildRecipe) (buildRunRecord, error) {
	if revision.ID == "" || snapshot.ID == "" || !sourceSnapshotMatchesRevision(snapshot, revision) {
		return buildRunRecord{}, errSourceStateNotReady
	}
	if err := ensureReadySnapshot(snapshot); err != nil {
		return buildRunRecord{}, err
	}
	existing, err := s.findBuildByServiceAndCommitTx(ctx, tx, service.ID, revision.CommitSHA)
	switch {
	case err == nil:
		return existing, nil
	case !errors.Is(err, sql.ErrNoRows):
		return buildRunRecord{}, err
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx,
		`UPDATE build_runs
		    SET state = $2,
		        finished_at = $3,
		        failure_reason = $4
		  WHERE service_id = $1
		    AND state = $5`,
		service.ID, buildStateSuperseded, now, "superseded by newer queued build", buildStateQueued,
	); err != nil {
		return buildRunRecord{}, err
	}

	rec := buildRunRecord{
		ID:                   mustID(),
		ServiceID:            service.ID,
		ProjectID:            service.ProjectID,
		CommitSHA:            revision.CommitSHA,
		State:                buildStateQueued,
		SourceRevisionID:     revision.ID,
		SourceSnapshotID:     snapshot.ID,
		SourceSnapshotDigest: snapshot.Digest,
		BuildRecipe:          cloneBuildRecipe(buildRecipe),
		QueuedAt:             now,
	}
	recipeJSON, err := marshalBuildRecipe(rec.BuildRecipe)
	if err != nil {
		return buildRunRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO build_runs(
			id, service_id, project_id, commit_sha, state,
			source_revision_id, source_snapshot_id, source_snapshot_digest, build_recipe_json,
			builder_id, queued_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, '', $10)`,
		rec.ID, rec.ServiceID, rec.ProjectID, rec.CommitSHA, rec.State,
		rec.SourceRevisionID, rec.SourceSnapshotID, rec.SourceSnapshotDigest, recipeJSON, rec.QueuedAt,
	); err != nil {
		return buildRunRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE services
		    SET latest_build_id = $1,
		        updated_at = $2
		  WHERE id = $3`,
		rec.ID, now, service.ID,
	); err != nil {
		return buildRunRecord{}, err
	}
	return rec, nil
}

func sourceSnapshotMatchesRevision(snapshot sourceSnapshotRecord, revision sourceRevisionRecord) bool {
	if snapshot.SourceRevisionID == revision.ID {
		return true
	}
	return snapshot.Provider == revision.Provider &&
		snapshot.ProviderRepositoryExternalID == revision.ProviderRepositoryExternalID &&
		snapshot.CommitSHA == revision.CommitSHA
}

func (s *Store) findBuildByServiceAndCommitTx(ctx context.Context, tx *sql.Tx, serviceID, commitSHA string) (buildRunRecord, error) {
	var (
		rec        buildRunRecord
		recipeJSON []byte
	)
	err := tx.QueryRowContext(ctx,
		`SELECT id, service_id, project_id, commit_sha, state, builder_id, image_digest, failure_reason,
		        source_revision_id, source_snapshot_id, source_snapshot_digest, build_recipe_json,
		        queued_at, started_at, finished_at
		   FROM build_runs
		  WHERE service_id = $1 AND commit_sha = $2`,
		serviceID, commitSHA,
	).Scan(
		&rec.ID,
		&rec.ServiceID,
		&rec.ProjectID,
		&rec.CommitSHA,
		&rec.State,
		&rec.BuilderID,
		&rec.ImageDigest,
		&rec.FailureReason,
		&rec.SourceRevisionID,
		&rec.SourceSnapshotID,
		&rec.SourceSnapshotDigest,
		&recipeJSON,
		&rec.QueuedAt,
		&rec.StartedAt,
		&rec.FinishedAt,
	)
	if err != nil {
		return buildRunRecord{}, err
	}
	rec.BuildRecipe, err = unmarshalBuildRecipe(recipeJSON)
	if err != nil {
		return buildRunRecord{}, err
	}
	return rec, nil
}

func (s *Store) claimNextBuild(ctx context.Context, builderID, builderName string, staleAfter time.Duration) (buildRunRecord, error) {
	var (
		rec        buildRunRecord
		recipeJSON []byte
	)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO builder_workers(id, name, current_build_id, last_heartbeat_at, created_at, updated_at)
			 VALUES ($1, $2, '', $3, $3, $3)
			 ON CONFLICT(id) DO UPDATE
			    SET name = excluded.name,
			        last_heartbeat_at = excluded.last_heartbeat_at,
			        updated_at = excluded.updated_at`,
			builderID, builderName, now,
		); err != nil {
			return err
		}
		if staleAfter > 0 {
			if err := s.recoverExpiredBuildsTx(ctx, tx, now.Add(-staleAfter)); err != nil {
				return err
			}
		}
		row := tx.QueryRowContext(ctx,
			`SELECT id, service_id, project_id, commit_sha, state, builder_id, image_digest, failure_reason,
			        source_revision_id, source_snapshot_id, source_snapshot_digest, build_recipe_json,
			        queued_at, started_at, finished_at
			   FROM build_runs
			  WHERE state = $1
			  ORDER BY queued_at ASC, id ASC
			  LIMIT 1`,
			buildStateQueued,
		)
		if err := row.Scan(
			&rec.ID,
			&rec.ServiceID,
			&rec.ProjectID,
			&rec.CommitSHA,
			&rec.State,
			&rec.BuilderID,
			&rec.ImageDigest,
			&rec.FailureReason,
			&rec.SourceRevisionID,
			&rec.SourceSnapshotID,
			&rec.SourceSnapshotDigest,
			&recipeJSON,
			&rec.QueuedAt,
			&rec.StartedAt,
			&rec.FinishedAt,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		rec.State = buildStateRunning
		rec.BuilderID = builderID
		rec.StartedAt = sql.NullTime{Time: now, Valid: true}
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_runs
			    SET state = $1,
			        started_at = $2,
			        builder_id = $3
			  WHERE id = $4`,
			buildStateRunning, now, builderID, rec.ID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE builder_workers
			    SET current_build_id = $1,
			        last_heartbeat_at = $2,
			        updated_at = $2
			  WHERE id = $3`,
			rec.ID, now, builderID,
		); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return buildRunRecord{}, err
	}
	rec.BuildRecipe, err = unmarshalBuildRecipe(recipeJSON)
	if err != nil {
		return buildRunRecord{}, err
	}
	return rec, nil
}

func (s *Store) recoverExpiredBuilds(ctx context.Context, staleAfter time.Duration) error {
	if staleAfter <= 0 {
		return nil
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		return s.recoverExpiredBuildsTx(ctx, tx, time.Now().UTC().Add(-staleAfter))
	})
}

func (s *Store) recoverExpiredBuildsTx(ctx context.Context, tx *sql.Tx, cutoff time.Time) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT b.id, b.service_id, b.queued_at
		   FROM build_runs b
		   JOIN builder_workers w ON w.id = b.builder_id
		  WHERE b.state = $1
		    AND w.last_heartbeat_at < $2
		  ORDER BY b.queued_at ASC, b.id ASC`,
		buildStateRunning, cutoff,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	type expiredBuild struct {
		ID        string
		ServiceID string
		QueuedAt  time.Time
	}
	var expired []expiredBuild
	for rows.Next() {
		var rec expiredBuild
		if err := rows.Scan(&rec.ID, &rec.ServiceID, &rec.QueuedAt); err != nil {
			return err
		}
		expired = append(expired, rec)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, rec := range expired {
		var newerCount int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*)
			   FROM build_runs
			  WHERE service_id = $1
			    AND queued_at > $2
			    AND state IN ($3, $4, $5)`,
			rec.ServiceID, rec.QueuedAt, buildStateQueued, buildStateRunning, buildStateSucceeded,
		).Scan(&newerCount); err != nil {
			return err
		}
		nextState := buildStateQueued
		failureReason := ""
		startedAt := any(nil)
		if newerCount > 0 {
			nextState = buildStateSuperseded
			failureReason = "superseded after stale builder heartbeat"
			startedAt = nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_runs
			    SET state = $1,
			        started_at = $2,
			        finished_at = CASE WHEN $1 = $3 THEN $4 ELSE NULL END,
			        builder_id = '',
			        failure_reason = $5
			  WHERE id = $6`,
			nextState, startedAt, buildStateSuperseded, now, failureReason, rec.ID,
		); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE builder_workers
		    SET current_build_id = '',
		        updated_at = $1
		  WHERE last_heartbeat_at < $2`,
		now, cutoff,
	); err != nil {
		return err
	}
	return nil
}

func (s *Store) recordBuilderHeartbeat(ctx context.Context, builderID, buildID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE builder_workers
		    SET current_build_id = $1,
		        last_heartbeat_at = $2,
		        updated_at = $2
		  WHERE id = $3`,
		buildID, time.Now().UTC(), builderID,
	)
	return err
}

func (s *Store) completeBuild(ctx context.Context, builderID, buildID string, state platformv1.BuildState, commitSHA, imageDigest, failureReason string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		build, err := s.buildRunByIDQuerier(ctx, tx, buildID)
		if err != nil {
			return err
		}
		if build.State == buildStateSucceeded || build.State == buildStateFailed || build.State == buildStateSuperseded {
			return nil
		}
		now := time.Now().UTC()
		stateValue := buildStateFailed
		switch state {
		case platformv1.BuildState_BUILD_STATE_SUCCEEDED:
			stateValue = buildStateSucceeded
		case platformv1.BuildState_BUILD_STATE_SUPERSEDED:
			stateValue = buildStateSuperseded
		case platformv1.BuildState_BUILD_STATE_FAILED:
			stateValue = buildStateFailed
		default:
			return errors.New("invalid terminal build state")
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_runs
			    SET state = $1,
			        commit_sha = $2,
			        image_digest = $3,
			        failure_reason = $4,
			        finished_at = $5,
			        builder_id = $6
			  WHERE id = $7`,
			stateValue, commitSHA, imageDigest, failureReason, now, builderID, buildID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE builder_workers
			    SET current_build_id = '',
			        last_heartbeat_at = $1,
			        updated_at = $1
			  WHERE id = $2`,
			now, builderID,
		); err != nil {
			return err
		}

		if stateValue != buildStateSucceeded {
			return nil
		}
		if build.SourceRevisionID == "" || build.SourceSnapshotID == "" || build.SourceSnapshotDigest == "" {
			return errSourceStateNotReady
		}
		var newerCount int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*)
			   FROM build_runs
			  WHERE service_id = $1
			    AND queued_at > $2
			    AND state IN ($3, $4, $5)`,
			build.ServiceID, build.QueuedAt, buildStateQueued, buildStateRunning, buildStateSucceeded,
		).Scan(&newerCount); err != nil {
			return err
		}
		if newerCount > 0 {
			return nil
		}

		service, err := s.serviceByIDInternalQuerier(ctx, tx, build.ServiceID)
		if err != nil {
			return err
		}
		nextRolloutGeneration := service.RolloutGeneration + 1
		if _, err := tx.ExecContext(ctx,
			`UPDATE services
			    SET current_resolved_image = $1,
			        last_successful_commit_sha = $2,
			        current_rollout_generation = $3,
			        latest_build_id = $4,
			        updated_at = $5
			  WHERE id = $6`,
			imageDigest, commitSHA, nextRolloutGeneration, buildID, now, build.ServiceID,
		); err != nil {
			return err
		}
		if err := s.insertServiceRolloutTx(ctx, tx, build.ServiceID, nextRolloutGeneration, service.SpecRevision, "build-success", "", "", now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE allocations
			    SET desired_rollout_generation = $1,
			        phase = $2,
			        message = $3,
			        healthy = $4,
			        updated_at = $5
			  WHERE service_id = $6`,
			nextRolloutGeneration, "Pending", "", false, now, build.ServiceID,
		); err != nil {
			return err
		}
		return s.bumpDesiredRevisionsTx(ctx, tx, []string{service.AllocatedAgentID})
	})
}

func (s *Store) serviceByIDInternalQuerier(ctx context.Context, q serviceQueryer, serviceID string) (serviceRecord, error) {
	row := q.QueryRowContext(ctx,
		`SELECT id, project_id, name, current_spec_revision, current_rollout_generation, allocated_agent_id, current_resolved_image, last_successful_commit_sha, latest_build_id, created_at, updated_at
		   FROM services
		  WHERE id = $1`,
		serviceID,
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
	rec.LatestBuild, err = s.latestBuildForServiceQuerier(ctx, q, rec.LatestBuildID)
	if err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
}
