//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

func TestListProjectsExcludesManagedProjects(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{
			ID:       "user-1",
			Email:    "user@example.com",
			Projects: []string{"demo"},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	managed, err := store.catalog.ensureManagedProject(ctx, "Platform Dashboard", "dashboard")
	if err != nil {
		t.Fatalf("ensureManagedProject: %v", err)
	}
	if managed.Kind != deliverycore.ProjectKindManaged {
		t.Fatalf("expected managed project kind, got %s", managed.Kind)
	}

	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
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

	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
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

	firstProjects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil {
		t.Fatalf("listProjects(user-1): %v", err)
	}
	secondProjects, err := store.catalog.listProjects(ctx, testUser("user-2"), false)
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
	if _, err := store.catalog.projectByID(ctx, testUser("user-1"), secondProjects[0].ID); err == nil {
		t.Fatalf("expected user-1 to be denied access to user-2 project %q", secondProjects[0].ID)
	}
}

func TestCreateProjectRepairsOwnerMembership(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{
			ID:       "user-1",
			Email:    "user1@example.com",
			Projects: []string{"demo"},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
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
	if _, err := store.catalog.authz.AuthorizeProject(ctx, testUser("user-1"), projectID, authz.Write); !errors.Is(err, authz.ErrDenied) || !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected viewer write denial, got %v", err)
	}

	project, err := store.catalog.createProject(ctx, testUser("user-1"), "demo")
	if err != nil {
		t.Fatalf("createProject: %v", err)
	}
	if project.ID != projectID {
		t.Fatalf("expected repaired project %q, got %q", projectID, project.ID)
	}
	assertProjectOwnerInvariant(t, store, projectID, "user-1")
}

func assertProjectOwnerInvariant(t *testing.T, store *persistence, projectID, userID string) {
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

func TestDuplicateEnvironmentSkipsDeletedChildren(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.catalog.productionEnvironmentByProjectInternal(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	volume, err := store.catalog.createScheduledVolume(ctx, testUser("owner"), source.ID, "data", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	service, err := store.createStagedServiceForTest(ctx, "owner", source.ID, "web", directImageServiceSpec("example.test/web:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.db.ExecContext(ctx, `UPDATE volumes SET deleted_at=$1, delete_expires_at=$2 WHERE id=$3`, now, now.Add(time.Hour), volume.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE services SET deleted_at=$1, delete_expires_at=$2 WHERE id=$3`, now, now.Add(time.Hour), service.ID); err != nil {
		t.Fatal(err)
	}
	duplicate, err := newTestDelivery(store, nil, nil, nil).DuplicateEnvironment(ctx, testUser("owner"), source.ID, "Staging", true)
	if err != nil {
		t.Fatal(err)
	}
	var volumes, services int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM volumes WHERE environment_id=$1`, duplicate.ID).Scan(&volumes); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM services WHERE environment_id=$1`, duplicate.ID).Scan(&services); err != nil {
		t.Fatal(err)
	}
	if volumes != 0 || services != 0 {
		t.Fatalf("deleted children were duplicated: volumes=%d services=%d", volumes, services)
	}
}

func TestDeletedProductionEnvironmentIsNotReused(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	production, err := store.catalog.productionEnvironmentByProjectInternal(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.deleteEnvironment(ctx, testUser("owner"), production.ID, production.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.productionEnvironmentByProjectInternal(ctx, project.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("production lookup after delete: %v", err)
	}
	if _, err := store.catalog.ensureProductionEnvironmentQuerier(ctx, store.db, project.ID); !errors.Is(err, deliverycore.ErrEnvironmentDeleted) {
		t.Fatalf("ensure production after delete: %v", err)
	}
}

func TestEnvironmentLifecycleAndAuthorization(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()

	project, err := store.catalog.createProject(ctx, testUser("owner"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	production, err := store.catalog.productionEnvironmentByProjectInternal(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.deleteEnvironment(ctx, testUser("owner"), production.ID, ""); !errors.Is(err, deliverycore.ErrConfirmationMismatch) {
		t.Fatalf("delete production without confirmation: %v", err)
	}
	staging, err := store.catalog.createEnvironment(ctx, testUser("owner"), project.ID, "Staging")
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := store.catalog.renameEnvironment(ctx, testUser("owner"), staging.ID, "QA")
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
	if _, err := store.catalog.createEnvironment(ctx, testUser("editor"), project.ID, "Editor Sandbox"); err != nil {
		t.Fatalf("editor could not create environment: %v", err)
	}
	if _, err := store.catalog.createEnvironment(ctx, testUser("viewer"), project.ID, "Viewer Sandbox"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("viewer mutated project environments: %v", err)
	}
	if environments, err := store.catalog.listEnvironments(ctx, testUser("viewer"), project.ID, false); err != nil || len(environments) != 3 {
		t.Fatalf("viewer could not read project environments: %#v: %v", environments, err)
	}

	_, err = store.catalog.createProject(ctx, testUser("other"), "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.reads.EnvironmentByID(ctx, testUser("other"), staging.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-project environment access: %v", err)
	}
	if _, err := store.createStagedServiceForTest(ctx, "owner", staging.ID, "web", directImageServiceSpec("example.test/web:1", nil)); err != nil {
		t.Fatal(err)
	}
	services, err := store.reads.ListServices(ctx, testUser("owner"), staging.ID, false)
	if err != nil || len(services) != 1 {
		t.Fatalf("list staging services: %#v: %v", services, err)
	}
	updated, _, err := updateService(ctx, store, "editor", services[0].ID, "web-editor", services[0].Spec)
	if err != nil || updated.Name != "web-editor" {
		t.Fatalf("editor could not mutate environment service: %#v: %v", updated, err)
	}
	services[0] = updated
	if _, err := store.reads.ServiceByID(ctx, testUser("other"), services[0].ID); !errors.Is(err, sql.ErrNoRows) {
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
	project, err := store.catalog.createProject(ctx, testUser("owner"), "demo")
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
	volume, err := store.catalog.createScheduledVolume(ctx, testUser("owner"), source.ID, "data", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "owner", source.ID, "web", directImageServiceSpec("example.test/web:1", &platformv1.ServiceRuntime{
		Env: map[string]string{"SECRET": "production"}, Volume: &platformv1.ServiceVolumeMount{VolumeName: "data"},
	}), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, testUser("owner"), "web.example.test", service.ID, 8080); err != nil {
		t.Fatal(err)
	}

	duplicate, err := newTestDelivery(store, nil, nil, nil).DuplicateEnvironment(ctx, testUser("owner"), source.ID, "Staging", false)
	if err != nil {
		t.Fatal(err)
	}
	volumes, err := store.catalog.listVolumes(ctx, testUser("owner"), duplicate.ID, false)
	if err != nil || len(volumes) != 1 || volumes[0].ID == volume.ID {
		t.Fatalf("duplicated volumes: %#v: %v", volumes, err)
	}
	services, err := store.reads.ListServices(ctx, testUser("owner"), duplicate.ID, false)
	if err != nil || len(services) != 1 {
		t.Fatalf("duplicated services: %#v: %v", services, err)
	}
	copy := services[0]
	if copy.ID == service.ID || copy.RolloutGeneration != 0 || copy.AllocatedAgentID != "" || copy.ResolvedImage != "" {
		t.Fatalf("duplicate inherited runtime state: %#v", copy)
	}
	if len(copy.Spec.GetRuntime().GetEnv()) != 0 || copy.Spec.GetRuntime().GetVolume().GetVolumeName() != "data" {
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

	withVariables, err := newTestDelivery(store, nil, nil, nil).DuplicateEnvironment(ctx, testUser("owner"), source.ID, "Credentials Review", true)
	if err != nil {
		t.Fatal(err)
	}
	variableCopies, err := store.reads.ListServices(ctx, testUser("owner"), withVariables.ID, false)
	if err != nil || len(variableCopies) != 1 || variableCopies[0].Spec.GetRuntime().GetEnv()["SECRET"] != "production" {
		t.Fatalf("explicit variable copy failed: %#v: %v", variableCopies, err)
	}
}

func TestEnvironmentReleaseAndDeleteAreScoped(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	production, err := store.catalog.productionEnvironmentByProjectInternal(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	staging, err := store.catalog.createEnvironment(ctx, testUser("owner"), project.ID, "Staging")
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
		t.Fatalf("new allocation notification result = %v", notifiedAgentIDs)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != node1Before+1 {
		t.Fatalf("node-1 revision after release = %d, want %d", got, node1Before+1)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-2"); got != node2Before {
		t.Fatalf("unrelated node-2 revision after release = %d, want %d", got, node2Before)
	}
	productionService, err = store.reads.ServiceByID(ctx, testUser("owner"), productionService.ID)
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
	operations := newCatalogOperations(store.platform(), notifier, ingress)
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
	if got := mustDesiredRevision(t, store, ctx, "node-2"); got != node2Before {
		t.Fatalf("unrelated node-2 revision after environment delete = %d, want %d", got, node2Before)
	}
	state, err = desiredStateForAgent(ctx, store, "node-1")
	if err != nil || len(state.GetServices()) != 0 {
		t.Fatalf("deleted environment retained desired workloads: %#v: %v", state.GetServices(), err)
	}
	if _, err := store.reads.EnvironmentByID(ctx, testUser("owner"), production.ID); err != nil {
		t.Fatalf("staging delete affected production: %v", err)
	}
}

func (s *persistence) createStagedServiceForTest(ctx context.Context, userID, environmentID, name string, spec *platformv1.ServiceSpec) (deliverycore.ServiceRecord, error) {
	return createScheduledService(ctx, s, userID, environmentID, name, spec)
}

func TestSchemaRejectsCrossServiceReferences(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	err := store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		statements := []string{
			`INSERT INTO projects(id, name, kind, owner_user_id, created_at) VALUES ('integrity-project', 'integrity', 'user', 'owner', $1)`,
			`INSERT INTO environments(id, project_id, name, kind, auto_deploy, network_identity, created_at, updated_at) VALUES ('integrity-environment', 'integrity-project', 'production', 'production', TRUE, 9001, $1, $1)`,
			`INSERT INTO services(id, environment_id, name, current_spec_revision, desired_replica_count, created_at, updated_at) VALUES ('service-a', 'integrity-environment', 'a', 1, 1, $1, $1), ('service-b', 'integrity-environment', 'b', 1, 1, $1, $1)`,
			`INSERT INTO service_delivery_status(service_id, updated_at) VALUES ('service-a', $1), ('service-b', $1)`,
			`INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at) VALUES ('service-a', 1, '{}', $1), ('service-b', 1, '{}', $1)`,
			`INSERT INTO deployments(id, service_id, spec_revision, state, cause_kind, reason_code, created_at, updated_at) VALUES ('deployment-a', 'service-a', 1, 'staged', 'system', 'TEST', $1, $1), ('deployment-b', 'service-b', 1, 'staged', 'system', 'TEST', $1, $1)`,
			`INSERT INTO agent_registrations(id, name, region, failure_domain, created_at, updated_at) VALUES ('integrity-agent', 'integrity-agent', 'test', 'integrity-agent', $1, $1)`,
			`INSERT INTO agent_administration(agent_id, lifecycle_state, updated_at) VALUES ('integrity-agent', 'ready', $1)`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.db.ExecContext(ctx, `INSERT INTO allocation_assignments(
		id, service_id, deployment_id, agent_id, desired_spec_revision, desired_rollout_generation,
		rollout_state, intent, created_at, updated_at
	) VALUES ('bad-allocation', 'service-a', 'deployment-b', 'integrity-agent', 1, 0, 'starting', 'run', $1, $1)`, now); err == nil {
		t.Fatal("cross-service allocation was accepted")
	}

	if _, err := store.db.ExecContext(ctx, `INSERT INTO deployment_actions(
		id, service_id, target_deployment_id, action, idempotency_key, requested_by_user_id, created_at
	) VALUES ('bad-action', 'service-a', 'deployment-b', 'restore', 'bad-action', 'owner', $1)`, now); err == nil {
		t.Fatal("cross-service deployment action was accepted")
	}
}
