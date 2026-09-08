//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"errors"
	"slices"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestEnvironmentLifecycleAndAuthorization(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()

	project, err := store.catalog.createProject(ctx, "owner", "demo")
	if err != nil {
		t.Fatal(err)
	}
	production, err := store.catalog.productionEnvironmentByProjectInternal(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.deleteEnvironment(ctx, "owner", production.ID); !errors.Is(err, errProductionEnvironment) {
		t.Fatalf("delete production: %v", err)
	}
	staging, err := store.catalog.createEnvironment(ctx, "owner", project.ID, "Staging")
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := store.catalog.renameEnvironment(ctx, "owner", staging.ID, "QA")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ID != staging.ID || renamed.NetworkIdentity != staging.NetworkIdentity {
		t.Fatalf("rename changed environment identity: before=%#v after=%#v", staging, renamed)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO project_memberships(user_id, project_id, role)
		VALUES ('editor', $1, 'editor'), ('viewer', $1, 'viewer')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.createEnvironment(ctx, "editor", project.ID, "Editor Sandbox"); err != nil {
		t.Fatalf("editor could not create environment: %v", err)
	}
	if _, err := store.catalog.createEnvironment(ctx, "viewer", project.ID, "Viewer Sandbox"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("viewer mutated project environments: %v", err)
	}
	if environments, err := store.catalog.listEnvironments(ctx, "viewer", project.ID); err != nil || len(environments) != 3 {
		t.Fatalf("viewer could not read project environments: %#v: %v", environments, err)
	}

	_, err = store.catalog.createProject(ctx, "other", "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.reads.deliveryQueries().EnvironmentByID(ctx, "other", staging.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-project environment access: %v", err)
	}
	if _, err := store.createStagedServiceForTest(ctx, "owner", staging.ID, "web", directImageServiceSpec("example.test/web:1", nil)); err != nil {
		t.Fatal(err)
	}
	services, err := store.reads.listServices(ctx, "owner", staging.ID)
	if err != nil || len(services) != 1 {
		t.Fatalf("list staging services: %#v: %v", services, err)
	}
	updated, _, err := updateService(ctx, store, "editor", services[0].ID, "web-editor", services[0].Spec)
	if err != nil || updated.Name != "web-editor" {
		t.Fatalf("editor could not mutate environment service: %#v: %v", updated, err)
	}
	services[0] = updated
	if _, err := store.reads.serviceByID(ctx, "other", services[0].ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("service ID bypassed ancestry authorization: %v", err)
	}
	if _, _, err := updateService(ctx, store, "viewer", services[0].ID, "web-viewer", services[0].Spec); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("viewer mutated a service: %v", err)
	}
}

