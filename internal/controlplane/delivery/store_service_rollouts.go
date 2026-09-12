package delivery

import (
	"context"
	"database/sql"
	"time"

	"ebof-wg-mesh/internal/controlplane/journal"

	"google.golang.org/protobuf/encoding/protojson"
)

const (
	rolloutStatePendingBuild = "pending_build"
	rolloutStateInProgress   = "in_progress"
	rolloutStateSucceeded    = "succeeded"
	rolloutStateFailed       = "failed"
	rolloutStateSuperseded   = "superseded"
)

func (s *persistence) insertServiceRolloutTx(
	ctx context.Context,
	tx *sql.Tx,
	serviceID string,
	rolloutGeneration int64,
	specRevision int64,
	reason, buildID, requestedByUserID string,
	now time.Time,
) error {
	var rawSpec []byte
	var desired int32
	var image string
	if err := tx.QueryRowContext(ctx,
		`SELECT r.spec_json, s.desired_replica_count, COALESCE(ds.current_resolved_image, '')
		   FROM services s
		   JOIN service_delivery_status ds ON ds.service_id = s.id
		   JOIN service_revisions r ON r.service_id = s.id AND r.spec_revision = $2
		  WHERE s.id = $1`, serviceID, specRevision,
	).Scan(&rawSpec, &desired, &image); err != nil {
		return err
	}
	spec, err := LoadServiceSpec(rawSpec)
	if err != nil {
		return err
	}
	strategyJSON, err := protojson.Marshal(canonicalRollingStrategy(spec.GetRollingStrategy()))
	if err != nil {
		return err
	}
	state := rolloutStateInProgress
	if image == "" {
		state = rolloutStatePendingBuild
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO service_rollouts(
			service_id, rollout_generation, spec_revision, reason, build_id, requested_by_user_id,
			state, strategy_json, desired_replica_count, image_digest, failure_reason, created_at, progress_at
		) VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, $8, $9, $10, '', $11, $11)`,
		serviceID, rolloutGeneration, specRevision, reason, buildID, requestedByUserID,
		state, strategyJSON, desired, image, now,
	); err != nil {
		return err
	}
	journal.RecordRollout(ctx, serviceID, rolloutGeneration)
	return nil
}
