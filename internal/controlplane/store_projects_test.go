//go:build integration

package controlplane

import (
	"context"
	"testing"

	"ebof-wg-mesh/internal/config"
)

func TestListProjectsExcludesManagedProjects(t *testing.T) {
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

func TestEnsurePrincipalIsIdempotent(t *testing.T) {
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
