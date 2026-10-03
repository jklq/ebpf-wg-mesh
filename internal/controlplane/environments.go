package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func (s *catalogPersistence) ensureProductionEnvironmentQuerier(ctx context.Context, q deliverycore.ServiceQueryer, projectID string) (deliverycore.EnvironmentRecord, error) {
	rec, err := deliverycore.ScanEnvironmentRow(q.QueryRowContext(ctx, environmentSelect+`
		WHERE e.project_id = $1 AND e.is_production = TRUE`, projectID))
	if err == nil {
		if rec.Deletion != nil {
			return deliverycore.EnvironmentRecord{}, deliverycore.ErrEnvironmentDeleted
		}
		return rec, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return deliverycore.EnvironmentRecord{}, err
	}
	return s.createEnvironmentQuerier(ctx, q, projectID, "Production", true, "")
}

func (s *catalogPersistence) createEnvironment(ctx context.Context, user authz.User, projectID, name string) (deliverycore.EnvironmentRecord, error) {
	scope, err := s.authz.AuthorizeProject(ctx, user, projectID, authz.Write)
	if err != nil {
		return deliverycore.EnvironmentRecord{}, err
	}
	var rec deliverycore.EnvironmentRecord
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		project, err := s.projectByIDInternalQuerier(ctx, tx, scope.ID())
		if err != nil {
			return err
		}
		if project.Deletion != nil {
			return deliverycore.ErrProjectDeleted
		}
		rec, err = s.createEnvironmentQuerier(ctx, tx, scope.ID(), name, false, "")
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return s.environmentNameConflictErr(ctx, tx, scope.ID(), name)
			}
			return err
		}
		return nil
	})
	return rec, err
}

// environmentNameConflictErr maps an environment-name collision to a typed
// error. Tombstoned rows reserve their names for the grace period.
func (s *catalogPersistence) environmentNameConflictErr(ctx context.Context, q deliverycore.ServiceQueryer, projectID, name string) error {
	row := q.QueryRowContext(ctx, environmentSelect+`
		 WHERE e.project_id = $1 AND e.name = $2`,
		projectID, strings.TrimSpace(name))
	existing, err := deliverycore.ScanEnvironmentRow(row)
	if err != nil {
		return fmt.Errorf("%w: %q", deliverycore.ErrEnvironmentAlreadyExists, strings.TrimSpace(name))
	}
	if existing.Deletion != nil && !existing.Deletion.Inherited {
		return fmt.Errorf("%w: %q was deleted; restore it or wait until %s", deliverycore.ErrEnvironmentAlreadyExists, existing.Name, existing.Deletion.ExpiresAt.Format(time.RFC3339))
	}
	return fmt.Errorf("%w: %q", deliverycore.ErrEnvironmentAlreadyExists, existing.Name)
}

func (s *catalogPersistence) listEnvironments(ctx context.Context, user authz.User, projectID string, includeDeleted bool) ([]deliverycore.EnvironmentRecord, error) {
	scope, err := s.authz.AuthorizeProject(ctx, user, projectID, authz.Read)
	if err != nil {
		return nil, err
	}
	filter := `
		 AND e.deleted_at IS NULL AND p.deleted_at IS NULL`
	if includeDeleted {
		filter = ``
	}
	rows, err := s.db.QueryContext(ctx, environmentSelect+`
		 WHERE e.project_id = $1`+filter+`
		 ORDER BY e.is_production DESC, e.created_at ASC, e.id ASC`, scope.ID())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []deliverycore.EnvironmentRecord
	for rows.Next() {
		rec, err := deliverycore.ScanEnvironmentRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *catalogPersistence) productionEnvironmentByProjectInternal(ctx context.Context, projectID string) (deliverycore.EnvironmentRecord, error) {
	return deliverycore.ScanEnvironmentRow(s.db.QueryRowContext(ctx, environmentSelect+`
		 WHERE e.project_id = $1 AND e.is_production = TRUE
		   AND e.deleted_at IS NULL AND p.deleted_at IS NULL`, projectID))
}

func (s *catalogPersistence) renameEnvironment(ctx context.Context, user authz.User, environmentID, name string) (deliverycore.EnvironmentRecord, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return deliverycore.EnvironmentRecord{}, fmt.Errorf("environment name is required")
	}
	scope, err := s.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Write)
	if err != nil {
		return deliverycore.EnvironmentRecord{}, err
	}
	var rec deliverycore.EnvironmentRecord
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := s.environmentByScopeQuerier(ctx, tx, scope)
		if err != nil {
			return err
		}
		if current.Deletion != nil {
			return deliverycore.ErrEnvironmentDeleted
		}
		now := time.Now().UTC()
		if _, err := journal.EnvironmentRow(current.ID).Exec(ctx, tx, `UPDATE environments SET name = $1, updated_at = $2 WHERE id = $3`, name, now, current.ID); err != nil {
			return err
		}
		current.Name = name
		current.UpdatedAt = now
		rec = current
		return nil
	})
	return rec, err
}

