package delivery

import (
	"context"
	"database/sql"
	"errors"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/source"
)

const (
	BuildStateQueued     = "queued"
	BuildStateRunning    = "running"
	BuildStateSucceeded  = "succeeded"
	BuildStateFailed     = "failed"
	BuildStateSuperseded = "superseded"
	BuildStateCancelled  = "cancelled"
)

var errServiceNotBuildable = errors.New("service does not use a build source")

var errSourceStateNotReady = errors.New("source state is not ready")

var ErrBuildNotOwned = errors.New("build is not assigned to builder")

var ErrBuildLeaseLost = errors.New("build lease lost: owner epoch is stale")

var ErrBuildCancelled = errors.New("build cancellation was requested")

const (
	BuildAttemptLeased         = "leased"
	BuildAttemptSucceeded      = "succeeded"
	BuildAttemptFailedTerminal = "failed_terminal"
	BuildAttemptWorkerLost     = "worker_lost"
	BuildAttemptCancelled      = "cancelled"
	BuildAttemptTimedOut       = "timed_out"
	BuildAttemptSuperseded     = "superseded"
)

// Attempts record claims, so a build that expires in queue without ever being
// claimed has no attempt rows; its build_runs row carries the terminal reason.

const buildRunSelectColumns = `id, service_id, commit_sha, commit_message, commit_author, state, COALESCE(builder_id, ''), owner_epoch, lease_expires_at,
		attempt_count, attempt_limit, cancel_requested_at, deadline_at, last_heartbeat_at,
		image_digest, failure_reason,
	        COALESCE(source_revision_id, ''), COALESCE(source_snapshot_id, ''), source_snapshot_digest, target_rollout_generation, build_recipe_json,
	        queued_at, started_at, finished_at`

func (s *persistence) latestBuildForServiceQuerier(ctx context.Context, q ServiceQueryer, buildID string) (*platformv1.BuildStatus, error) {
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
	return ToProtoBuildStatus(rec), nil
}

func (s *persistence) buildRunByIDQuerier(ctx context.Context, q ServiceQueryer, buildID string) (BuildRunRecord, error) {
	rec, err := scanBuildRunRow(q.QueryRowContext(ctx,
		`SELECT `+buildRunSelectColumns+`
		   FROM build_runs
		  WHERE id = $1`,
		buildID,
	))
	if err != nil {
		return BuildRunRecord{}, err
	}
	err = q.QueryRowContext(ctx, `SELECT e.project_id, s.environment_id FROM services s
		JOIN environments e ON e.id = s.environment_id WHERE s.id = $1`, rec.ServiceID).
		Scan(&rec.ProjectID, &rec.EnvironmentID)
	return rec, err
}

func sourceSnapshotMatchesRevision(snapshot source.SourceSnapshotRecord, revision source.SourceRevisionRecord) bool {
	if snapshot.SourceRevisionID == revision.ID {
		return true
	}
	return snapshot.Provider == revision.Provider &&
		snapshot.ProviderRepositoryExternalID == revision.ProviderRepositoryExternalID &&
		snapshot.CommitSHA == revision.CommitSHA
}

func BuildStateTerminal(state string) bool {
	switch state {
	case BuildStateSucceeded, BuildStateFailed, BuildStateSuperseded, BuildStateCancelled:
		return true
	default:
		return false
	}
}

func (s *persistence) serviceByIDInternalQuerier(ctx context.Context, q ServiceQueryer, serviceID string) (ServiceRecord, error) {
	row := q.QueryRowContext(ctx,
		serviceSelectSQL+` WHERE s.id = $1`,
		serviceID,
	)
	rec, err := scanServiceRow(row)
	if err != nil {
		return ServiceRecord{}, err
	}
	rec.Spec, err = s.loadServiceDetailsQuerier(ctx, q, rec.ID, rec.SpecRevision)
	if err != nil {
		return ServiceRecord{}, err
	}
	rec.SourceSummary, err = s.loadServiceSourceSummaryQuerier(ctx, q, rec.Spec, rec.ID)
	if err != nil {
		return ServiceRecord{}, err
	}
	rec.LatestBuild, err = s.latestBuildForServiceQuerier(ctx, q, rec.LatestBuildID)
	if err != nil {
		return ServiceRecord{}, err
	}
	if err := s.attachLatestDeploymentQuerier(ctx, q, &rec); err != nil {
		return ServiceRecord{}, err
	}
	return rec, nil
}
