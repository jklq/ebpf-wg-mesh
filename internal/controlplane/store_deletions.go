package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
)

// lockProjectTx locks a user project row and loads it with its deletion
// state. Managed projects do not resolve here: the kind filter keeps them
// out, and authorization already denied them above.
func (s *catalogPersistence) lockProjectTx(ctx context.Context, tx *sql.Tx, scope authz.Project) (deliverycore.ProjectRecord, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT p.id, p.name, p.kind, COALESCE(p.system_key, ''), p.created_at,
		        p.deleted_at, p.deleted_by_user_id, p.delete_expires_at, p.log_retention_days
		   FROM projects p
		  WHERE p.id = $1 AND p.kind = $2 FOR UPDATE OF p`,
		scope.ID(),
		string(deliverycore.ProjectKindUser),
	)
	return deliverycore.ScanProjectRow(row)
}

func (s *catalogPersistence) tombstoneProjectTx(ctx context.Context, tx *sql.Tx, projectID, userID string, now time.Time) (bool, error) {
	return s.tombstoneRowTx(ctx, tx, "projects", projectID, userID, now)
}

func (s *catalogPersistence) clearProjectTombstoneTx(ctx context.Context, tx *sql.Tx, projectID string) (bool, error) {
	return clearTombstoneRowTx(ctx, tx, "projects", projectID)
}

// lockEnvironmentTx locks an environment row and loads it with its effective
// deletion state.
func (s *catalogPersistence) lockEnvironmentTx(ctx context.Context, tx *sql.Tx, scope authz.Environment) (deliverycore.EnvironmentRecord, error) {
	row := tx.QueryRowContext(ctx, environmentSelect+`
		 WHERE e.id = $1 AND e.project_id = $2 FOR UPDATE OF e`,
		scope.ID(), scope.ProjectID())
	return deliverycore.ScanEnvironmentRow(row)
}

func (s *catalogPersistence) tombstoneEnvironmentTx(ctx context.Context, tx *sql.Tx, environmentID, userID string, now time.Time) (bool, error) {
	return s.tombstoneRowTx(ctx, tx, "environments", environmentID, userID, now)
}

func (s *catalogPersistence) clearEnvironmentTombstoneTx(ctx context.Context, tx *sql.Tx, environmentID string) (bool, error) {
	return clearTombstoneRowTx(ctx, tx, "environments", environmentID)
}

// lockVolumeTx locks a volume row and loads it with its effective deletion
// state plus the environment's production flag.
func (s *catalogPersistence) lockVolumeTx(ctx context.Context, tx *sql.Tx, scope authz.Volume) (deliverycore.VolumeRecord, bool, error) {
	var rec deliverycore.VolumeRecord
	var self, environment, project deliverycore.Tombstone
	var production bool
	targets := []any{&rec.ID, &rec.EnvironmentID, &rec.Name, &rec.SizeBytes, &rec.CreatedAt, &production}
	targets = deliverycore.ScanTombstone(targets, &self)
	targets = deliverycore.ScanTombstone(targets, &environment)
	err := tx.QueryRowContext(ctx,
		`SELECT v.id, v.environment_id, v.name, v.size_bytes, v.created_at, e.is_production,
		        v.deleted_at, v.deleted_by_user_id, v.delete_expires_at,
		        e.deleted_at, e.deleted_by_user_id, e.delete_expires_at,
		        p.deleted_at, p.deleted_by_user_id, p.delete_expires_at
		   FROM volumes v
		   JOIN environments e ON e.id = v.environment_id
		   JOIN projects p ON p.id = e.project_id
		  WHERE v.id = $1 AND v.environment_id = $2 FOR UPDATE OF v`,
		scope.ID(), scope.EnvironmentID(),
	).Scan(deliverycore.ScanTombstone(targets, &project)...)
	if err != nil {
		return deliverycore.VolumeRecord{}, false, err
	}
	rec.Deletion = deliverycore.EffectiveDeletion(self, environment, project)
	return rec, production, nil
}

func (s *catalogPersistence) tombstoneVolumeTx(ctx context.Context, tx *sql.Tx, volumeID, userID string, now time.Time) (bool, error) {
	return s.tombstoneRowTx(ctx, tx, "volumes", volumeID, userID, now)
}

func (s *catalogPersistence) tombstoneRowTx(ctx context.Context, tx *sql.Tx, table, id, userID string, now time.Time) (bool, error) {
	result, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s
		    SET deleted_at = $1,
		        deleted_by_user_id = $2,
		        delete_expires_at = $3
		  WHERE id = $4 AND deleted_at IS NULL`, table),
		now, userID, now.Add(s.deletionGracePeriod()), id,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func clearTombstoneRowTx(ctx context.Context, tx *sql.Tx, table, id string) (bool, error) {
	result, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s
		    SET deleted_at = NULL,
		        deleted_by_user_id = '',
		        delete_expires_at = NULL
		  WHERE id = $1 AND deleted_at IS NOT NULL AND delete_expires_at > statement_timestamp()`, table), id)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// quiesceEnvironmentServicesTx stops new work for every service in an
// environment being deleted.
func (s *catalogPersistence) quiesceEnvironmentServicesTx(ctx context.Context, tx *sql.Tx, environmentID, userID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM services WHERE environment_id = $1 ORDER BY id`, environmentID)
	if err != nil {
		return err
	}
	var serviceIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		serviceIDs = append(serviceIDs, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	quiescer := deliverycore.NewServiceQuiescer(s.source)
	for _, id := range serviceIDs {
		if err := quiescer.QuiesceTx(ctx, tx, id, userID); err != nil {
			return err
		}
	}
	return nil
}

