package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
)

func (s *catalogPersistence) createScheduledVolume(ctx context.Context, user authz.User, environmentID, name string, sizeBytes int64) (deliverycore.VolumeRecord, error) {
	scope, err := s.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Write)
	if err != nil {
		return deliverycore.VolumeRecord{}, err
	}
	var rec deliverycore.VolumeRecord
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		rec, err = s.createVolumeTx(ctx, tx, scope.Project(), scope.ID(), name, sizeBytes)
		return err
	})
	if err != nil {
		return deliverycore.VolumeRecord{}, err
	}
	return rec, nil
}

func (s *catalogPersistence) listVolumes(ctx context.Context, user authz.User, environmentID string) ([]deliverycore.VolumeRecord, error) {
	scope, err := s.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Read)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, environment_id, name, size_bytes, created_at
		   FROM volumes
		  WHERE environment_id = $1
		  ORDER BY created_at ASC`,
		scope.ID(),
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

func (s *catalogPersistence) deleteVolume(ctx context.Context, user authz.User, volumeID string) error {
	scope, err := s.authz.AuthorizeVolume(ctx, user, volumeID, authz.Write)
	if err != nil {
		return err
	}
	return s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var (
			volumeName    string
			environmentID string
		)
		err := tx.QueryRowContext(ctx, `SELECT name, environment_id FROM volumes WHERE id = $1 AND environment_id = $2`, scope.ID(), scope.EnvironmentID()).Scan(&volumeName, &environmentID)
		if err != nil {
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
				return fmt.Errorf("%w: service %s references volume %s", deliverycore.ErrVolumeInUse, serviceID, scope.ID())
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}

		result, err := tx.ExecContext(ctx, `DELETE FROM volumes WHERE id = $1`, scope.ID())
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
		journal.RecordVolume(ctx, scope.ID())
		return nil
	})
}

func (s *catalogPersistence) createVolumeTx(ctx context.Context, tx *sql.Tx, project authz.Project, environmentID, name string, sizeBytes int64) (deliverycore.VolumeRecord, error) {
	name = strings.TrimSpace(name)
	if name == "" || sizeBytes <= 0 {
		return deliverycore.VolumeRecord{}, deliverycore.ErrInvalidVolume
	}
	var resolvedID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM environments WHERE id = $1 AND project_id = $2`, environmentID, project.ID()).Scan(&resolvedID); err != nil {
		return deliverycore.VolumeRecord{}, err
	}
	rec := deliverycore.VolumeRecord{
		ID:            uuid.NewString(),
		EnvironmentID: resolvedID,
		Name:          name,
		SizeBytes:     sizeBytes,
		CreatedAt:     time.Now().UTC(),
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO volumes(id, environment_id, name, size_bytes, created_at) VALUES ($1, $2, $3, $4, $5)`,
		rec.ID, rec.EnvironmentID, rec.Name, rec.SizeBytes, rec.CreatedAt,
	); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "volumes_environment_id_name_key" {
			return deliverycore.VolumeRecord{}, deliverycore.ErrVolumeAlreadyExists
		}
		return deliverycore.VolumeRecord{}, err
	}
	journal.RecordVolume(ctx, rec.ID)
	return rec, nil
}
