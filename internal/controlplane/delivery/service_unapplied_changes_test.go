package delivery

import (
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/proto"
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

func TestServiceUnappliedChangesIncludesHTTPReadinessCheck(t *testing.T) {
	deployed := directImageServiceSpec("repo/app:v1", &platformv1.ServiceRuntime{})
	current := directImageServiceSpec("repo/app:v1", &platformv1.ServiceRuntime{
		HealthCheck: &platformv1.HealthCheck{
			Type:           platformv1.HealthCheck_TYPE_HTTP,
			Path:           "/ready",
			Port:           8080,
			TimeoutSeconds: 3,
		},
	})

	changes := diffServiceUnappliedChanges(current, deployed)
	if got, want := len(changes), 1; got != want {
		t.Fatalf("change count = %d, want %d: %#v", got, want, changes)
	}
	assertChange(t, changes[0], "runtime.healthCheck", platformv1.ServiceUnappliedChangeAction_SERVICE_UNAPPLIED_CHANGE_ACTION_ADD, "", "GET /ready on port 8080, timeout 3s")

	discarded := applyDiscardedServiceChanges(current, deployed, false, []string{"runtime.healthCheck"})
	if discarded.GetRuntime().GetHealthCheck() != nil {
		t.Fatalf("discarded health check = %+v, want nil", discarded.GetRuntime().GetHealthCheck())
	}
}

func TestServiceUnappliedChangesIncludesReplicaCount(t *testing.T) {
	deployed := directImageServiceSpec("repo/app:v1", &platformv1.ServiceRuntime{})
	deployed.DesiredReplicaCount = replicaCountPtr(1)
	current := proto.Clone(deployed).(*platformv1.ServiceSpec)
	current.DesiredReplicaCount = replicaCountPtr(3)

	changes := diffServiceUnappliedChanges(current, deployed)
	if got, want := len(changes), 1; got != want {
		t.Fatalf("change count = %d, want %d: %#v", got, want, changes)
	}
	assertChange(t, changes[0], "desiredReplicaCount", platformv1.ServiceUnappliedChangeAction_SERVICE_UNAPPLIED_CHANGE_ACTION_UPDATE, "1", "3")

	discarded := applyDiscardedServiceChanges(current, deployed, false, []string{"desiredReplicaCount"})
	if discarded.GetDesiredReplicaCount() != 1 {
		t.Fatalf("discarded replica count = %d, want 1", discarded.GetDesiredReplicaCount())
	}
}

func TestServiceUnappliedChangesIncludesPlacementRegion(t *testing.T) {
	deployed := directImageServiceSpec("repo/app:v1", &platformv1.ServiceRuntime{})
	current := proto.Clone(deployed).(*platformv1.ServiceSpec)
	current.PlacementRegion = "eu-west"

	changes := diffServiceUnappliedChanges(current, deployed)
	if got, want := len(changes), 1; got != want {
		t.Fatalf("change count = %d, want %d: %#v", got, want, changes)
	}
	assertChange(t, changes[0], "placementRegion", platformv1.ServiceUnappliedChangeAction_SERVICE_UNAPPLIED_CHANGE_ACTION_ADD, "", "eu-west")

	discarded := applyDiscardedServiceChanges(current, deployed, false, []string{"placementRegion"})
	if discarded.GetPlacementRegion() != "" {
		t.Fatalf("discarded placement region = %q, want empty", discarded.GetPlacementRegion())
	}
}

func TestServiceUnappliedChangesIncludesRollingStrategy(t *testing.T) {
	deployed := directImageServiceSpec("repo/app:v1", &platformv1.ServiceRuntime{})
	current := proto.Clone(deployed).(*platformv1.ServiceSpec)
	current.RollingStrategy = &platformv1.RollingStrategy{
		HealthcheckTimeoutSeconds: proto.Int32(60),
		DrainingSeconds:           proto.Int32(5),
	}

	changes := diffServiceUnappliedChanges(current, deployed)
	if got, want := len(changes), 1; got != want {
		t.Fatalf("change count = %d, want %d: %#v", got, want, changes)
	}
	assertChange(t, changes[0], "rollingStrategy", platformv1.ServiceUnappliedChangeAction_SERVICE_UNAPPLIED_CHANGE_ACTION_UPDATE,
		"healthcheck timeout 300s, draining time 0s",
		"healthcheck timeout 60s, draining time 5s")

	discarded := applyDiscardedServiceChanges(current, deployed, false, []string{"rollingStrategy"})
	got := canonicalRollingStrategy(discarded.GetRollingStrategy())
	if got.GetHealthcheckTimeoutSeconds() != 300 || got.GetDrainingSeconds() != 0 {
		t.Fatalf("discarded rolling strategy = %+v", got)
	}
}

func TestServiceSpecsCompareHTTPReadinessConfiguration(t *testing.T) {
	left := directImageServiceSpec("repo/app:v1", &platformv1.ServiceRuntime{
		HealthCheck: &platformv1.HealthCheck{Type: platformv1.HealthCheck_TYPE_HTTP, Path: "/ready"},
	})
	right := directImageServiceSpec("repo/app:v1", &platformv1.ServiceRuntime{
		HealthCheck: &platformv1.HealthCheck{Type: platformv1.HealthCheck_TYPE_HTTP, Path: "/healthz"},
	})
	if sameServiceSpec(left, right) {
		t.Fatal("service specs with different readiness paths compared equal")
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
