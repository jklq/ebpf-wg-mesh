package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"fmt"
)

func (s *catalogPersistence) createVolume(ctx context.Context, userID, environmentID, name string, sizeBytes int64, _ string) (deliverycore.VolumeRecord, error) {
	var rec deliverycore.VolumeRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		rec, err = s.createVolumeTx(ctx, tx, userID, environmentID, name, sizeBytes)
		return err
	})
	if err != nil {
		return deliverycore.VolumeRecord{}, err
	}
	return rec, nil
}

func (s *catalogPersistence) createScheduledVolume(ctx context.Context, userID, environmentID, name string, sizeBytes int64) (deliverycore.VolumeRecord, error) {
	var rec deliverycore.VolumeRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		rec, err = s.createVolumeTx(ctx, tx, userID, environmentID, name, sizeBytes)
		return err
	})
	if err != nil {
		return deliverycore.VolumeRecord{}, err
	}
	return rec, nil
}

func (s *catalogPersistence) listVolumes(ctx context.Context, userID, environmentID string) ([]deliverycore.VolumeRecord, error) {
	if _, err := s.reads.deliveryQueries().EnvironmentByID(ctx, userID, environmentID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, environment_id, name, size_bytes, created_at
		   FROM volumes
		  WHERE environment_id = $1
		  ORDER BY created_at ASC`,
		environmentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []deliverycore.VolumeRecord
	for rows.Next() {
		var rec deliverycore.VolumeRecord
		if err := rows.Scan(&rec.ID, &rec.EnvironmentID, &rec.Name, &rec.SizeBytes, &rec.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *catalogPersistence) deleteVolume(ctx context.Context, userID, volumeID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var (
			volumeName    string
			environmentID string
		)
		err := tx.QueryRowContext(ctx, `SELECT name, environment_id FROM volumes WHERE id = $1`, volumeID).Scan(&volumeName, &environmentID)
		if err != nil {
			return err
		}
		if _, err := s.reads.deliveryQueries().AuthorizeEnvironmentWriteQuerier(ctx, tx, userID, environmentID); err != nil {
			return err
		}

		rows, err := tx.QueryContext(ctx,
			`SELECT s.id, r.spec_json
			   FROM services s
			   JOIN service_revisions r ON r.service_id = s.id AND r.spec_revision = s.current_spec_revision
			  WHERE s.environment_id = $1`,
			environmentID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var serviceID string
			var rawSpec []byte
			if err := rows.Scan(&serviceID, &rawSpec); err != nil {
				return err
			}
			spec, err := deliverycore.LoadServiceSpec(rawSpec)
			if err != nil {
				return err
			}
			if deliverycore.ServiceVolumeName(spec) == volumeName {
				return fmt.Errorf("%w: service %s references volume %s", deliverycore.ErrVolumeInUse, serviceID, volumeID)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}

		result, err := tx.ExecContext(ctx, `DELETE FROM volumes WHERE id = $1`, volumeID)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}
