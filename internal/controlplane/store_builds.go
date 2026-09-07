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
	buildStateCancelled  = "cancelled"
)

var errServiceNotBuildable = errors.New("service does not use a build source")
var errSourceStateNotReady = errors.New("source state is not ready")
var errBuildNotOwned = errors.New("build is not assigned to builder")

func (s *Store) builderOwnsSourceSnapshot(ctx context.Context, builderID, snapshotID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM build_runs
		  WHERE builder_id = $1 AND source_snapshot_id = $2 AND state = $3`,
		builderID, snapshotID, buildStateRunning,
	).Scan(&count)
	return count > 0, err
}

const buildRunSelectColumns = `id, service_id, commit_sha, commit_message, commit_author, state, builder_id, image_digest, failure_reason,
	        source_revision_id, source_snapshot_id, source_snapshot_digest, target_rollout_generation, build_recipe_json,
	        queued_at, started_at, finished_at`

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
	rec, err := scanBuildRunRow(q.QueryRowContext(ctx,
		`SELECT `+buildRunSelectColumns+`
		   FROM build_runs
		  WHERE id = $1`,
		buildID,
	))
	if err != nil {
		return buildRunRecord{}, err
	}
	err = q.QueryRowContext(ctx, `SELECT e.project_id, s.environment_id FROM services s
		JOIN environments e ON e.id = s.environment_id WHERE s.id = $1`, rec.ServiceID).
		Scan(&rec.ProjectID, &rec.EnvironmentID)
	return rec, err
}

func sourceSnapshotMatchesRevision(snapshot sourceSnapshotRecord, revision sourceRevisionRecord) bool {
	if snapshot.SourceRevisionID == revision.ID {
		return true
	}
	return snapshot.Provider == revision.Provider &&
		snapshot.ProviderRepositoryExternalID == revision.ProviderRepositoryExternalID &&
		snapshot.CommitSHA == revision.CommitSHA
}

func (s *Store) recordBuilderHeartbeat(ctx context.Context, builderID, buildID string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE builder_workers
		    SET current_build_id = $1,
		        last_heartbeat_at = $2,
		        updated_at = $2
		  WHERE id = $3
		    AND current_build_id = $1
		    AND EXISTS (
		        SELECT 1 FROM build_runs
		         WHERE id = $1 AND builder_id = $3 AND state = $4
		    )`,
		buildID, time.Now().UTC(), builderID, buildStateRunning,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errBuildNotOwned
	}
	return nil
}

func buildStateTerminal(state string) bool {
	switch state {
	case buildStateSucceeded, buildStateFailed, buildStateSuperseded, buildStateCancelled:
		return true
	default:
		return false
	}
}

func (s *Store) serviceByIDInternalQuerier(ctx context.Context, q serviceQueryer, serviceID string) (serviceRecord, error) {
	row := q.QueryRowContext(ctx,
		serviceSelectSQL+` WHERE s.id = $1`,
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
	if err := s.attachLatestDeploymentQuerier(ctx, q, &rec); err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
}
