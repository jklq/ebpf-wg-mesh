package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"ebof-wg-mesh/internal/controlplane/xds"
)

// LoadPublication returns the last published xDS version row, or the zero
// Publication when nothing has been published yet.
func (s *database) LoadPublication(ctx context.Context) (xds.Publication, error) {
	var pub xds.Publication
	err := s.db.QueryRowContext(ctx, `SELECT version, hash, publisher FROM xds_publications WHERE id = TRUE`).Scan(
		&pub.Version, &pub.Hash, &pub.Publisher,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return xds.Publication{}, nil
	}
	if err != nil {
		return xds.Publication{}, err
	}
	return pub, nil
}

// CompareAndSwapPublication records a published snapshot only when the
// stored hash still equals oldHash (empty matches an absent row). It reports
// whether this replica won the write; losers reload and adopt the winner's
// identical bytes.
func (s *database) CompareAndSwapPublication(ctx context.Context, oldHash, version, hash string, counts xds.Counts, publisher string) (bool, error) {
	now := time.Now().UTC()
	if oldHash == "" {
		result, err := s.db.ExecContext(ctx, `INSERT INTO xds_publications(
				id, version, hash, listeners, clusters, endpoints, domains, publisher, updated_at
			) VALUES (TRUE, $1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (id) DO NOTHING`,
			version, hash, counts.Listeners, counts.Clusters, counts.Endpoints, counts.Domains, publisher, now,
		)
		if err != nil {
			return false, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		return affected == 1, nil
	}
	result, err := s.db.ExecContext(ctx, `UPDATE xds_publications
		SET version = $1, hash = $2, listeners = $3, clusters = $4, endpoints = $5,
			domains = $6, publisher = $7, updated_at = $8
		WHERE id = TRUE AND hash = $9`,
		version, hash, counts.Listeners, counts.Clusters, counts.Endpoints, counts.Domains, publisher, now, oldHash,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}
