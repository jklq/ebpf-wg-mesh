package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/logs"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// CatalogOperations serves project and environment deletes and restores,
// fanning deletions out to agent wakeups and ingress syncs.
type catalogOperations struct {
	store    catalogStore
	notifier deliverycore.PlatformNotifier
	ingress  deliverycore.PlatformIngress
}

// newCatalogOperations builds the catalog delete/restore operator.
func newCatalogOperations(store catalogStore, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress) *catalogOperations {
	return &catalogOperations{store: store, notifier: notifier, ingress: ingress}
}

func (s *catalogOperations) DeleteEnvironment(ctx context.Context, req *platformv1.DeleteEnvironmentRequest) (*emptypb.Empty, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	agentIDs, err := s.store.deleteEnvironment(ctx, user, req.GetEnvironmentId(), req.GetConfirmationName())
	if err != nil {
		if errors.Is(err, deliverycore.ErrNotLiveOwner) || errors.Is(err, deliverycore.ErrLeaseLost) {
			return nil, err
		}
		if errors.Is(err, deliverycore.ErrConfirmationMismatch) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, writeAccessError("delete environment", err)
	}
	for _, agentID := range agentIDs {
		s.notifier.Notify(agentID)
	}
	s.ingress.RequestSync()
	return &emptypb.Empty{}, nil
}

func (s *catalogOperations) RestoreEnvironment(ctx context.Context, environmentID string) (*platformv1.Environment, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.store.restoreEnvironment(ctx, user, environmentID)
	if err != nil {
		if errors.Is(err, deliverycore.ErrNotLiveOwner) || errors.Is(err, deliverycore.ErrLeaseLost) {
			return nil, err
		}
		if errors.Is(err, deliverycore.ErrAncestorDeleted) || errors.Is(err, deliverycore.ErrDeletionExpired) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, writeAccessError("restore environment", err)
	}
	// Restored rows re-enter the journal; wake everyone so desired state and
	// ingress converge without waiting for the next natural tick.
	if ids, err := s.store.AgentIDs(ctx); err == nil {
		for _, agentID := range ids {
			s.notifier.Notify(agentID)
		}
	}
	s.ingress.RequestSync()
	return toProtoEnvironment(rec), nil
}

func (s *catalogOperations) DeleteProject(ctx context.Context, req *platformv1.DeleteProjectRequest) (*emptypb.Empty, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	agentIDs, err := s.store.deleteProject(ctx, user, req.GetProjectId(), req.GetConfirmationName())
	if err != nil {
		if errors.Is(err, deliverycore.ErrNotLiveOwner) || errors.Is(err, deliverycore.ErrLeaseLost) {
			return nil, err
		}
		if errors.Is(err, deliverycore.ErrConfirmationMismatch) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, writeAccessError("delete project", err)
	}
	for _, agentID := range agentIDs {
		s.notifier.Notify(agentID)
	}
	s.ingress.RequestSync()
	return &emptypb.Empty{}, nil
}

func (s *catalogOperations) RestoreProject(ctx context.Context, projectID string) (*platformv1.Project, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.store.restoreProject(ctx, user, projectID)
	if err != nil {
		if errors.Is(err, deliverycore.ErrNotLiveOwner) || errors.Is(err, deliverycore.ErrLeaseLost) {
			return nil, err
		}
		if errors.Is(err, deliverycore.ErrDeletionExpired) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, writeAccessError("restore project", err)
	}
	if ids, err := s.store.AgentIDs(ctx); err == nil {
		for _, agentID := range ids {
			s.notifier.Notify(agentID)
		}
	}
	s.ingress.RequestSync()
	return toProtoProject(rec), nil
}

