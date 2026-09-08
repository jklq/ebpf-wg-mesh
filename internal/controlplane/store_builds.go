package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"time"
)

func (s *buildsPersistence) builderOwnsSourceSnapshot(ctx context.Context, builderID, snapshotID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM build_runs
		  WHERE builder_id = $1 AND source_snapshot_id = $2 AND state = $3`,
		builderID, snapshotID, deliverycore.BuildStateRunning,
	).Scan(&count)
	return count > 0, err
}

func (s *buildsPersistence) recordBuilderHeartbeat(ctx context.Context, builderID, buildID string) error {
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
		buildID, time.Now().UTC(), builderID, deliverycore.BuildStateRunning,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return deliverycore.ErrBuildNotOwned
	}
	return nil
}
