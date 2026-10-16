package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/identity"

	"github.com/google/uuid"
)

const (
	personalWorkspaceName  = "My Projects"
	maxWorkspaceNameLength = 64
)

var (
	errWorkspaceNameRequired  = errors.New("workspace name is required")
	errWorkspaceNameTooLong   = fmt.Errorf("workspace name must be at most %d characters", maxWorkspaceNameLength)
	errPersonalWorkspace      = errors.New("personal workspaces have exactly one member and cannot be deleted; create a workspace to collaborate")
	errLastWorkspaceAdmin     = errors.New("a workspace needs at least one admin")
	errWorkspaceHasProjects   = errors.New("move or delete the workspace's projects first")
	errInvalidInvitationEmail = errors.New("a valid email address is required")
	errInvitationNotFound     = errors.New("invitation not found")
	errWorkspaceMemberMissing = errors.New("workspace member not found")
)

type workspaceRecord struct {
	ID        string
	Name      string
	Personal  bool
	CreatedAt time.Time
	// Role is the reading user's role.
	Role authz.Role
}

type workspaceMemberRecord struct {
	UserID   string
	Role     authz.Role
	JoinedAt time.Time
}

type workspaceInvitationRecord struct {
	ID              string
	WorkspaceID     string
	WorkspaceName   string
	Email           string
	Role            authz.Role
	InvitedByUserID string
	CreatedAt       time.Time
}

// workspaceStore owns workspaces, their memberships, and invitations. Email
// arguments are the caller's verified address from the user assertion.
type workspaceStore interface {
	listWorkspaces(ctx context.Context, user authz.User, email string) ([]workspaceRecord, []workspaceInvitationRecord, error)
	workspaceByID(ctx context.Context, user authz.User, workspaceID string) (workspaceRecord, error)
	createWorkspace(ctx context.Context, user authz.User, name string) (workspaceRecord, error)
	renameWorkspace(ctx context.Context, user authz.User, workspaceID, name string) (workspaceRecord, error)
	deleteWorkspace(ctx context.Context, user authz.User, workspaceID, confirmation string) error
	listWorkspaceMembers(ctx context.Context, user authz.User, workspaceID string) ([]workspaceMemberRecord, []workspaceInvitationRecord, error)
	inviteWorkspaceMember(ctx context.Context, user authz.User, workspaceID, email string, role authz.Role) (workspaceInvitationRecord, error)
	revokeWorkspaceInvitation(ctx context.Context, user authz.User, invitationID string) error
	acceptWorkspaceInvitation(ctx context.Context, user authz.User, email, invitationID string) (workspaceRecord, error)
	declineWorkspaceInvitation(ctx context.Context, user authz.User, email, invitationID string) error
	updateWorkspaceMemberRole(ctx context.Context, user authz.User, workspaceID, memberUserID string, role authz.Role) (workspaceMemberRecord, error)
	removeWorkspaceMember(ctx context.Context, user authz.User, workspaceID, memberUserID string) error
	moveProject(ctx context.Context, user authz.User, projectID, workspaceID string) (deliverycore.ProjectRecord, error)
}

func normalizeWorkspaceName(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", errWorkspaceNameRequired
	case len([]rune(name)) > maxWorkspaceNameLength:
		return "", errWorkspaceNameTooLong
	}
	return name, nil
}

func normalizeInvitationEmail(email string) (string, error) {
	email = identity.NormalizeEmail(email)
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || len(email) > 254 {
		return "", errInvalidInvitationEmail
	}
	return email, nil
}