// quiesceProjectServicesTx stops new work for every service in a project
// being deleted.
func (s *catalogPersistence) quiesceProjectServicesTx(ctx context.Context, tx *sql.Tx, projectID, userID string) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT s.id FROM services s
		  JOIN environments e ON e.id = s.environment_id
		 WHERE e.project_id = $1 ORDER BY s.id`,
		projectID,
	)
	if err != nil {
		return err
	}
	var serviceIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		serviceIDs = append(serviceIDs, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	quiescer := deliverycore.NewServiceQuiescer(s.source)
	for _, id := range serviceIDs {
		if err := quiescer.QuiesceTx(ctx, tx, id, userID); err != nil {
			return err
		}
	}
	return nil
}

// dropEnvironmentAssignmentsTx deletes the placement records of every service
// in an environment. Assignments reference the Removed deployment a delete
// leaves behind, so they are dropped at delete time and the restore-time call
// re-asserts the empty set; a release recreates them.
func dropEnvironmentAssignmentsTx(ctx context.Context, tx *sql.Tx, environmentID string) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM allocation_assignments a USING services s
		  WHERE a.service_id = s.id AND s.environment_id = $1`,
		environmentID,
	)
	return err
}

// dropProjectAssignmentsTx deletes the placement records of every service in
// a project. Assignments are dropped at delete time and the restore-time call
// re-asserts the empty set; a release recreates them.
func dropProjectAssignmentsTx(ctx context.Context, tx *sql.Tx, projectID string) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM allocation_assignments a USING services s, environments e
		  WHERE a.service_id = s.id AND s.environment_id = e.id AND e.project_id = $1`,
		projectID,
	)
	return err
}

// deleteProject tombstones a user project and quiesces its services.
// Managed projects are refused: platform services cannot be deleted.
// Repeats are idempotent; confirmation is only checked on the first delete.
func (s *catalogPersistence) deleteProject(ctx context.Context, user authz.User, projectID, confirmation string) ([]string, error) {
	scope, err := s.authz.AuthorizeProject(ctx, user, projectID, authz.Write)
	if err != nil {
		return nil, err
	}
	var agentIDs []string
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		agentIDs = nil
		rec, err := s.lockProjectTx(ctx, tx, scope)
		if err != nil {
			return err
		}
		if rec.Deletion != nil {
			return nil
		}
		// Managed projects never reach this point: authorization admits
		// user projects only, and the row lock below filters by kind.
		if err := deliverycore.CheckDeletionConfirmation(rec.Name, confirmation); err != nil {
			return err
		}
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		tombstoned, err := s.tombstoneProjectTx(ctx, tx, rec.ID, user.ID(), now)
		if err != nil {
			return err
		}
		if !tombstoned {
			return nil
		}
		if err := s.quiesceProjectServicesTx(ctx, tx, rec.ID, user.ID()); err != nil {
			return err
		}
		if err := journal.RecordProjectRemoval(ctx, tx, rec.ID); err != nil {
			return err
		}
		agentIDs, err = s.projectAgentIDsQuerier(ctx, tx, rec.ID)
		if err != nil {
			return err
		}
		// Drop after the agent query: the notifier set is derived from the
		// assignments being removed.
		return dropProjectAssignmentsTx(ctx, tx, rec.ID)
	})
	return agentIDs, err
}

// restoreProject clears a project's tombstone within the grace period.
// Independently tombstoned children keep their tombstones.
func (s *catalogPersistence) restoreProject(ctx context.Context, user authz.User, projectID string) (deliverycore.ProjectRecord, error) {
	scope, err := s.authz.AuthorizeProject(ctx, user, projectID, authz.Write)
	if err != nil {
		return deliverycore.ProjectRecord{}, err
	}
	var rec deliverycore.ProjectRecord
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := s.lockProjectTx(ctx, tx, scope)
		if err != nil {
			return err
		}
		if current.Deletion == nil {
			rec = current
			return nil
		}
		restored, err := s.clearProjectTombstoneTx(ctx, tx, current.ID)
		if err != nil {
			return err
		}
		if !restored {
			return deliverycore.ErrDeletionExpired
		}
		if err := dropProjectAssignmentsTx(ctx, tx, current.ID); err != nil {
			return err
		}
		if err := journal.RecordProjectRemoval(ctx, tx, current.ID); err != nil {
			return err
		}
		rec, err = s.projectByScopeQuerier(ctx, tx, scope)
		return err
	})
	return rec, err
}

func (s *catalogPersistence) projectAgentIDsQuerier(ctx context.Context, q deliverycore.ServiceQueryer, projectID string) ([]string, error) {
	var any bool
	if err := q.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM allocation_assignments a
		  JOIN services s ON s.id = a.service_id
		  JOIN environments e ON e.id = s.environment_id
		 WHERE e.project_id = $1)`,
		projectID,
	).Scan(&any); err != nil {
		return nil, err
	}
	if !any {
		return nil, nil
	}
	return s.agentIDsQuerier(ctx, q)
}

func (s *catalogPersistence) environmentAgentIDsQuerier(ctx context.Context, q deliverycore.ServiceQueryer, environmentID string) ([]string, error) {
	var any bool
	if err := q.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM allocation_assignments a
		  JOIN services s ON s.id = a.service_id WHERE s.environment_id = $1)`,
		environmentID,
	).Scan(&any); err != nil {
		return nil, err
	}
	if !any {
		return nil, nil
	}
	return s.agentIDsQuerier(ctx, q)
}
