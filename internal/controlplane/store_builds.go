package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
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
