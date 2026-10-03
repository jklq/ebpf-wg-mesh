package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
)

// lockProjectTx locks a user project row and loads it with its deletion state. Managed
// projects never resolve here: the kind filter keeps them out.
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
	return s.tombstoneRowTx(ctx, tx, "projects", journal.ProjectTree(projectID), projectID, userID, now)
}

func (s *catalogPersistence) clearProjectTombstoneTx(ctx context.Context, tx *sql.Tx, projectID string) (bool, error) {
	return clearTombstoneRowTx(ctx, tx, "projects", journal.ProjectTree(projectID), projectID)
}

// lockEnvironmentTx locks an environment row and loads it with its effective deletion state.
func (s *catalogPersistence) lockEnvironmentTx(ctx context.Context, tx *sql.Tx, scope authz.Environment) (deliverycore.EnvironmentRecord, error) {
	row := tx.QueryRowContext(ctx, environmentSelect+`
		 WHERE e.id = $1 AND e.project_id = $2 FOR UPDATE OF e`,
		scope.ID(), scope.ProjectID())
	return deliverycore.ScanEnvironmentRow(row)
}

func (s *catalogPersistence) tombstoneEnvironmentTx(ctx context.Context, tx *sql.Tx, environmentID, userID string, now time.Time) (bool, error) {
	return s.tombstoneRowTx(ctx, tx, "environments", journal.EnvironmentTree(environmentID), environmentID, userID, now)
}

func (s *catalogPersistence) clearEnvironmentTombstoneTx(ctx context.Context, tx *sql.Tx, environmentID string) (bool, error) {
	return clearTombstoneRowTx(ctx, tx, "environments", journal.EnvironmentTree(environmentID), environmentID)
}

