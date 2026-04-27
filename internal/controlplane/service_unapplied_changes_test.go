package controlplane

import (
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestServiceUnappliedChangesRuntimeEnvStableIDs(t *testing.T) {
	deployed := directImageServiceSpec("repo/app:v1", &platformv1.ServiceRuntime{
		Env: map[string]string{"KEEP": "same", "REMOVE": "old", "UPDATE": "old"},
	})
	current := directImageServiceSpec("repo/app:v1", &platformv1.ServiceRuntime{
		Env: map[string]string{"ADD": "new", "KEEP": "same", "UPDATE": "new"},
	})

	changes := diffServiceUnappliedChanges(current, deployed)
	if got, want := len(changes), 3; got != want {
		t.Fatalf("change count = %d, want %d: %#v", got, want, changes)
	}
	assertChange(t, changes[0], "runtime.env.ADD", platformv1.ServiceUnappliedChangeAction_SERVICE_UNAPPLIED_CHANGE_ACTION_ADD, "", "new")
	assertChange(t, changes[1], "runtime.env.REMOVE", platformv1.ServiceUnappliedChangeAction_SERVICE_UNAPPLIED_CHANGE_ACTION_REMOVE, "old", "")
	assertChange(t, changes[2], "runtime.env.UPDATE", platformv1.ServiceUnappliedChangeAction_SERVICE_UNAPPLIED_CHANGE_ACTION_UPDATE, "old", "new")
}

func TestServiceUnappliedChangesSemanticSourceEquality(t *testing.T) {
	deployed := repositoryServiceSpec(nil, &platformv1.ServiceSourceSpec{
		Provider:           "github",
		RepositorySelector: "OWNER/Repo",
		BuildRecipe:        &platformv1.BuildRecipe{},
	})
	current := repositoryServiceSpec(nil, &platformv1.ServiceSourceSpec{
		Provider:           "github",
		RepositorySelector: "owner/repo",
		TrackedRef:         "main",
		BuildRecipe: &platformv1.BuildRecipe{
			DockerfilePath: "Dockerfile",
			ContextDir:     ".",
		},
	})

	if changes := diffServiceUnappliedChanges(current, deployed); len(changes) != 0 {
		t.Fatalf("changes = %#v, want none", changes)
	}
}

func assertChange(t *testing.T, change *platformv1.ServiceUnappliedChange, id string, action platformv1.ServiceUnappliedChangeAction, current, next string) {
	t.Helper()
	if change.GetId() != id {
		t.Fatalf("id = %q, want %q", change.GetId(), id)
	}
	if change.GetAction() != action {
		t.Fatalf("%s action = %v, want %v", id, change.GetAction(), action)
	}
	if change.GetCurrentValue() != current || change.GetNewValue() != next {
		t.Fatalf("%s values = (%q, %q), want (%q, %q)", id, change.GetCurrentValue(), change.GetNewValue(), current, next)
	}
}
