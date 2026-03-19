//go:build integration

package controlplane

import (
	"context"
	"testing"

	"ebof-wg-mesh/internal/config"
)

func TestListProjectsExcludesManagedProjects(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{
			Subject:  "user-1",
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

func TestProjectNamesAreScopedBySubject(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{
			{
				Subject:  "user-1",
				Email:    "user1@example.com",
				Projects: []string{"demo"},
			},
			{
				Subject:  "user-2",
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
		t.Fatalf("expected distinct projects for each subject, got shared id %q", firstProjects[0].ID)
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
			Subject:  "user-1",
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
		`UPDATE project_memberships SET role = $1 WHERE subject = $2 AND project_id = $3`,
		"viewer",
		"user-1",
		projectID,
	); err != nil {
		t.Fatalf("downgrade owner membership: %v", err)
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

func TestEnsurePrincipalIsIdempotent(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	first, err := store.ensurePrincipal(ctx, "user-1", "user@example.com")
	if err != nil {
		t.Fatalf("ensurePrincipal first: %v", err)
	}
	second, err := store.ensurePrincipal(ctx, "user-1", "user@example.com")
	if err != nil {
		t.Fatalf("ensurePrincipal second: %v", err)
	}
	if first.Subject != second.Subject || first.Email != second.Email {
		t.Fatalf("unexpected principal mismatch: %+v vs %+v", first, second)
	}
}

func assertProjectOwnerInvariant(t *testing.T, store *Store, projectID, subject string) {
	t.Helper()

	var ownerSubject string
	var role string
	if err := store.db.QueryRowContext(
		context.Background(),
		`SELECT p.owner_subject, m.role
		   FROM projects p
		   JOIN project_memberships m ON m.project_id = p.id AND m.subject = $2
		  WHERE p.id = $1`,
		projectID,
		subject,
	).Scan(&ownerSubject, &role); err != nil {
		t.Fatalf("load project ownership invariant for %q/%q: %v", projectID, subject, err)
	}
	if ownerSubject != subject {
		t.Fatalf("expected project %q owner_subject %q, got %q", projectID, subject, ownerSubject)
	}
	if role != "owner" {
		t.Fatalf("expected project %q membership role owner for %q, got %q", projectID, subject, role)
	}
}