func TestDuplicateEnvironmentCopiesConfigurationButNoRuntimeState(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, "owner", "demo")
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.catalog.productionEnvironmentByProjectInternal(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	volume, err := store.catalog.createVolume(ctx, "owner", source.ID, "data", 64<<20, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "owner", source.ID, "web", directImageServiceSpec("example.test/web:1", &platformv1.ServiceRuntime{
		Env: map[string]string{"SECRET": "production"}, VolumeName: "data",
	}), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.routing.createPlatformDomainBinding(ctx, "owner", "web.example.test", service.ID, 8080); err != nil {
		t.Fatal(err)
	}

	duplicate, err := store.reads.duplicateEnvironment(ctx, "owner", source.ID, "Staging", false)
	if err != nil {
		t.Fatal(err)
	}
	volumes, err := store.catalog.listVolumes(ctx, "owner", duplicate.ID)
	if err != nil || len(volumes) != 1 || volumes[0].ID == volume.ID {
		t.Fatalf("duplicated volumes: %#v: %v", volumes, err)
	}
	services, err := store.reads.listServices(ctx, "owner", duplicate.ID)
	if err != nil || len(services) != 1 {
		t.Fatalf("duplicated services: %#v: %v", services, err)
	}
	copy := services[0]
	if copy.ID == service.ID || copy.RolloutGeneration != 0 || copy.AllocatedAgentID != "" || copy.ResolvedImage != "" {
		t.Fatalf("duplicate inherited runtime state: %#v", copy)
	}
	if len(copy.Spec.GetRuntime().GetEnv()) != 0 || copy.Spec.GetRuntime().GetVolumeName() != "data" {
		t.Fatalf("unexpected copied specification: %#v", copy.Spec)
	}
	var allocations, domains, builds int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM allocations a JOIN services s ON s.id = a.service_id WHERE s.environment_id = $1`, duplicate.ID).Scan(&allocations); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM domain_bindings d JOIN services s ON s.id = d.service_id WHERE s.environment_id = $1`, duplicate.ID).Scan(&domains); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM build_runs b JOIN services s ON s.id = b.service_id WHERE s.environment_id = $1`, duplicate.ID).Scan(&builds); err != nil {
		t.Fatal(err)
	}
	if allocations != 0 || domains != 0 || builds != 0 {
		t.Fatalf("duplicate copied runtime records: allocations=%d domains=%d builds=%d", allocations, domains, builds)
	}

	withVariables, err := store.reads.duplicateEnvironment(ctx, "owner", source.ID, "Credentials Review", true)
	if err != nil {
		t.Fatal(err)
	}
	variableCopies, err := store.reads.listServices(ctx, "owner", withVariables.ID)
	if err != nil || len(variableCopies) != 1 || variableCopies[0].Spec.GetRuntime().GetEnv()["SECRET"] != "production" {
		t.Fatalf("explicit variable copy failed: %#v: %v", variableCopies, err)
	}
}

func TestEnvironmentReleaseAndDeleteAreScoped(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, "owner", "demo")
	if err != nil {
		t.Fatal(err)
	}
	production, err := store.catalog.productionEnvironmentByProjectInternal(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	staging, err := store.catalog.createEnvironment(ctx, "owner", project.ID, "Staging")
	if err != nil {
		t.Fatal(err)
	}
	for _, agentID := range []string{"node-1", "node-2"} {
		if _, err := upsertTestAgent(t, store, ctx, agentHello(agentID)); err != nil {
			t.Fatal(err)
		}
	}
	productionService, err := createScheduledService(ctx, store, "owner", production.ID, "web", directImageServiceSpec("example.test/web:production", nil))
	if err != nil {
		t.Fatal(err)
	}
	stagingService, err := createScheduledService(ctx, store, "owner", staging.ID, "web", directImageServiceSpec("example.test/web:staging", nil))
	if err != nil {
		t.Fatal(err)
	}
	node1Before := mustDesiredRevision(t, store, ctx, "node-1")
	node2Before := mustDesiredRevision(t, store, ctx, "node-2")
	deployed, notifiedAgentIDs, err := releaseEnvironmentForTest(ctx, store, "owner", staging.ID)
	if err != nil || len(deployed) != 1 || deployed[0].ID != stagingService.ID {
		t.Fatalf("release staging: %#v: %v", deployed, err)
	}
	if !slices.Equal(notifiedAgentIDs, []string{"node-1", "node-2"}) {
		t.Fatalf("new allocation notified agents %v, want both cluster nodes", notifiedAgentIDs)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != node1Before+1 {
		t.Fatalf("node-1 revision after release = %d, want %d", got, node1Before+1)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-2"); got != node2Before+1 {
		t.Fatalf("node-2 revision after release = %d, want %d", got, node2Before+1)
	}
	productionService, err = store.reads.serviceByID(ctx, "owner", productionService.ID)
	if err != nil {
		t.Fatal(err)
	}
	if productionService.RolloutGeneration != 0 || productionService.AllocatedAgentID != "" {
		t.Fatalf("staging deploy affected production: %#v", productionService)
	}
	state, err := desiredStateForAgent(ctx, store, "node-1")
	if err != nil || len(state.GetServices()) != 1 || state.GetServices()[0].GetServiceId() != stagingService.ID {
		t.Fatalf("unexpected desired state after staging deploy: %#v: %v", state.GetServices(), err)
	}
	node1Before = mustDesiredRevision(t, store, ctx, "node-1")
	node2Before = mustDesiredRevision(t, store, ctx, "node-2")
	notifier := &recordingNotifier{}
	ingress := &countingIngress{}
	operations := NewEnvironmentOperations(store.platform(), notifier, ingress)
	_, err = operations.DeleteEnvironment(contextWithDelegatedUser("owner", "owner@example.com"), &platformv1.DeleteEnvironmentRequest{EnvironmentId: staging.ID})
	notifiedAgentIDs = notifier.agentIDs
	if ingress.requests.Load() != 1 {
		t.Fatal("environment deletion did not request ingress sync")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(notifiedAgentIDs, []string{"node-1", "node-2"}) {
		t.Fatalf("deleted allocation notified agents %v, want both cluster nodes", notifiedAgentIDs)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != node1Before+1 {
		t.Fatalf("node-1 revision after environment delete = %d, want %d", got, node1Before+1)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-2"); got != node2Before+1 {
		t.Fatalf("node-2 revision after environment delete = %d, want %d", got, node2Before+1)
	}
	state, err = desiredStateForAgent(ctx, store, "node-1")
	if err != nil || len(state.GetServices()) != 0 {
		t.Fatalf("deleted environment retained desired workloads: %#v: %v", state.GetServices(), err)
	}
	if _, err := store.reads.deliveryQueries().EnvironmentByID(ctx, "owner", production.ID); err != nil {
		t.Fatalf("staging delete affected production: %v", err)
	}
}

func (s *persistence) createStagedServiceForTest(ctx context.Context, userID, environmentID, name string, spec *platformv1.ServiceSpec) (deliverycore.ServiceRecord, error) {
	return createScheduledService(ctx, s, userID, environmentID, name, spec)
}