// lockVolumeTx locks a volume row, its deletion state, and the environment's production flag.
func (s *catalogPersistence) lockVolumeTx(ctx context.Context, tx *sql.Tx, scope authz.Volume) (deliverycore.VolumeRecord, error) {
	var rec deliverycore.VolumeRecord
	var self, environment, project deliverycore.Tombstone
	targets := []any{&rec.ID, &rec.EnvironmentID, &rec.Name, &rec.SizeBytes, &rec.Staged, &rec.CreatedAt}
	targets = deliverycore.ScanTombstone(targets, &self)
	targets = deliverycore.ScanTombstone(targets, &environment)
	err := tx.QueryRowContext(ctx,
		`SELECT v.id, v.environment_id, v.name, v.size_bytes, v.staged, v.created_at,
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
		return deliverycore.VolumeRecord{}, err
	}
	rec.Deletion = deliverycore.EffectiveDeletion(self, environment, project)
	return rec, nil
}

func (s *catalogPersistence) tombstoneVolumeTx(ctx context.Context, tx *sql.Tx, volumeID, userID string, now time.Time) (bool, error) {
	return s.tombstoneRowTx(ctx, tx, "volumes", journal.VolumeRow(volumeID), volumeID, userID, now)
}

func (s *catalogPersistence) tombstoneRowTx(ctx context.Context, tx *sql.Tx, table string, effect journal.Mutation, id, userID string, now time.Time) (bool, error) {
	result, err := effect.Exec(ctx, tx,
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

func clearTombstoneRowTx(ctx context.Context, tx *sql.Tx, table string, effect journal.Mutation, id string) (bool, error) {
	result, err := effect.Exec(ctx, tx,
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

// deleteProject tombstones a user project and quiesces its services. Managed projects are
// refused; repeats are idempotent and confirmation is checked only on the first delete.
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
		agentIDs, err = s.projectAgentIDsQuerier(ctx, tx, rec.ID)
		if err != nil {
			return err
		}
		return deliverycore.QuiesceDeletionTx(ctx, tx, s.source, deliverycore.DeletionTarget{Kind: deliverycore.DeleteProject, ID: rec.ID}, user.ID())
	})
	return agentIDs, err
}

// restoreProject clears a project's own tombstone within the grace period.
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
		if err := deliverycore.DropDeletionAssignmentsTx(ctx, tx, deliverycore.DeletionTarget{Kind: deliverycore.DeleteProject, ID: current.ID}); err != nil {
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

// deletionPreview reports the live dependents a delete would tombstone.
type deletionPreview struct {
	Environments []previewEnvironment
	Services     []previewService
	Domains      []previewDomain
	Volumes      []previewVolume
}

// PreviewEnvironment is one environment in a deletion preview.
type previewEnvironment struct {
	ID           string
	Name         string
	IsProduction bool
}

// PreviewService is one service in a deletion preview.
type previewService struct {
	ID              string
	Name            string
	EnvironmentID   string
	EnvironmentName string
}

// PreviewDomain is one domain binding in a deletion preview.
type previewDomain struct {
	Hostname          string
	ServiceID         string
	ServiceName       string
	PlatformGenerated bool
}

// PreviewVolume is one volume in a deletion preview.
type previewVolume struct {
	ID            string
	Name          string
	EnvironmentID string
}

// previewProjectDeletion lists the live environments, services, domains, and
// volumes a project delete would tombstone.
func (s *catalogPersistence) previewProjectDeletion(ctx context.Context, user authz.User, projectID string) (deletionPreview, error) {
	scope, err := s.authz.AuthorizeProject(ctx, user, projectID, authz.Read)
	if err != nil {
		return deletionPreview{}, err
	}
	var preview deletionPreview
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		preview.Environments, err = listPreviewEnvironments(ctx, tx, scope.ID())
		if err != nil {
			return err
		}
		preview.Services, err = listPreviewServices(ctx, tx, `e.project_id = $1`, scope.ID())
		if err != nil {
			return err
		}
		preview.Domains, err = listPreviewDomains(ctx, tx, `e.project_id = $1`, scope.ID())
		if err != nil {
			return err
		}
		preview.Volumes, err = listPreviewVolumes(ctx, tx, `e.project_id = $1`, scope.ID())
		return err
	})
	return preview, err
}

// previewEnvironmentDeletion lists the live services, domains, and volumes
// an environment delete would tombstone.
func (s *catalogPersistence) previewEnvironmentDeletion(ctx context.Context, user authz.User, environmentID string) (deletionPreview, error) {
	scope, err := s.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Read)
	if err != nil {
		return deletionPreview{}, err
	}
	var preview deletionPreview
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		preview.Services, err = listPreviewServices(ctx, tx, `s.environment_id = $1`, scope.ID())
		if err != nil {
			return err
		}
		preview.Domains, err = listPreviewDomains(ctx, tx, `s.environment_id = $1`, scope.ID())
		if err != nil {
			return err
		}
		preview.Volumes, err = listPreviewVolumes(ctx, tx, `v.environment_id = $1`, scope.ID())
		return err
	})
	return preview, err
}

// previewVolumeDeletion lists the live services referencing a volume and
// their live domains. A non-empty service list means deletion is refused.
func (s *catalogPersistence) previewVolumeDeletion(ctx context.Context, user authz.User, volumeID string) (deletionPreview, error) {
	scope, err := s.authz.AuthorizeVolume(ctx, user, volumeID, authz.Read)
	if err != nil {
		return deletionPreview{}, err
	}
	var preview deletionPreview
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var volumeName string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM volumes WHERE id = $1`, scope.ID()).Scan(&volumeName); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT s.id, s.name, r.spec_json
			   FROM live_services s
			   JOIN service_revisions r ON r.service_id = s.id AND r.spec_revision = s.current_spec_revision
			  WHERE s.environment_id = $1
			  ORDER BY s.name ASC, s.id ASC`,
			scope.EnvironmentID(),
		)
		if err != nil {
			return err
		}
		var serviceIDs []string
		for rows.Next() {
			var id, name string
			var rawSpec []byte
			if err := rows.Scan(&id, &name, &rawSpec); err != nil {
				rows.Close()
				return err
			}
			spec, err := deliverycore.LoadServiceSpec(rawSpec)
			if err != nil {
				rows.Close()
				return err
			}
			if deliverycore.ServiceVolumeName(spec) != volumeName {
				continue
			}
			preview.Services = append(preview.Services, previewService{ID: id, Name: name, EnvironmentID: scope.EnvironmentID()})
			serviceIDs = append(serviceIDs, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(serviceIDs) == 0 {
			return nil
		}
		domains, err := listPreviewDomainsForServices(ctx, tx, serviceIDs)
		if err != nil {
			return err
		}
		preview.Domains = domains
		return nil
	})
	return preview, err
}

func listPreviewEnvironments(ctx context.Context, tx *sql.Tx, projectID string) ([]previewEnvironment, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT e.id, e.name, e.is_production
		   FROM environments e
		   JOIN projects p ON p.id = e.project_id
		  WHERE e.project_id = $1
		    AND e.deleted_at IS NULL AND p.deleted_at IS NULL
		  ORDER BY e.is_production DESC, e.name ASC, e.id ASC`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []previewEnvironment
	for rows.Next() {
		var item previewEnvironment
		if err := rows.Scan(&item.ID, &item.Name, &item.IsProduction); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func listPreviewServices(ctx context.Context, tx *sql.Tx, predicate, arg string) ([]previewService, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT s.id, s.name, s.environment_id, e.name
		   FROM live_services s
		   JOIN environments e ON e.id = s.environment_id
		  WHERE `+predicate+`
		  ORDER BY e.name ASC, s.name ASC, s.id ASC`,
		arg,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []previewService
	for rows.Next() {
		var item previewService
		if err := rows.Scan(&item.ID, &item.Name, &item.EnvironmentID, &item.EnvironmentName); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func listPreviewDomains(ctx context.Context, tx *sql.Tx, predicate, arg string) ([]previewDomain, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT d.hostname, d.service_id, s.name, d.platform_generated
		   FROM domain_bindings d
		   JOIN live_services s ON s.id = d.service_id
		   JOIN environments e ON e.id = s.environment_id
		  WHERE `+predicate+`
		    AND d.deleted_at IS NULL
		  ORDER BY d.hostname ASC`,
		arg,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []previewDomain
	for rows.Next() {
		var item previewDomain
		if err := rows.Scan(&item.Hostname, &item.ServiceID, &item.ServiceName, &item.PlatformGenerated); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func listPreviewVolumes(ctx context.Context, tx *sql.Tx, predicate, arg string) ([]previewVolume, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT v.id, v.name, v.environment_id
		   FROM volumes v
		   JOIN environments e ON e.id = v.environment_id
		   JOIN projects p ON p.id = e.project_id
		  WHERE `+predicate+`
		    AND v.deleted_at IS NULL AND e.deleted_at IS NULL AND p.deleted_at IS NULL
		  ORDER BY e.name ASC, v.name ASC, v.id ASC`,
		arg,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []previewVolume
	for rows.Next() {
		var item previewVolume
		if err := rows.Scan(&item.ID, &item.Name, &item.EnvironmentID); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func listPreviewDomainsForServices(ctx context.Context, tx *sql.Tx, serviceIDs []string) ([]previewDomain, error) {
	if len(serviceIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(serviceIDs))
	args := make([]any, len(serviceIDs))
	for i, id := range serviceIDs {
		placeholders[i] = "$" + strconv.Itoa(i+1)
		args[i] = id
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT d.hostname, d.service_id, s.name, d.platform_generated
		   FROM domain_bindings d
		   JOIN services s ON s.id = d.service_id
		  WHERE d.service_id IN (`+strings.Join(placeholders, ", ")+`)
		    AND d.deleted_at IS NULL
		  ORDER BY d.hostname ASC`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []previewDomain
	for rows.Next() {
		var item previewDomain
		if err := rows.Scan(&item.Hostname, &item.ServiceID, &item.ServiceName, &item.PlatformGenerated); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
