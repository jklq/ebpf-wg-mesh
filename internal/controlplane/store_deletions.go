package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
)

// This file owns the project/environment/volume side of safe deletion:
// tombstone writes, restores, deletion previews, and the garbage-collection
// primitives the DeletionGC loop drives. Service tombstones live in the
// delivery package next to the deployment lifecycle; domain tombstones live
// in store_domains.go next to the binding rules. All of them share one
// model: delete tombstones (never destroys), the journal hides tombstoned
// subtrees from agents and ingress, restore clears within the grace period,
// and garbage collection physically deletes expired tombstones.

// lockProjectTx locks a user project row and loads it with its deletion
// state. Managed projects resolve through the same row; callers that must
// refuse them check the kind.
func (s *catalogPersistence) lockProjectTx(ctx context.Context, tx *sql.Tx, scope authz.Project) (deliverycore.ProjectRecord, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT p.id, p.name, p.kind, COALESCE(p.system_key, ''), p.created_at,
		        p.deleted_at, p.deleted_by_user_id, p.delete_expires_at
		   FROM projects p
		  WHERE p.id = $1 AND p.kind = $2 FOR UPDATE OF p`,
		scope.ID(),
		string(deliverycore.ProjectKindUser),
	)
	return deliverycore.ScanProjectRow(row)
}

func (s *catalogPersistence) tombstoneProjectTx(ctx context.Context, tx *sql.Tx, projectID, userID string, now time.Time) (bool, error) {
	result, err := tx.ExecContext(ctx,
		`UPDATE projects
		    SET deleted_at = $1,
		        deleted_by_user_id = $2,
		        delete_expires_at = $3
		  WHERE id = $4 AND deleted_at IS NULL`,
		now, userID, now.Add(s.deletionGracePeriod()), projectID,
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

func (s *catalogPersistence) clearProjectTombstoneTx(ctx context.Context, tx *sql.Tx, projectID string) (bool, error) {
	result, err := tx.ExecContext(ctx,
		`UPDATE projects
		    SET deleted_at = NULL,
		        deleted_by_user_id = '',
		        delete_expires_at = NULL
		  WHERE id = $1 AND deleted_at IS NOT NULL`,
		projectID,
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

// lockEnvironmentTx locks an environment row and loads it with its effective
// deletion state.
func (s *catalogPersistence) lockEnvironmentTx(ctx context.Context, tx *sql.Tx, scope authz.Environment) (deliverycore.EnvironmentRecord, error) {
	row := tx.QueryRowContext(ctx, environmentSelect+`
		 WHERE e.id = $1 AND e.project_id = $2 FOR UPDATE OF e`,
		scope.ID(), scope.ProjectID())
	return deliverycore.ScanEnvironmentRow(row)
}

func (s *catalogPersistence) tombstoneEnvironmentTx(ctx context.Context, tx *sql.Tx, environmentID, userID string, now time.Time) (bool, error) {
	result, err := tx.ExecContext(ctx,
		`UPDATE environments
		    SET deleted_at = $1,
		        deleted_by_user_id = $2,
		        delete_expires_at = $3
		  WHERE id = $4 AND deleted_at IS NULL`,
		now, userID, now.Add(s.deletionGracePeriod()), environmentID,
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

func (s *catalogPersistence) clearEnvironmentTombstoneTx(ctx context.Context, tx *sql.Tx, environmentID string) (bool, error) {
	result, err := tx.ExecContext(ctx,
		`UPDATE environments
		    SET deleted_at = NULL,
		        deleted_by_user_id = '',
		        delete_expires_at = NULL
		  WHERE id = $1 AND deleted_at IS NOT NULL`,
		environmentID,
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
	result, err := tx.ExecContext(ctx,
		`UPDATE volumes
		    SET deleted_at = $1,
		        deleted_by_user_id = $2,
		        delete_expires_at = $3
		  WHERE id = $4 AND deleted_at IS NULL`,
		now, userID, now.Add(s.deletionGracePeriod()), volumeID,
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

// dropEnvironmentAssignmentsTx deletes stale assignments for a restored
// environment. Assignments reference the Removed deployment the delete left
// behind; a release recreates them.
func dropEnvironmentAssignmentsTx(ctx context.Context, tx *sql.Tx, environmentID string) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM allocation_assignments a USING services s
		  WHERE a.service_id = s.id AND s.environment_id = $1`,
		environmentID,
	)
	return err
}

