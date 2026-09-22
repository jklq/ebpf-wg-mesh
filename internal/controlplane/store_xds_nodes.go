package controlplane

import (
	"context"
	"time"

	"ebof-wg-mesh/internal/controlplane/xds"
)

// UpsertNodeObservations records what this replica's Envoy subscribers have
// applied. Empty observations mean "no news" and are retained: while a node
// is mid-apply or NACKing it still routes with the old version it holds,
// and that must keep blocking drains until a full apply lands. The same
// holds for a freshly started replica that has not re-observed a rejection.
func (s *database) UpsertNodeObservations(ctx context.Context, observations []xds.NodeObservation) error {
	now := time.Now().UTC()
	for _, observation := range observations {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO xds_node_observations(node_id, applied_hash, nacks, last_nack, updated_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (node_id) DO UPDATE SET
				applied_hash = CASE WHEN EXCLUDED.applied_hash = '' THEN xds_node_observations.applied_hash ELSE EXCLUDED.applied_hash END,
				nacks = GREATEST(xds_node_observations.nacks, EXCLUDED.nacks),
				last_nack = CASE WHEN EXCLUDED.last_nack = '' THEN xds_node_observations.last_nack ELSE EXCLUDED.last_nack END,
				updated_at = EXCLUDED.updated_at`,
			observation.NodeID, observation.AppliedHash, observation.NACKs, observation.LastNACK, now,
		); err != nil {
			return err
		}
	}
	return nil
}

// ListNodeObservations returns every observed Envoy's durable apply state.
// Rows are never deleted implicitly: a disconnected Envoy keeps serving its
// last-known-good config until it reconnects and applies, so its withdrawals
// must keep waiting for that ACK.
func (s *database) ListNodeObservations(ctx context.Context) ([]xds.NodeObservation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT node_id, applied_hash, nacks, last_nack FROM xds_node_observations ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var observations []xds.NodeObservation
	for rows.Next() {
		var observation xds.NodeObservation
		if err := rows.Scan(&observation.NodeID, &observation.AppliedHash, &observation.NACKs, &observation.LastNACK); err != nil {
			return nil, err
		}
		observations = append(observations, observation)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return observations, nil
}
