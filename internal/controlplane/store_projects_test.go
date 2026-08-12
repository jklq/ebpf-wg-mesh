//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"testing"

	"ebof-wg-mesh/internal/config"
)

func TestListProjectsExcludesManagedProjects(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{
			ID:       "user-1",
			Email:    "user@example.com",
			Projects: []string{"demo"},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	managed, err := store.ensureManagedProject(ctx, "Platform Dashboard", "dashboard")
	if err != nil {
		t.Fatalf("ensureManagedProject: %v", err)
	}
	if managed.Kind != projectKindManaged {
		t.Fatalf("expected managed project kind, got %s", managed.Kind)
	}

	projects, err := store.listProjects(ctx, "user-1")
	if err != nil {
		t.Fatalf("listProjects: %v", err)
	}
	if len(projects) != 1 || projects[0].Name != "demo" {
		t.Fatalf("unexpected projects: %+v", projects)
	}
}

func TestProjectNamesAreScopedByUserID(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{
			{
				ID:       "user-1",
				Email:    "user1@example.com",
				Projects: []string{"demo"},
			},
			{
				ID:       "user-2",
				Email:    "user2@example.com",
				Projects: []string{"demo"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	firstProjects, err := store.listProjects(ctx, "user-1")
	if err != nil {
		t.Fatalf("listProjects(user-1): %v", err)
	}
	secondProjects, err := store.listProjects(ctx, "user-2")
	if err != nil {
		t.Fatalf("listProjects(user-2): %v", err)
	}
	if len(firstProjects) != 1 || len(secondProjects) != 1 {
		t.Fatalf("unexpected project lists: user-1=%+v user-2=%+v", firstProjects, secondProjects)
	}
	if firstProjects[0].ID == secondProjects[0].ID {
		t.Fatalf("expected distinct projects for each user ID, got shared id %q", firstProjects[0].ID)
	}
	if firstProjects[0].Name != "demo" || secondProjects[0].Name != "demo" {
		t.Fatalf("unexpected project names: user-1=%q user-2=%q", firstProjects[0].Name, secondProjects[0].Name)
	}
	assertProjectOwnerInvariant(t, store, firstProjects[0].ID, "user-1")
	assertProjectOwnerInvariant(t, store, secondProjects[0].ID, "user-2")
	if _, err := store.projectByID(ctx, "user-1", secondProjects[0].ID); err == nil {
		t.Fatalf("expected user-1 to be denied access to user-2 project %q", secondProjects[0].ID)
	}
}

func TestCreateProjectRepairsOwnerMembership(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{
			ID:       "user-1",
			Email:    "user1@example.com",
			Projects: []string{"demo"},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	projects, err := store.listProjects(ctx, "user-1")
	if err != nil {
		t.Fatalf("listProjects(user-1): %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("unexpected projects: %+v", projects)
	}

	projectID := projects[0].ID
	if _, err := store.db.ExecContext(
		ctx,
		`UPDATE project_memberships SET role = $1 WHERE user_id = $2 AND project_id = $3`,
		"viewer",
		"user-1",
		projectID,
	); err != nil {
		t.Fatalf("downgrade owner membership: %v", err)
	}
	if err := store.authorizeProjectWrite(ctx, "user-1", projectID); err != sql.ErrNoRows {
		t.Fatalf("expected viewer write denial, got %v", err)
	}

	project, err := store.createProject(ctx, "user-1", "demo")
	if err != nil {
		t.Fatalf("createProject: %v", err)
	}
	if project.ID != projectID {
		t.Fatalf("expected repaired project %q, got %q", projectID, project.ID)
	}
	assertProjectOwnerInvariant(t, store, projectID, "user-1")
}

func assertProjectOwnerInvariant(t *testing.T, store *Store, projectID, userID string) {
	t.Helper()

	var ownerUserID string
	var role string
	if err := store.db.QueryRowContext(
		context.Background(),
		`SELECT p.owner_user_id, m.role
		   FROM projects p
		   JOIN project_memberships m ON m.project_id = p.id AND m.user_id = $2
		  WHERE p.id = $1`,
		projectID,
		userID,
	).Scan(&ownerUserID, &role); err != nil {
		t.Fatalf("load project ownership invariant for %q/%q: %v", projectID, userID, err)
	}
	if ownerUserID != userID {
		t.Fatalf("expected project %q owner user ID %q, got %q", projectID, userID, ownerUserID)
	}
	if role != "owner" {
		t.Fatalf("expected project %q membership role owner for %q, got %q", projectID, userID, role)
	}
}