// dropProjectAssignmentsTx deletes stale assignments for a restored project.
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
		if rec.Kind == deliverycore.ProjectKindManaged {
			return deliverycore.ErrManagedProjectProtected
		}
		if err := deliverycore.CheckDeletionConfirmation(rec.Name, confirmation); err != nil {
			return err
		}
		now := time.Now().UTC()
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
		return err
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
			rec, err = s.projectByScopeQuerier(ctx, tx, scope)
			return err
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

// DeletionPreview reports the live dependents a delete would tombstone.
type DeletionPreview struct {
	Environments []PreviewEnvironment
	Services     []PreviewService
	Domains      []PreviewDomain
	Volumes      []PreviewVolume
}

// PreviewEnvironment is one environment in a deletion preview.
type PreviewEnvironment struct {
	ID           string
	Name         string
	IsProduction bool
}

// PreviewService is one service in a deletion preview.
type PreviewService struct {
	ID              string
	Name            string
	EnvironmentID   string
	EnvironmentName string
}

// PreviewDomain is one domain binding in a deletion preview.
type PreviewDomain struct {
	Hostname          string
	ServiceID         string
	ServiceName       string
	PlatformGenerated bool
}

// PreviewVolume is one volume in a deletion preview.
type PreviewVolume struct {
	ID            string
	Name          string
	EnvironmentID string
}

