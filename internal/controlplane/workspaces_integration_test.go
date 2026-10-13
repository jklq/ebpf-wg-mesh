//go:build integration

package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"ebof-wg-mesh/internal/controlplane/authz"
)

func TestPersonalWorkspaceHoldsDefaultProjects(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	owner := testUser("owner")

	project, err := store.catalog.createProject(ctx, owner, "", "demo")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := store.catalog.createWorkspace(ctx, owner, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	workspaces, _, err := store.catalog.listWorkspaces(ctx, owner, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 2 || !workspaces[0].Personal || workspaces[0].Name != personalWorkspaceName || workspaces[1].ID != shared.ID {
		t.Fatalf("workspaces = %+v, want personal first then Acme", workspaces)
	}
	if project.WorkspaceID != workspaces[0].ID {
		t.Fatalf("project workspace = %q, want personal %q", project.WorkspaceID, workspaces[0].ID)
	}
	if _, err := store.catalog.inviteWorkspaceMember(ctx, owner, workspaces[0].ID, "friend@example.com", authz.RoleMember); !errors.Is(err, errPersonalWorkspace) {
		t.Fatalf("invite to personal workspace = %v, want errPersonalWorkspace", err)
	}
	if err := store.catalog.deleteWorkspace(ctx, owner, workspaces[0].ID, personalWorkspaceName); !errors.Is(err, errPersonalWorkspace) {
		t.Fatalf("delete personal workspace = %v, want errPersonalWorkspace", err)
	}
	if err := store.catalog.removeWorkspaceMember(ctx, owner, workspaces[0].ID, owner.ID()); !errors.Is(err, errLastWorkspaceAdmin) {
		t.Fatalf("leave personal workspace = %v, want errLastWorkspaceAdmin", err)
	}
}

func TestWorkspaceInvitationGrantsProjectAccess(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	admin, guest, stranger := testUser("admin"), testUser("guest"), testUser("stranger")

	workspace, err := store.catalog.createWorkspace(ctx, admin, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.catalog.createProject(ctx, admin, workspace.ID, "api")
	if err != nil {
		t.Fatal(err)
	}
	invitation, err := store.catalog.inviteWorkspaceMember(ctx, admin, workspace.ID, " Guest@Example.com ", authz.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if invitation.Email != "guest@example.com" {
		t.Fatalf("invitation email = %q, want normalized", invitation.Email)
	}
	if _, err := store.catalog.projectByID(ctx, guest, project.ID); err == nil {
		t.Fatal("pending invitation granted project access")
	}
	if _, err := store.catalog.acceptWorkspaceInvitation(ctx, stranger, "stranger@example.com", invitation.ID); !errors.Is(err, errInvitationNotFound) {
		t.Fatalf("accept by other email = %v, want errInvitationNotFound", err)
	}
	_, invitations, err := store.catalog.listWorkspaces(ctx, guest, "guest@example.com")
	if err != nil || len(invitations) != 1 || invitations[0].WorkspaceName != "Acme" {
		t.Fatalf("guest invitations = %+v, %v", invitations, err)
	}
	joined, err := store.catalog.acceptWorkspaceInvitation(ctx, guest, "guest@example.com", invitation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if joined.ID != workspace.ID || joined.Role != authz.RoleViewer {
		t.Fatalf("joined = %+v", joined)
	}
	if _, err := store.catalog.projectByID(ctx, guest, project.ID); err != nil {
		t.Fatalf("viewer read: %v", err)
	}
	if _, err := store.catalog.createEnvironment(ctx, guest, project.ID, "staging"); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("viewer write = %v, want ErrDenied", err)
	}
	if _, err := store.catalog.updateWorkspaceMemberRole(ctx, guest, workspace.ID, guest.ID(), authz.RoleAdmin); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("viewer self-promotion = %v, want ErrDenied", err)
	}
	if _, err := store.catalog.updateWorkspaceMemberRole(ctx, admin, workspace.ID, guest.ID(), authz.RoleMember); err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.createEnvironment(ctx, guest, project.ID, "staging"); err != nil {
		t.Fatalf("member write: %v", err)
	}
	if err := store.catalog.removeWorkspaceMember(ctx, guest, workspace.ID, guest.ID()); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if _, err := store.catalog.projectByID(ctx, guest, project.ID); err == nil {
		t.Fatal("former member kept project access")
	}
}

func TestWorkspaceKeepsAnAdmin(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	first, second := testUser("first"), testUser("second")

	workspace, err := store.catalog.createWorkspace(ctx, first, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	invitation, err := store.catalog.inviteWorkspaceMember(ctx, first, workspace.ID, "second@example.com", authz.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.acceptWorkspaceInvitation(ctx, second, "second@example.com", invitation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.updateWorkspaceMemberRole(ctx, second, workspace.ID, first.ID(), authz.RoleMember); err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.updateWorkspaceMemberRole(ctx, second, workspace.ID, second.ID(), authz.RoleViewer); !errors.Is(err, errLastWorkspaceAdmin) {
		t.Fatalf("demote last admin = %v, want errLastWorkspaceAdmin", err)
	}
	if err := store.catalog.removeWorkspaceMember(ctx, second, workspace.ID, second.ID()); !errors.Is(err, errLastWorkspaceAdmin) {
		t.Fatalf("last admin leaves = %v, want errLastWorkspaceAdmin", err)
	}
}

func TestMoveProjectBetweenWorkspaces(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	owner := testUser("owner")

	project, err := store.catalog.createProject(ctx, owner, "", "api")
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.catalog.createWorkspace(ctx, owner, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.createProject(ctx, owner, target.ID, "api"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.moveProject(ctx, owner, project.ID, target.ID); !errors.Is(err, errProjectNameTaken) {
		t.Fatalf("move onto taken name = %v, want errProjectNameTaken", err)
	}
	if err := store.catalog.deleteWorkspace(ctx, owner, target.ID, "Acme"); !errors.Is(err, errWorkspaceHasProjects) {
		t.Fatalf("delete non-empty workspace = %v, want errWorkspaceHasProjects", err)
	}

	other, err := store.catalog.createWorkspace(ctx, owner, "Other")
	if err != nil {
		t.Fatal(err)
	}
	moved, err := store.catalog.moveProject(ctx, owner, project.ID, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if moved.WorkspaceID != other.ID {
		t.Fatalf("moved workspace = %q, want %q", moved.WorkspaceID, other.ID)
	}
	listed, err := store.catalog.listProjects(ctx, owner, other.ID, false)
	if err != nil || len(listed) != 1 || listed[0].ID != project.ID {
		t.Fatalf("projects in target = %+v, %v", listed, err)
	}
	if _, err := store.catalog.moveProject(ctx, owner, project.ID, target.ID+"-missing"); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("move to foreign workspace = %v, want ErrDenied", err)
	}
}

func TestDeleteWorkspaceForfeitsRecentlyDeletedProjects(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	owner := testUser("owner")

	workspace, err := store.catalog.createWorkspace(ctx, owner, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.catalog.createProject(ctx, owner, workspace.ID, "api")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.catalog.deleteWorkspace(ctx, owner, workspace.ID, "Acme"); !errors.Is(err, errWorkspaceHasProjects) {
		t.Fatalf("delete with a live project = %v, want errWorkspaceHasProjects", err)
	}
	if _, err := store.catalog.deleteProject(ctx, owner, project.ID, "api"); err != nil {
		t.Fatal(err)
	}
	if err := store.catalog.deleteWorkspace(ctx, owner, workspace.ID, "Acme"); err != nil {
		t.Fatalf("delete with only a recently deleted project: %v", err)
	}
	if _, err := store.catalog.restoreProject(ctx, owner, project.ID); err == nil {
		t.Fatal("restored a project whose workspace was deleted")
	}

	stats, err := newDeletionGC(store, nil, nil, time.Minute).CollectOnce(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if stats.ByKind[expiredDeletionProject] != 1 {
		t.Fatalf("gc collected %+v, want the forfeited project", stats.ByKind)
	}
	var remaining int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM projects WHERE id = $1`, project.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatal("forfeited project survived deletion gc")
	}
}