// ensurePersonalWorkspaceQuerier returns the user's personal workspace,
// creating it with the user as its admin. It is idempotent under concurrency.
func ensurePersonalWorkspaceQuerier(ctx context.Context, q deliverycore.ServiceQueryer, userID string) (workspaceRecord, error) {
	now := time.Now().UTC()
	if _, err := q.ExecContext(ctx,
		`INSERT INTO workspaces(id, name, personal_user_id, created_at) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (personal_user_id) WHERE personal_user_id IS NOT NULL DO NOTHING`,
		uuid.NewString(), personalWorkspaceName, userID, now); err != nil {
		return workspaceRecord{}, err
	}
	rec := workspaceRecord{Personal: true, Role: authz.RoleAdmin}
	if err := q.QueryRowContext(ctx,
		`SELECT id, name, created_at FROM workspaces WHERE personal_user_id = $1`, userID,
	).Scan(&rec.ID, &rec.Name, &rec.CreatedAt); err != nil {
		return workspaceRecord{}, err
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO workspace_memberships(workspace_id, user_id, role, created_at) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (workspace_id, user_id) DO NOTHING`,
		rec.ID, userID, string(authz.RoleAdmin), now); err != nil {
		return workspaceRecord{}, err
	}
	return rec, nil
}

func (s *catalogPersistence) ensurePersonalWorkspace(ctx context.Context, user authz.User) (workspaceRecord, error) {
	var rec workspaceRecord
	err := s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		rec, err = ensurePersonalWorkspaceQuerier(ctx, tx, user.ID())
		if err != nil {
			return err
		}
		return syncGitHubProfileTx(ctx, tx, user.ID())
	})
	return rec, err
}

// syncGitHubProfileTx records the delegated user's GitHub account age
// evidence for billing's trial gate. It runs on the personal-workspace
// ensure every sign-in passes through. Users whose assertions carry no
// GitHub claim (bootstrap and dev users) get no profile row.
func syncGitHubProfileTx(ctx context.Context, tx *sql.Tx, userID string) error {
	delegated, err := identity.DelegatedUserFromContext(ctx)
	if err != nil || delegated.UserID != userID || delegated.GitHubCreatedAt == nil {
		return nil
	}
	var created sql.NullTime
	if *delegated.GitHubCreatedAt > 0 {
		created = sql.NullTime{Time: time.Unix(*delegated.GitHubCreatedAt, 0).UTC(), Valid: true}
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO user_profiles(user_id, github_created_at, updated_at) VALUES ($1, $2, statement_timestamp())
		 ON CONFLICT (user_id) DO UPDATE
		   SET github_created_at = excluded.github_created_at, updated_at = excluded.updated_at
		 WHERE user_profiles.github_created_at IS DISTINCT FROM excluded.github_created_at`,
		userID, created)
	return err
}

func (s *catalogPersistence) listWorkspaces(ctx context.Context, user authz.User, email string) ([]workspaceRecord, []workspaceInvitationRecord, error) {
	if _, err := s.ensurePersonalWorkspace(ctx, user); err != nil {
		return nil, nil, fmt.Errorf("ensure personal workspace: %w", err)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT w.id, w.name, w.personal_user_id IS NOT NULL, w.created_at, m.role
		   FROM workspaces w
		   JOIN workspace_memberships m ON m.workspace_id = w.id
		  WHERE m.user_id = $1
		  ORDER BY w.personal_user_id IS NULL, w.created_at, w.id`,
		user.ID())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var workspaces []workspaceRecord
	for rows.Next() {
		rec, err := scanWorkspace(rows)
		if err != nil {
			return nil, nil, err
		}
		workspaces = append(workspaces, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if email == "" {
		return workspaces, nil, nil
	}
	invitations, err := s.queryInvitations(ctx, `i.email = $1`, email)
	if err != nil {
		return nil, nil, err
	}
	return workspaces, invitations, nil
}

func (s *catalogPersistence) workspaceByID(ctx context.Context, user authz.User, workspaceID string) (workspaceRecord, error) {
	scope, err := s.authz.AuthorizeWorkspace(ctx, user, workspaceID, authz.Read)
	if err != nil {
		return workspaceRecord{}, err
	}
	return s.workspaceByScopeQuerier(ctx, s.db, scope)
}

func (s *catalogPersistence) workspaceByScopeQuerier(ctx context.Context, q deliverycore.ServiceQueryer, scope authz.Workspace) (workspaceRecord, error) {
	return scanWorkspace(q.QueryRowContext(ctx,
		`SELECT id, name, personal_user_id IS NOT NULL, created_at, $2::TEXT FROM workspaces WHERE id = $1`,
		scope.ID(), string(scope.Role())))
}

func scanWorkspace(scanner interface{ Scan(...any) error }) (workspaceRecord, error) {
	var rec workspaceRecord
	var role string
	if err := scanner.Scan(&rec.ID, &rec.Name, &rec.Personal, &rec.CreatedAt, &role); err != nil {
		return workspaceRecord{}, err
	}
	rec.Role = authz.Role(role)
	return rec, nil
}

func (s *catalogPersistence) createWorkspace(ctx context.Context, user authz.User, name string) (workspaceRecord, error) {
	name, err := normalizeWorkspaceName(name)
	if err != nil {
		return workspaceRecord{}, err
	}
	rec := workspaceRecord{ID: uuid.NewString(), Name: name, CreatedAt: time.Now().UTC(), Role: authz.RoleAdmin}
	err = s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workspaces(id, name, created_at) VALUES ($1, $2, $3)`,
			rec.ID, rec.Name, rec.CreatedAt); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO workspace_memberships(workspace_id, user_id, role, created_at) VALUES ($1, $2, $3, $4)`,
			rec.ID, user.ID(), string(authz.RoleAdmin), rec.CreatedAt)
		return err
	})
	if err != nil {
		return workspaceRecord{}, err
	}
	return rec, nil
}

func (s *catalogPersistence) renameWorkspace(ctx context.Context, user authz.User, workspaceID, name string) (workspaceRecord, error) {
	name, err := normalizeWorkspaceName(name)
	if err != nil {
		return workspaceRecord{}, err
	}
	scope, err := s.authz.AuthorizeWorkspace(ctx, user, workspaceID, authz.Manage)
	if err != nil {
		return workspaceRecord{}, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE workspaces SET name = $1 WHERE id = $2`, name, scope.ID()); err != nil {
		return workspaceRecord{}, err
	}
	return s.workspaceByScopeQuerier(ctx, s.db, scope)
}