// previewProjectDeletion lists the live environments, services, domains, and
// volumes a project delete would tombstone.
func (s *catalogPersistence) previewProjectDeletion(ctx context.Context, user authz.User, projectID string) (DeletionPreview, error) {
	scope, err := s.authz.AuthorizeProject(ctx, user, projectID, authz.Read)
	if err != nil {
		return DeletionPreview{}, err
	}
	var preview DeletionPreview
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
func (s *catalogPersistence) previewEnvironmentDeletion(ctx context.Context, user authz.User, environmentID string) (DeletionPreview, error) {
	scope, err := s.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Read)
	if err != nil {
		return DeletionPreview{}, err
	}
	var preview DeletionPreview
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
func (s *catalogPersistence) previewVolumeDeletion(ctx context.Context, user authz.User, volumeID string) (DeletionPreview, error) {
	scope, err := s.authz.AuthorizeVolume(ctx, user, volumeID, authz.Read)
	if err != nil {
		return DeletionPreview{}, err
	}
	var preview DeletionPreview
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var volumeName string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM volumes WHERE id = $1`, scope.ID()).Scan(&volumeName); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT s.id, s.name, r.spec_json
			   FROM services s
			   JOIN service_revisions r ON r.service_id = s.id AND r.spec_revision = s.current_spec_revision
			   JOIN environments e ON e.id = s.environment_id
			   JOIN projects p ON p.id = e.project_id
			  WHERE s.environment_id = $1
			    AND s.deleted_at IS NULL AND e.deleted_at IS NULL AND p.deleted_at IS NULL
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
			preview.Services = append(preview.Services, PreviewService{ID: id, Name: name, EnvironmentID: scope.EnvironmentID()})
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

func listPreviewEnvironments(ctx context.Context, tx *sql.Tx, projectID string) ([]PreviewEnvironment, error) {
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
	var out []PreviewEnvironment
	for rows.Next() {
		var item PreviewEnvironment
		if err := rows.Scan(&item.ID, &item.Name, &item.IsProduction); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func listPreviewServices(ctx context.Context, tx *sql.Tx, predicate, arg string) ([]PreviewService, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT s.id, s.name, s.environment_id, e.name
		   FROM services s
		   JOIN environments e ON e.id = s.environment_id
		   JOIN projects p ON p.id = e.project_id
		  WHERE `+predicate+`
		    AND s.deleted_at IS NULL AND e.deleted_at IS NULL AND p.deleted_at IS NULL
		  ORDER BY e.name ASC, s.name ASC, s.id ASC`,
		arg,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PreviewService
	for rows.Next() {
		var item PreviewService
		if err := rows.Scan(&item.ID, &item.Name, &item.EnvironmentID, &item.EnvironmentName); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func listPreviewDomains(ctx context.Context, tx *sql.Tx, predicate, arg string) ([]PreviewDomain, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT d.hostname, d.service_id, s.name, d.platform_generated
		   FROM domain_bindings d
		   JOIN services s ON s.id = d.service_id
		   JOIN environments e ON e.id = s.environment_id
		   JOIN projects p ON p.id = e.project_id
		  WHERE `+predicate+`
		    AND d.deleted_at IS NULL
		    AND s.deleted_at IS NULL AND e.deleted_at IS NULL AND p.deleted_at IS NULL
		  ORDER BY d.hostname ASC`,
		arg,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PreviewDomain
	for rows.Next() {
		var item PreviewDomain
		if err := rows.Scan(&item.Hostname, &item.ServiceID, &item.ServiceName, &item.PlatformGenerated); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func listPreviewVolumes(ctx context.Context, tx *sql.Tx, predicate, arg string) ([]PreviewVolume, error) {
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
	var out []PreviewVolume
	for rows.Next() {
		var item PreviewVolume
		if err := rows.Scan(&item.ID, &item.Name, &item.EnvironmentID); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// Expired-deletion kinds collected by garbage collection.
const (
	ExpiredDeletionProject     = "project"
	ExpiredDeletionEnvironment = "environment"
	ExpiredDeletionService     = "service"
	ExpiredDeletionVolume      = "volume"
	ExpiredDeletionDomain      = "domain"
)

// ExpiredDeletion is one tombstone whose grace period ended.
type ExpiredDeletion struct {
	Kind string
	ID   string
}

// listExpiredDeletions returns up to limit tombstones per resource kind whose
// grace period ended at or before cutoff, oldest first.
func (p *persistence) listExpiredDeletions(ctx context.Context, cutoff time.Time, limit int) ([]ExpiredDeletion, error) {
	if limit <= 0 {
		limit = 100
	}
	queries := []struct {
		kind  string
		query string
	}{
		{ExpiredDeletionProject, `SELECT id FROM projects WHERE deleted_at IS NOT NULL AND delete_expires_at <= $1 ORDER BY delete_expires_at ASC, id ASC LIMIT $2`},
		{ExpiredDeletionEnvironment, `SELECT id FROM environments WHERE deleted_at IS NOT NULL AND delete_expires_at <= $1 ORDER BY delete_expires_at ASC, id ASC LIMIT $2`},
		{ExpiredDeletionService, `SELECT id FROM services WHERE deleted_at IS NOT NULL AND delete_expires_at <= $1 ORDER BY delete_expires_at ASC, id ASC LIMIT $2`},
		{ExpiredDeletionVolume, `SELECT id FROM volumes WHERE deleted_at IS NOT NULL AND delete_expires_at <= $1 ORDER BY delete_expires_at ASC, id ASC LIMIT $2`},
		{ExpiredDeletionDomain, `SELECT hostname FROM domain_bindings WHERE deleted_at IS NOT NULL AND delete_expires_at <= $1 ORDER BY delete_expires_at ASC, hostname ASC LIMIT $2`},
	}
	var out []ExpiredDeletion
	for _, item := range queries {
		rows, err := p.db.QueryContext(ctx, item.query, cutoff, limit)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, ExpiredDeletion{Kind: item.kind, ID: id})
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// hardDeleteExpired irreversibly deletes one expired tombstone. It reports
// whether a row was destroyed; a restored or concurrently collected row is a
// no-op success, so retries and concurrent collectors stay idempotent.
func (p *persistence) hardDeleteExpired(ctx context.Context, deletion ExpiredDeletion, cutoff time.Time) (bool, error) {
	switch deletion.Kind {
	case ExpiredDeletionProject:
		return hardDeleteExpiredRow(ctx, p.database, deletion.ID, cutoff,
			`SELECT id FROM projects WHERE id = $1 AND deleted_at IS NOT NULL AND delete_expires_at <= $2 FOR UPDATE`,
			func(ctx context.Context, tx *sql.Tx, id string) error {
				return journal.RecordProjectRemoval(ctx, tx, id)
			},
			`DELETE FROM projects WHERE id = $1`)
	case ExpiredDeletionEnvironment:
		return hardDeleteExpiredRow(ctx, p.database, deletion.ID, cutoff,
			`SELECT id FROM environments WHERE id = $1 AND deleted_at IS NOT NULL AND delete_expires_at <= $2 FOR UPDATE`,
			func(ctx context.Context, tx *sql.Tx, id string) error {
				return journal.RecordEnvironmentRemoval(ctx, tx, id)
			},
			`DELETE FROM environments WHERE id = $1`)
	case ExpiredDeletionService:
		return hardDeleteExpiredRow(ctx, p.database, deletion.ID, cutoff,
			`SELECT id FROM services WHERE id = $1 AND deleted_at IS NOT NULL AND delete_expires_at <= $2 FOR UPDATE`,
			func(ctx context.Context, tx *sql.Tx, id string) error {
				return journal.RecordServiceRemoval(ctx, tx, id)
			},
			`DELETE FROM services WHERE id = $1`)
	case ExpiredDeletionVolume:
		return hardDeleteExpiredRow(ctx, p.database, deletion.ID, cutoff,
			`SELECT id FROM volumes WHERE id = $1 AND deleted_at IS NOT NULL AND delete_expires_at <= $2 FOR UPDATE`,
			func(ctx context.Context, tx *sql.Tx, id string) error { journal.RecordVolume(ctx, id); return nil },
			`DELETE FROM volumes WHERE id = $1`)
	case ExpiredDeletionDomain:
		return hardDeleteExpiredDomain(ctx, p.database, deletion.ID, cutoff)
	default:
		return false, nil
	}
}

func hardDeleteExpiredRow(ctx context.Context, db *database, id string, cutoff time.Time, lockQuery string, record func(context.Context, *sql.Tx, string) error, deleteQuery string) (bool, error) {
	var collected bool
	err := db.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var found string
		if err := tx.QueryRowContext(ctx, lockQuery, id, cutoff).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if err := record(ctx, tx, found); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, deleteQuery, found); err != nil {
			return err
		}
		collected = true
		return nil
	})
	return collected, err
}

func hardDeleteExpiredDomain(ctx context.Context, db *database, hostname string, cutoff time.Time) (bool, error) {
	var collected bool
	err := db.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var serviceID string
		err := tx.QueryRowContext(ctx,
			`SELECT service_id FROM domain_bindings WHERE hostname = $1 AND deleted_at IS NOT NULL AND delete_expires_at <= $2 FOR UPDATE`,
			hostname, cutoff,
		).Scan(&serviceID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		journal.RecordDomain(ctx, hostname, serviceID)
		if _, err := tx.ExecContext(ctx, `DELETE FROM domain_bindings WHERE hostname = $1`, hostname); err != nil {
			return err
		}
		collected = true
		return nil
	})
	return collected, err
}

func listPreviewDomainsForServices(ctx context.Context, tx *sql.Tx, serviceIDs []string) ([]PreviewDomain, error) {
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
	var out []PreviewDomain
	for rows.Next() {
		var item PreviewDomain
		if err := rows.Scan(&item.Hostname, &item.ServiceID, &item.ServiceName, &item.PlatformGenerated); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