func (s *catalogPersistence) updateEnvironmentAutoDeploy(ctx context.Context, user authz.User, environmentID string, autoDeploy bool) (deliverycore.EnvironmentRecord, error) {
	scope, err := s.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Write)
	if err != nil {
		return deliverycore.EnvironmentRecord{}, err
	}
	var rec deliverycore.EnvironmentRecord
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := s.environmentByScopeQuerier(ctx, tx, scope)
		if err != nil {
			return err
		}
		if current.Deletion != nil {
			return deliverycore.ErrEnvironmentDeleted
		}
		now := time.Now().UTC()
		if _, err := journal.EnvironmentRow(current.ID).Exec(ctx, tx, `UPDATE environments SET auto_deploy = $1, updated_at = $2 WHERE id = $3`, autoDeploy, now, current.ID); err != nil {
			return err
		}
		current.AutoDeploy = autoDeploy
		current.UpdatedAt = now
		rec = current
		return nil
	})
	return rec, err
}

// deleteEnvironment tombstones an environment and quiesces its services.
// Production environments require a typed confirmation matching the current
// name; the confirmation is only checked on the first delete, so repeats
// stay idempotent.
func (s *catalogPersistence) deleteEnvironment(ctx context.Context, user authz.User, environmentID, confirmation string) ([]string, error) {
	scope, err := s.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Write)
	if err != nil {
		return nil, err
	}
	var agentIDs []string
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		agentIDs = nil
		rec, err := s.lockEnvironmentTx(ctx, tx, scope)
		if err != nil {
			return err
		}
		if rec.Deletion != nil && !rec.Deletion.Inherited {
			return nil
		}
		if rec.IsProduction {
			if err := deliverycore.CheckDeletionConfirmation(rec.Name, confirmation); err != nil {
				return err
			}
		}
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		tombstoned, err := s.tombstoneEnvironmentTx(ctx, tx, rec.ID, user.ID(), now)
		if err != nil {
			return err
		}
		if !tombstoned {
			return nil
		}
		agentIDs, err = s.environmentAgentIDsQuerier(ctx, tx, rec.ID)
		if err != nil {
			return err
		}
		return deliverycore.QuiesceDeletionTx(ctx, tx, s.source, deliverycore.DeletionTarget{Kind: deliverycore.DeleteEnvironment, ID: rec.ID}, user.ID())
	})
	return agentIDs, err
}

// restoreEnvironment clears an environment's tombstone within the grace
// period. Restoring under a tombstoned project is refused: restore top-down.
// Independently tombstoned services keep their tombstones.
func (s *catalogPersistence) restoreEnvironment(ctx context.Context, user authz.User, environmentID string) (deliverycore.EnvironmentRecord, error) {
	scope, err := s.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Write)
	if err != nil {
		return deliverycore.EnvironmentRecord{}, err
	}
	var rec deliverycore.EnvironmentRecord
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := s.lockEnvironmentTx(ctx, tx, scope)
		if err != nil {
			return err
		}
		if current.Deletion == nil {
			rec = current
			return nil
		}
		if current.Deletion.Inherited {
			return deliverycore.ErrAncestorDeleted
		}
		restored, err := s.clearEnvironmentTombstoneTx(ctx, tx, current.ID)
		if err != nil {
			return err
		}
		if !restored {
			return deliverycore.ErrDeletionExpired
		}
		if err := deliverycore.DropDeletionAssignmentsTx(ctx, tx, deliverycore.DeletionTarget{Kind: deliverycore.DeleteEnvironment, ID: current.ID}); err != nil {
			return err
		}
		rec, err = s.environmentByScopeQuerier(ctx, tx, scope)
		return err
	})
	return rec, err
}

