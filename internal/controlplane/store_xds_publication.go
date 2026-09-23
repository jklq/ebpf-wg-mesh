package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"ebof-wg-mesh/internal/controlplane/xds"
)

func (s *database) LoadPublication(ctx context.Context) (xds.Publication, error) {
	var pub xds.Publication
	err := s.db.QueryRowContext(ctx, `SELECT version, inputs, publisher FROM xds_publications WHERE id = TRUE`).Scan(
		&pub.Version, &pub.Inputs, &pub.Publisher,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return xds.Publication{}, nil
	}
	if err != nil {
		return xds.Publication{}, err
	}
	return pub, nil
}

func (s *database) CompareAndSwapPublication(ctx context.Context, oldVersion string, pub xds.Publication) (bool, error) {
	now := time.Now().UTC()
	if oldVersion == "" {
		result, err := s.db.ExecContext(ctx, `INSERT INTO xds_publications(
				id, version, inputs, publisher, updated_at
			) VALUES (TRUE, $1, $2, $3, $4) ON CONFLICT (id) DO NOTHING`,
			pub.Version, pub.Inputs, pub.Publisher, now,
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
		SET version = $1, inputs = $2, publisher = $3, updated_at = $4
		WHERE id = TRUE AND version = $5`,
		pub.Version, pub.Inputs, pub.Publisher, now, oldVersion,
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