// lockWorkspaceTx serializes membership changes and deletion of one workspace.
func lockWorkspaceTx(ctx context.Context, tx *sql.Tx, workspaceID string) (name string, personal bool, err error) {
	err = tx.QueryRowContext(ctx,
		`SELECT name, personal_user_id IS NOT NULL FROM workspaces WHERE id = $1 FOR UPDATE`,
		workspaceID).Scan(&name, &personal)
	return name, personal, err
}

// deleteWorkspace removes a shared workspace without live projects. Its
// recently deleted projects are forfeited: their grace period ends now and
// they lose their workspace, so nobody can restore them and deletion GC
// destroys them on its next pass. Projects reference their workspace, so a
// concurrent project creation either commits first and blocks the delete or
// fails its foreign key afterwards.
func (s *catalogPersistence) deleteWorkspace(ctx context.Context, user authz.User, workspaceID, confirmation string) error {
	scope, err := s.authz.AuthorizeWorkspace(ctx, user, workspaceID, authz.Manage)
	if err != nil {
		return err
	}
	return s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		name, personal, err := lockWorkspaceTx(ctx, tx, scope.ID())
		if err != nil {
			return err
		}
		if personal {
			return errPersonalWorkspace
		}
		if err := deliverycore.CheckDeletionConfirmation(name, confirmation); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT id, deleted_at IS NOT NULL FROM projects WHERE workspace_id = $1 FOR UPDATE`, scope.ID())
		if err != nil {
			return err
		}
		var deletedProjects []string
		live := false
		for rows.Next() {
			var id string
			var deleted bool
			if err := rows.Scan(&id, &deleted); err != nil {
				rows.Close()
				return err
			}
			if deleted {
				deletedProjects = append(deletedProjects, id)
			} else {
				live = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if live {
			return errWorkspaceHasProjects
		}
		for _, id := range deletedProjects {
			if _, err := tx.ExecContext(ctx,
				`UPDATE projects
				    SET workspace_id = NULL,
				        delete_expires_at = LEAST(delete_expires_at, statement_timestamp())
				  WHERE id = $1`, id); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, scope.ID())
		return err
	})
}

func (s *catalogPersistence) listWorkspaceMembers(ctx context.Context, user authz.User, workspaceID string) ([]workspaceMemberRecord, []workspaceInvitationRecord, error) {
	scope, err := s.authz.AuthorizeWorkspace(ctx, user, workspaceID, authz.Read)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id, role, created_at FROM workspace_memberships
		  WHERE workspace_id = $1 ORDER BY created_at, user_id`, scope.ID())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var members []workspaceMemberRecord
	for rows.Next() {
		var rec workspaceMemberRecord
		var role string
		if err := rows.Scan(&rec.UserID, &role, &rec.JoinedAt); err != nil {
			return nil, nil, err
		}
		rec.Role = authz.Role(role)
		members = append(members, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	invitations, err := s.queryInvitations(ctx, `i.workspace_id = $1`, scope.ID())
	if err != nil {
		return nil, nil, err
	}
	return members, invitations, nil
}

func (s *catalogPersistence) queryInvitations(ctx context.Context, where string, arg string) ([]workspaceInvitationRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT i.id, i.workspace_id, w.name, i.email, i.role, i.invited_by_user_id, i.created_at
		   FROM workspace_invitations i
		   JOIN workspaces w ON w.id = i.workspace_id
		  WHERE `+where+`
		  ORDER BY i.created_at, i.id`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []workspaceInvitationRecord
	for rows.Next() {
		var rec workspaceInvitationRecord
		var role string
		if err := rows.Scan(&rec.ID, &rec.WorkspaceID, &rec.WorkspaceName, &rec.Email, &role, &rec.InvitedByUserID, &rec.CreatedAt); err != nil {
			return nil, err
		}
		rec.Role = authz.Role(role)
		out = append(out, rec)
	}
	return out, rows.Err()
}

// inviteWorkspaceMember addresses an invitation to email. Inviting the same
// address again replaces the pending invitation's role.
func (s *catalogPersistence) inviteWorkspaceMember(ctx context.Context, user authz.User, workspaceID, email string, role authz.Role) (workspaceInvitationRecord, error) {
	email, err := normalizeInvitationEmail(email)
	if err != nil {
		return workspaceInvitationRecord{}, err
	}
	scope, err := s.authz.AuthorizeWorkspace(ctx, user, workspaceID, authz.Manage)
	if err != nil {
		return workspaceInvitationRecord{}, err
	}
	rec := workspaceInvitationRecord{WorkspaceID: scope.ID(), Email: email, Role: role, InvitedByUserID: user.ID()}
	err = s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		name, personal, err := lockWorkspaceTx(ctx, tx, scope.ID())
		if err != nil {
			return err
		}
		if personal {
			return errPersonalWorkspace
		}
		rec.WorkspaceName = name
		return tx.QueryRowContext(ctx,
			`INSERT INTO workspace_invitations(id, workspace_id, email, role, invited_by_user_id, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (workspace_id, email) DO UPDATE
			   SET role = excluded.role, invited_by_user_id = excluded.invited_by_user_id, created_at = excluded.created_at
			 RETURNING id, created_at`,
			uuid.NewString(), rec.WorkspaceID, rec.Email, string(rec.Role), rec.InvitedByUserID, time.Now().UTC(),
		).Scan(&rec.ID, &rec.CreatedAt)
	})
	if err != nil {
		return workspaceInvitationRecord{}, err
	}
	return rec, nil
}

func (s *catalogPersistence) revokeWorkspaceInvitation(ctx context.Context, user authz.User, invitationID string) error {
	var workspaceID string
	if err := s.db.QueryRowContext(ctx, `SELECT workspace_id FROM workspace_invitations WHERE id = $1`, invitationID).Scan(&workspaceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errInvitationNotFound
		}
		return err
	}
	scope, err := s.authz.AuthorizeWorkspace(ctx, user, workspaceID, authz.Manage)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM workspace_invitations WHERE id = $1 AND workspace_id = $2`, invitationID, scope.ID())
	return err
}

// acceptWorkspaceInvitation joins the invitation's workspace when it is
// addressed to the caller's verified email. An existing membership keeps its role.
func (s *catalogPersistence) acceptWorkspaceInvitation(ctx context.Context, user authz.User, email, invitationID string) (workspaceRecord, error) {
	if email == "" {
		return workspaceRecord{}, errInvitationNotFound
	}
	var workspaceID string
	err := s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var role string
		if err := tx.QueryRowContext(ctx,
			`DELETE FROM workspace_invitations WHERE id = $1 AND email = $2 RETURNING workspace_id, role`,
			invitationID, email).Scan(&workspaceID, &role); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errInvitationNotFound
			}
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO workspace_memberships(workspace_id, user_id, role, created_at) VALUES ($1, $2, $3, $4)
			 ON CONFLICT (workspace_id, user_id) DO NOTHING`,
			workspaceID, user.ID(), role, time.Now().UTC())
		return err
	})
	if err != nil {
		return workspaceRecord{}, err
	}
	return s.workspaceByID(ctx, user, workspaceID)
}

func (s *catalogPersistence) declineWorkspaceInvitation(ctx context.Context, user authz.User, email, invitationID string) error {
	if email == "" {
		return errInvitationNotFound
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM workspace_invitations WHERE id = $1 AND email = $2`, invitationID, email)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return errInvitationNotFound
	}
	return nil
}