func (s *catalogPersistence) createEnvironmentQuerier(ctx context.Context, q deliverycore.ServiceQueryer, projectID, name string, production bool, copiedFrom string) (deliverycore.EnvironmentRecord, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return deliverycore.EnvironmentRecord{}, fmt.Errorf("environment name is required")
	}
	networkIdentity, err := allocateEnvironmentNetworkIdentity(ctx, q)
	if err != nil {
		return deliverycore.EnvironmentRecord{}, err
	}
	now := time.Now().UTC()
	rec := deliverycore.EnvironmentRecord{
		ID:                      uuid.NewString(),
		ProjectID:               projectID,
		Name:                    name,
		Kind:                    deliverycore.EnvironmentKindPersistent,
		IsProduction:            production,
		AutoDeploy:              true,
		NetworkIdentity:         networkIdentity,
		CopiedFromEnvironmentID: copiedFrom,
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	_, err = journal.EnvironmentRow(rec.ID).Exec(ctx, q, `
		INSERT INTO environments(
			id, project_id, name, kind, is_production, auto_deploy, network_identity,
			copied_from_environment_id, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		rec.ID, rec.ProjectID, rec.Name, string(rec.Kind), rec.IsProduction, rec.AutoDeploy,
		rec.NetworkIdentity, nullIfEmpty(rec.CopiedFromEnvironmentID), rec.CreatedAt, rec.UpdatedAt,
	)
	if err != nil {
		return deliverycore.EnvironmentRecord{}, err
	}

	return rec, nil
}

func allocateEnvironmentNetworkIdentity(ctx context.Context, q deliverycore.ServiceQueryer) (uint32, error) {
	var identity int64
	if err := q.QueryRowContext(ctx,
		`UPDATE environment_network_identity_counter
		    SET next_identity = next_identity + 1
		  WHERE id = TRUE AND next_identity <= 4294967295
		  RETURNING next_identity - 1`,
	).Scan(&identity); err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("environment network identity space exhausted")
		}
		return 0, fmt.Errorf("allocate environment network identity: %w", err)
	}
	return uint32(identity), nil
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *catalogPersistence) environmentByScopeQuerier(ctx context.Context, q deliverycore.ServiceQueryer, scope authz.Environment) (deliverycore.EnvironmentRecord, error) {
	row := q.QueryRowContext(ctx, environmentSelect+`
		 WHERE e.id = $1 AND e.project_id = $2`,
		scope.ID(), scope.ProjectID())
	return deliverycore.ScanEnvironmentRow(row)
}

const environmentSelect = `SELECT e.id, e.project_id, e.name, e.kind, e.is_production, e.auto_deploy,
	e.network_identity, COALESCE(e.copied_from_environment_id, ''), e.created_at, e.updated_at,
	e.deleted_at, e.deleted_by_user_id, e.delete_expires_at,
	p.deleted_at, p.deleted_by_user_id, p.delete_expires_at
	FROM environments e JOIN projects p ON p.id = e.project_id`

func (s *catalogPersistence) agentIDsQuerier(ctx context.Context, q deliverycore.ServiceQueryer) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM agents ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

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

// deleteVolume tombstones a volume. Deletion needs typed confirmation, is never
// restored, and is delayed by the grace period; only then does its node get an
// explicit destroy. A volume referenced by a live service is refused.
func (s *catalogPersistence) deleteVolume(ctx context.Context, user authz.User, volumeID, confirmation string) error {
	scope, err := s.authz.AuthorizeVolume(ctx, user, volumeID, authz.Write)
	if err != nil {
		return err
	}
	return s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rec, err := s.lockVolumeTx(ctx, tx, scope)
		if err != nil {
			return err
		}
		if rec.Deletion != nil && !rec.Deletion.Inherited {
			return nil
		}
		if rec.Staged {
			// Never released, so nothing exists on any node: discarding the
			// staged change removes it outright and needs no typed name.
			if confirmation != "" {
				if err := deliverycore.CheckDeletionConfirmation(rec.Name, confirmation); err != nil {
					return err
				}
			}
			if err := s.requireVolumeDetachedTx(ctx, tx, rec); err != nil {
				return err
			}
			_, err := deliverycore.DiscardStagedVolumeTx(ctx, tx, rec.ID)
			return err
		}
		if err := deliverycore.CheckDeletionConfirmation(rec.Name, confirmation); err != nil {
			return err
		}
		if err := s.requireVolumeDetachedTx(ctx, tx, rec); err != nil {
			return err
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
		return nil
	})
}

// requireVolumeDetachedTx refuses deletion while a live service mounts the
// volume in its draft or its deployed revision. Tombstoned services do not
// block: their mounts are withdrawn.
func (s *catalogPersistence) requireVolumeDetachedTx(ctx context.Context, tx *sql.Tx, rec deliverycore.VolumeRecord) error {
	owner, err := deliverycore.VolumeAttachmentOwner(ctx, tx, rec.EnvironmentID, rec.Name, "")
	if err != nil {
		return err
	}
	if owner != "" {
		return fmt.Errorf("%w: service %s mounts volume %s; detach and deploy it first", deliverycore.ErrVolumeInUse, owner, rec.Name)
	}
	return nil
}

// volumeNamePattern keeps volume names usable as stable references in service
// specs and on the canvas: lowercase words joined by hyphens.
var volumeNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func (s *catalogPersistence) createVolumeTx(ctx context.Context, tx *sql.Tx, project authz.Project, environmentID, name string, sizeBytes int64) (deliverycore.VolumeRecord, error) {
	name = strings.TrimSpace(name)
	if !volumeNamePattern.MatchString(name) {
		return deliverycore.VolumeRecord{}, deliverycore.ErrInvalidVolume
	}
	if err := deliverycore.ValidateVolumeSize(sizeBytes); err != nil {
		return deliverycore.VolumeRecord{}, err
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
		Staged:        true,
		CreatedAt:     time.Now().UTC(),
	}
	if _, err := journal.VolumeRow(rec.ID).Exec(ctx, tx,
		`INSERT INTO volumes(id, environment_id, name, size_bytes, staged, created_at) VALUES ($1, $2, $3, $4, true, $5)`,
		rec.ID, rec.EnvironmentID, rec.Name, rec.SizeBytes, rec.CreatedAt,
	); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "volumes_environment_id_name_key" {
			return deliverycore.VolumeRecord{}, deliverycore.ErrVolumeAlreadyExists
		}
		return deliverycore.VolumeRecord{}, err
	}
	return rec, nil
}
