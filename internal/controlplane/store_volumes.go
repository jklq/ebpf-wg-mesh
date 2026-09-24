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
	"ebof-wg-mesh/internal/controlplane/dbtx"
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

func (s *catalogPersistence) listVolumes(ctx context.Context, user authz.User, environmentID string, includeDeleted bool) ([]deliverycore.VolumeRecord, error) {
	scope, err := s.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Read)
	if err != nil {
		return nil, err
	}
	filter := `
		    AND v.deleted_at IS NULL AND e.deleted_at IS NULL AND p.deleted_at IS NULL`
	if includeDeleted {
		filter = ``
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT v.id, v.environment_id, v.name, v.size_bytes, v.created_at,
		        v.deleted_at, v.deleted_by_user_id, v.delete_expires_at,
		        e.deleted_at, e.deleted_by_user_id, e.delete_expires_at,
		        p.deleted_at, p.deleted_by_user_id, p.delete_expires_at
		   FROM volumes v
		   JOIN environments e ON e.id = v.environment_id
		   JOIN projects p ON p.id = e.project_id
		  WHERE v.environment_id = $1`+filter+`
		  ORDER BY v.created_at ASC`,
		scope.ID(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []deliverycore.VolumeRecord
	for rows.Next() {
		var rec deliverycore.VolumeRecord
		var self, environment, project deliverycore.Tombstone
		targets := []any{&rec.ID, &rec.EnvironmentID, &rec.Name, &rec.SizeBytes, &rec.CreatedAt}
		targets = deliverycore.ScanTombstone(targets, &self)
		targets = deliverycore.ScanTombstone(targets, &environment)
		if err := rows.Scan(deliverycore.ScanTombstone(targets, &project)...); err != nil {
			return nil, err
		}
		rec.Deletion = deliverycore.EffectiveDeletion(self, environment, project)
		out = append(out, rec)
	}
	return out, rows.Err()
}

// deleteVolume tombstones a volume. Deletion always requires a typed
// confirmation matching the current name, and volumes are never restored:
// until the stateful volume provider (06-stateful) owns the data lifecycle,
// deletion is one-way but delayed by the grace period instead of instant.
// A volume referenced by a live service is refused everywhere; a production
// volume that was ever attached is refused as possibly non-empty, because
// nothing can prove it empty yet.
func (s *catalogPersistence) deleteVolume(ctx context.Context, user authz.User, volumeID, confirmation string) error {
	scope, err := s.authz.AuthorizeVolume(ctx, user, volumeID, authz.Write)
	if err != nil {
		return err
	}
	return s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rec, production, err := s.lockVolumeTx(ctx, tx, scope)
		if err != nil {
			return err
		}
		if rec.Deletion != nil && !rec.Deletion.Inherited {
			return nil
		}
		if err := deliverycore.CheckDeletionConfirmation(rec.Name, confirmation); err != nil {
			return err
		}
		if err := s.requireVolumeDetachedTx(ctx, tx, rec); err != nil {
			return err
		}
		if production {
			if err := s.requireVolumeNeverAttachedTx(ctx, tx, rec); err != nil {
				return err
			}
		}
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		tombstoned, err := s.tombstoneVolumeTx(ctx, tx, rec.ID, user.ID(), now)
		if err != nil {
			return err
		}
		if !tombstoned {
			return nil
		}
		journal.RecordVolume(ctx, rec.ID)
		return nil
	})
}

// requireVolumeDetachedTx refuses deletion while a live service references
// the volume. References from tombstoned services do not block: their mounts
// are already withdrawn.
func (s *catalogPersistence) requireVolumeDetachedTx(ctx context.Context, tx *sql.Tx, rec deliverycore.VolumeRecord) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT s.id, r.spec_json
		   FROM live_services s
		   JOIN service_revisions r ON r.service_id = s.id AND r.spec_revision = s.current_spec_revision
		  WHERE s.environment_id = $1`,
		rec.EnvironmentID,
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
		if deliverycore.ServiceVolumeName(spec) == rec.Name {
			return fmt.Errorf("%w: service %s references volume %s", deliverycore.ErrVolumeInUse, serviceID, rec.ID)
		}
	}
	return rows.Err()
}

// requireVolumeNeverAttachedTx fails closed on production volumes: without a
// volume provider there is no emptiness signal, so any service revision that
// ever referenced the volume — current or historical — blocks deletion.
func (s *catalogPersistence) requireVolumeNeverAttachedTx(ctx context.Context, tx *sql.Tx, rec deliverycore.VolumeRecord) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT r.spec_json
		   FROM service_revisions r
		   JOIN services s ON s.id = r.service_id
		  WHERE s.environment_id = $1`,
		rec.EnvironmentID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var rawSpec []byte
		if err := rows.Scan(&rawSpec); err != nil {
			return err
		}
		spec, err := deliverycore.LoadServiceSpec(rawSpec)
		if err != nil {
			return err
		}
		if deliverycore.ServiceVolumeName(spec) == rec.Name {
			return fmt.Errorf("%w: production volume %q was attached and cannot be proven empty", deliverycore.ErrVolumeNotEmpty, rec.Name)
		}
	}
	return rows.Err()
}

func (s *catalogPersistence) createVolumeTx(ctx context.Context, tx *sql.Tx, project authz.Project, environmentID, name string, sizeBytes int64) (deliverycore.VolumeRecord, error) {
	name = strings.TrimSpace(name)
	if name == "" || sizeBytes <= 0 {
		return deliverycore.VolumeRecord{}, deliverycore.ErrInvalidVolume
	}
	var resolvedID string
	var environmentTombstone, projectTombstone deliverycore.Tombstone
	err := tx.QueryRowContext(ctx,
		`SELECT e.id, e.deleted_at, e.deleted_by_user_id, e.delete_expires_at,
		        p.deleted_at, p.deleted_by_user_id, p.delete_expires_at
		   FROM environments e JOIN projects p ON p.id = e.project_id
		  WHERE e.id = $1 AND e.project_id = $2`,
		environmentID, project.ID(),
	).Scan(&resolvedID, &environmentTombstone.DeletedAt, &environmentTombstone.DeletedByUserID, &environmentTombstone.ExpiresAt,
		&projectTombstone.DeletedAt, &projectTombstone.DeletedByUserID, &projectTombstone.ExpiresAt)
	if err != nil {
		return deliverycore.VolumeRecord{}, err
	}
	if environmentTombstone.Active() || projectTombstone.Active() {
		return deliverycore.VolumeRecord{}, deliverycore.ErrEnvironmentDeleted
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