func (s *catalogPersistence) ensureUserProjectNamedQuerier(ctx context.Context, q deliverycore.ServiceQueryer, userID, name string) (string, error) {
	project, found, err := s.projectByOwnedNameQuerier(ctx, q, userID, name)
	if err != nil {
		return "", err
	}
	if found {
		if project.Deletion != nil {
			return "", fmt.Errorf("%w: restore %q or wait until %s", deliverycore.ErrProjectDeleted, project.Name, project.Deletion.ExpiresAt.Format(time.RFC3339))
		}
		if _, err := s.ensureProductionEnvironmentQuerier(ctx, q, project.ID); err != nil {
			return "", fmt.Errorf("ensure production environment: %w", err)
		}
		return project.ID, nil
	}

	id := uuid.NewString()
	now := time.Now().UTC()
	if _, err := q.ExecContext(
		ctx,
		`INSERT INTO projects(id, owner_user_id, name, kind, system_key, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		id,
		userID,
		name,
		string(deliverycore.ProjectKindUser),
		nil,
		now,
	); err != nil {
		return "", fmt.Errorf("insert project: %w", err)
	}
	journal.RecordProject(ctx, id)
	if _, err := s.createEnvironmentQuerier(ctx, q, id, "Production", true, ""); err != nil {
		return "", fmt.Errorf("create production environment: %w", err)
	}
	return id, nil
}

func (s *catalogPersistence) ensureProjectOwnerMembershipQuerier(ctx context.Context, q deliverycore.ServiceQueryer, userID, projectID string) error {
	_, err := q.ExecContext(
		ctx,
		`INSERT INTO project_memberships(user_id, project_id, role) VALUES ($1, $2, $3)
		 ON CONFLICT(user_id, project_id) DO UPDATE SET role = excluded.role`,
		userID,
		projectID,
		"owner",
	)
	return err
}

func (s *catalogPersistence) ensureManagedProject(ctx context.Context, name, systemKey string) (deliverycore.ProjectRecord, error) {
	var project deliverycore.ProjectRecord
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, found, err := s.projectBySystemKeyQuerier(ctx, tx, systemKey)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if !found {
			project = deliverycore.ProjectRecord{
				ID:        uuid.NewString(),
				Name:      name,
				Kind:      deliverycore.ProjectKindManaged,
				SystemKey: systemKey,
				CreatedAt: now,
			}
			_, err = tx.ExecContext(
				ctx,
				`INSERT INTO projects(id, owner_user_id, name, kind, system_key, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
				project.ID,
				"",
				project.Name,
				string(project.Kind),
				project.SystemKey,
				project.CreatedAt,
			)
			if err != nil {
				return err
			}
			journal.RecordProject(ctx, project.ID)
			_, err = s.createEnvironmentQuerier(ctx, tx, project.ID, "Production", true, "")
			return err
		}
		if current.Name != name || current.Kind != deliverycore.ProjectKindManaged {
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE projects SET name = $1, kind = $2 WHERE id = $3`,
				name,
				string(deliverycore.ProjectKindManaged),
				current.ID,
			); err != nil {
				return err
			}
			journal.RecordProject(ctx, current.ID)
			current.Name = name
			current.Kind = deliverycore.ProjectKindManaged
		}
		if _, err := s.ensureProductionEnvironmentQuerier(ctx, tx, current.ID); err != nil {
			return err
		}
		project = current
		return nil
	})
	if err != nil {
		return deliverycore.ProjectRecord{}, err
	}
	return project, nil
}

func (s *catalogPersistence) createProject(ctx context.Context, user authz.User, name string) (deliverycore.ProjectRecord, error) {
	var project deliverycore.ProjectRecord
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		id, err := s.ensureUserProjectNamedQuerier(ctx, tx, user.ID(), name)
		if err != nil {
			return err
		}
		if err := s.ensureProjectOwnerMembershipQuerier(ctx, tx, user.ID(), id); err != nil {
			return err
		}
		project, err = s.projectByIDInternalQuerier(ctx, tx, id)
		return err
	})
	if err != nil {
		return deliverycore.ProjectRecord{}, err
	}
	return project, nil
}

func (s *catalogPersistence) listProjects(ctx context.Context, user authz.User, includeDeleted bool) ([]deliverycore.ProjectRecord, error) {
	filter := ` AND p.deleted_at IS NULL`
	if includeDeleted {
		filter = ``
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT p.id, p.name, p.kind, COALESCE(p.system_key, ''), p.created_at,
		        p.deleted_at, p.deleted_by_user_id, p.delete_expires_at, p.log_retention_days
		   FROM projects p
		   JOIN project_memberships m ON m.project_id = p.id
		  WHERE m.user_id = $1
		    AND m.role IN ('owner', 'editor', 'viewer')
		    AND p.kind = $2`+filter+`
		  ORDER BY p.created_at ASC`,
		user.ID(),
		string(deliverycore.ProjectKindUser),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []deliverycore.ProjectRecord
	for rows.Next() {
		rec, err := deliverycore.ScanProjectRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *catalogPersistence) projectByID(ctx context.Context, user authz.User, projectID string) (deliverycore.ProjectRecord, error) {
	scope, err := s.authz.AuthorizeProject(ctx, user, projectID, authz.Read)
	if err != nil {
		return deliverycore.ProjectRecord{}, err
	}
	return s.projectByScopeQuerier(ctx, s.db, scope)
}

func (s *catalogPersistence) projectByScopeQuerier(ctx context.Context, q deliverycore.ServiceQueryer, scope authz.Project) (deliverycore.ProjectRecord, error) {
	row := q.QueryRowContext(
		ctx,
		`SELECT p.id, p.name, p.kind, COALESCE(p.system_key, ''), p.created_at,
		        p.deleted_at, p.deleted_by_user_id, p.delete_expires_at, p.log_retention_days
		   FROM projects p
		  WHERE p.id = $1 AND p.kind = $2`,
		scope.ID(),
		string(deliverycore.ProjectKindUser),
	)
	return deliverycore.ScanProjectRow(row)
}

func (s *catalogPersistence) projectBySystemKeyQuerier(ctx context.Context, q deliverycore.ServiceQueryer, systemKey string) (deliverycore.ProjectRecord, bool, error) {
	row := q.QueryRowContext(
		ctx,
		`SELECT id, name, kind, COALESCE(system_key, ''), created_at,
		        deleted_at, deleted_by_user_id, delete_expires_at, log_retention_days
		   FROM projects
		  WHERE system_key = $1`,
		systemKey,
	)
	rec, err := deliverycore.ScanProjectRow(row)
	switch {
	case err == nil:
		return rec, true, nil
	case err == sql.ErrNoRows:
		return deliverycore.ProjectRecord{}, false, nil
	default:
		return deliverycore.ProjectRecord{}, false, err
	}
}

func (s *catalogPersistence) projectByOwnedNameQuerier(ctx context.Context, q deliverycore.ServiceQueryer, userID, name string) (deliverycore.ProjectRecord, bool, error) {
	row := q.QueryRowContext(
		ctx,
		`SELECT id, name, kind, COALESCE(system_key, ''), created_at,
		        deleted_at, deleted_by_user_id, delete_expires_at, log_retention_days
		   FROM projects
		  WHERE owner_user_id = $1 AND name = $2 AND kind = $3`,
		userID,
		name,
		string(deliverycore.ProjectKindUser),
	)
	rec, err := deliverycore.ScanProjectRow(row)
	switch {
	case err == nil:
		return rec, true, nil
	case err == sql.ErrNoRows:
		return deliverycore.ProjectRecord{}, false, nil
	default:
		return deliverycore.ProjectRecord{}, false, err
	}
}

var errInvalidLogRetention = errors.New("log retention must be 0 (platform default) or 1..90 days")

func (s *catalogPersistence) updateProjectLogRetention(ctx context.Context, user authz.User, projectID string, retentionDays int32) (deliverycore.ProjectRecord, error) {
	scope, err := s.authz.AuthorizeProject(ctx, user, projectID, authz.Write)
	if err != nil {
		return deliverycore.ProjectRecord{}, err
	}
	if retentionDays < 0 || retentionDays > 90 {
		return deliverycore.ProjectRecord{}, errInvalidLogRetention
	}
	var rec deliverycore.ProjectRecord
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := s.projectByScopeQuerier(ctx, tx, scope)
		if err != nil {
			return err
		}
		if current.Deletion != nil {
			return deliverycore.ErrProjectDeleted
		}
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET log_retention_days = $1 WHERE id = $2`, retentionDays, current.ID); err != nil {
			return err
		}
		journal.RecordProject(ctx, current.ID)
		current.LogRetentionDays = retentionDays
		rec = current
		return nil
	})
	return rec, err
}

func (s *catalogPersistence) projectByIDInternalQuerier(ctx context.Context, q deliverycore.ServiceQueryer, projectID string) (deliverycore.ProjectRecord, error) {
	row := q.QueryRowContext(
		ctx,
		`SELECT id, name, kind, COALESCE(system_key, ''), created_at,
		        deleted_at, deleted_by_user_id, delete_expires_at, log_retention_days
		   FROM projects
		  WHERE id = $1`,
		projectID,
	)
	return deliverycore.ScanProjectRow(row)
}

// resolveLogRetention maps service IDs to their project's log
// retention policy for the log pipeline's per-row expiry. Unknown
// services resolve to the zero policy, which keeps the platform
// default.
func (s *catalogPersistence) resolveLogRetention(ctx context.Context, serviceIDs []string) (map[string]logs.ProjectRetention, error) {
	out := make(map[string]logs.ProjectRetention, len(serviceIDs))
	if len(serviceIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, e.project_id, COALESCE(p.log_retention_days, 0)
		   FROM services s
		   JOIN environments e ON e.id = s.environment_id
		   JOIN projects p ON p.id = e.project_id
		  WHERE s.id = ANY($1)`,
		serviceIDs,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var serviceID string
		var policy logs.ProjectRetention
		if err := rows.Scan(&serviceID, &policy.ProjectID, &policy.RetentionDays); err != nil {
			return nil, err
		}
		out[serviceID] = policy
	}
	return out, rows.Err()
}
