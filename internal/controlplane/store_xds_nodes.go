package controlplane

import (
	"context"
	"time"

	"ebof-wg-mesh/internal/controlplane/xds"
)

func (s *database) UpsertNodeObservations(ctx context.Context, observations []xds.NodeObservation) error {
	now := time.Now().UTC()
	for _, observation := range observations {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO xds_node_observations(node_id, applied_version, nacks, last_nack, updated_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (node_id) DO UPDATE SET
				applied_version = CASE WHEN EXCLUDED.applied_version = '' THEN xds_node_observations.applied_version ELSE EXCLUDED.applied_version END,
				nacks = GREATEST(xds_node_observations.nacks, EXCLUDED.nacks),
				last_nack = CASE WHEN EXCLUDED.last_nack = '' THEN xds_node_observations.last_nack ELSE EXCLUDED.last_nack END,
				updated_at = EXCLUDED.updated_at`,
			observation.NodeID, observation.AppliedVersion, observation.NACKs, observation.LastNACK, now,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *database) ListNodeObservations(ctx context.Context) ([]xds.NodeObservation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT node_id, applied_version, nacks, last_nack FROM xds_node_observations ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var observations []xds.NodeObservation
	for rows.Next() {
		var observation xds.NodeObservation
		if err := rows.Scan(&observation.NodeID, &observation.AppliedVersion, &observation.NACKs, &observation.LastNACK); err != nil {
			return nil, err
		}
		observations = append(observations, observation)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return observations, nil
}
