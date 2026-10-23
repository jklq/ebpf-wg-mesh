//go:build integration

package controlplane

import (
	"context"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

// TestServiceEnvReadsRedactValuesByDefault pins the secret-read split: plain
// service reads keep variable names but never values, for every role, while
// the system snapshot that feeds builds and agents keeps the real env.
func TestServiceEnvReadsRedactValuesByDefault(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "", "env")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "owner", environmentID, "web",
		envServiceSpec("a", map[string]string{"TOKEN": "first-secret"}), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	// Stage an env change so the read also carries unapplied-change diffs.
	if _, _, err := updateService(ctx, store, "owner", service.ID, service.Name,
		envServiceSpec("a", map[string]string{"TOKEN": "second-secret"})); err != nil {
		t.Fatal(err)
	}
	grantWorkspaceRole(t, store, ctx, project.ID, "viewer-user", "viewer")

	assertRedacted := func(t *testing.T, env map[string]string, changes []*platformv1.ServiceUnappliedChange) {
		t.Helper()
		if len(env) != 1 {
			t.Fatalf("env names = %v, want just TOKEN", redactEnvForTest(env))
		}
		if got, ok := env["TOKEN"]; !ok || got != "" {
			t.Fatalf("env TOKEN = %q (present %v), want redacted empty value", got, ok)
		}
		for _, change := range changes {
			if change.GetId() != "runtime.env.TOKEN" {
				continue
			}
			if change.GetCurrentValue() != "" || change.GetNewValue() != "" {
				t.Fatalf("unapplied change carries values: current %q new %q",
					change.GetCurrentValue(), change.GetNewValue())
			}
			if change.GetAction() == platformv1.ServiceUnappliedChangeAction_SERVICE_UNAPPLIED_CHANGE_ACTION_UNSPECIFIED {
				t.Fatal("unapplied env change lost its action under redaction")
			}
			return
		}
		t.Fatal("unapplied changes missing runtime.env.TOKEN")
	}

	for _, userID := range []string{"owner", "viewer-user"} {
		read, err := store.reads.ServiceByID(ctx, testUser(userID), service.ID)
		if err != nil {
			t.Fatalf("ServiceByID(%s): %v", userID, err)
		}
		assertRedacted(t, read.Spec.GetRuntime().GetEnv(), read.UnappliedChanges)

		listed, err := store.reads.ListServices(ctx, testUser(userID), environmentID, false)
		if err != nil {
			t.Fatalf("ListServices(%s): %v", userID, err)
		}
		if len(listed) != 1 {
			t.Fatalf("ListServices(%s) = %d services", userID, len(listed))
		}
		assertRedacted(t, listed[0].Spec.GetRuntime().GetEnv(), listed[0].UnappliedChanges)

		status, _, err := store.reads.ServiceStatus(ctx, testUser(userID), service.ID)
		if err != nil {
			t.Fatalf("ServiceStatus(%s): %v", userID, err)
		}
		assertRedacted(t, status.Spec.GetRuntime().GetEnv(), status.UnappliedChanges)
	}

	revealed, err := store.reads.ServiceByIDWithSecrets(ctx, testUser("owner"), service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := revealed.Spec.GetRuntime().GetEnv()["TOKEN"]; got != "second-secret" {
		t.Fatalf("revealed TOKEN = %q, want the staged value", got)
	}
	if _, err := store.reads.ServiceByIDWithSecrets(ctx, testUser("viewer-user"), service.ID); err == nil {
		t.Fatal("viewer revealed secret values")
	}

	// The system snapshot is not a user read: builds and agents need values.
	snapshot, err := store.reads.ServiceSnapshot(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Spec.GetRuntime().GetEnv()["TOKEN"]; got != "second-secret" {
		t.Fatalf("snapshot TOKEN = %q, want the staged value", got)
	}
}