// changeMembershipTx applies change to one membership while holding the
// workspace lock, refusing any change that leaves the workspace without an admin.
func changeMembershipTx(ctx context.Context, tx *sql.Tx, workspaceID, memberUserID string, next *authz.Role) (workspaceMemberRecord, error) {
	if _, _, err := lockWorkspaceTx(ctx, tx, workspaceID); err != nil {
		return workspaceMemberRecord{}, err
	}
	rec := workspaceMemberRecord{UserID: memberUserID}
	var current string
	if err := tx.QueryRowContext(ctx,
		`SELECT role, created_at FROM workspace_memberships WHERE workspace_id = $1 AND user_id = $2`,
		workspaceID, memberUserID).Scan(&current, &rec.JoinedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return workspaceMemberRecord{}, errWorkspaceMemberMissing
		}
		return workspaceMemberRecord{}, err
	}
	if authz.Role(current) == authz.RoleAdmin && (next == nil || *next != authz.RoleAdmin) {
		var admins int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM workspace_memberships WHERE workspace_id = $1 AND role = $2`,
			workspaceID, string(authz.RoleAdmin)).Scan(&admins); err != nil {
			return workspaceMemberRecord{}, err
		}
		if admins <= 1 {
			return workspaceMemberRecord{}, errLastWorkspaceAdmin
		}
	}
	if next == nil {
		_, err := tx.ExecContext(ctx, `DELETE FROM workspace_memberships WHERE workspace_id = $1 AND user_id = $2`, workspaceID, memberUserID)
		return rec, err
	}
	rec.Role = *next
	_, err := tx.ExecContext(ctx, `UPDATE workspace_memberships SET role = $1 WHERE workspace_id = $2 AND user_id = $3`, string(*next), workspaceID, memberUserID)
	return rec, err
}

func (s *catalogPersistence) updateWorkspaceMemberRole(ctx context.Context, user authz.User, workspaceID, memberUserID string, role authz.Role) (workspaceMemberRecord, error) {
	scope, err := s.authz.AuthorizeWorkspace(ctx, user, workspaceID, authz.Manage)
	if err != nil {
		return workspaceMemberRecord{}, err
	}
	var rec workspaceMemberRecord
	err = s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var changeErr error
		rec, changeErr = changeMembershipTx(ctx, tx, scope.ID(), memberUserID, &role)
		return changeErr
	})
	return rec, err
}

// removeWorkspaceMember removes a member. Admins remove anyone; every member
// may remove themselves to leave.
func (s *catalogPersistence) removeWorkspaceMember(ctx context.Context, user authz.User, workspaceID, memberUserID string) error {
	need := authz.Manage
	if memberUserID == user.ID() {
		need = authz.Read
	}
	scope, err := s.authz.AuthorizeWorkspace(ctx, user, workspaceID, need)
	if err != nil {
		return err
	}
	return s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := changeMembershipTx(ctx, tx, scope.ID(), memberUserID, nil)
		return err
	})
}

// moveProject transfers a project to another workspace. The caller must
// administer the source workspace and be able to create projects in the target.
func (s *catalogPersistence) moveProject(ctx context.Context, user authz.User, projectID, workspaceID string) (deliverycore.ProjectRecord, error) {
	project, err := s.projectByID(ctx, user, projectID)
	if err != nil {
		return deliverycore.ProjectRecord{}, err
	}
	if project.Deletion != nil {
		return deliverycore.ProjectRecord{}, deliverycore.ErrProjectDeleted
	}
	if _, err := s.authz.AuthorizeWorkspace(ctx, user, project.WorkspaceID, authz.Manage); err != nil {
		return deliverycore.ProjectRecord{}, err
	}
	target, err := s.authz.AuthorizeWorkspace(ctx, user, workspaceID, authz.Write)
	if err != nil {
		return deliverycore.ProjectRecord{}, err
	}
	if project.WorkspaceID == target.ID() {
		return project, nil
	}
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, found, err := s.projectByWorkspaceNameQuerier(ctx, tx, target.ID(), project.Name); err != nil {
			return err
		} else if found {
			return fmt.Errorf("%w: the workspace already has a project named %q", errProjectNameTaken, project.Name)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE projects SET workspace_id = $1 WHERE id = $2 AND workspace_id = $3`,
			target.ID(), project.ID, project.WorkspaceID); err != nil {
			return err
		}
		moved, err := s.projectByIDInternalQuerier(ctx, tx, project.ID)
		project = moved
		return err
	})
	if err != nil {
		return deliverycore.ProjectRecord{}, err
	}
	return project, nil
}

var errProjectNameTaken = errors.New("project name is taken")
