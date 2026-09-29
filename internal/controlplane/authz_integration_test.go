//go:build integration

package controlplane

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

func seedAuthzProject(t *testing.T, store *persistence, ctx context.Context) (projectID, environmentID, serviceID, volumeID, hostname string) {
	t.Helper()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "owner", Email: "owner@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("owner"), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	projectID = projects[0].ID
	env, err := store.catalog.createEnvironment(ctx, testUser("owner"), projectID, "staging")
	if err != nil {
		t.Fatal(err)
	}
	environmentID = env.ID
	service, err := createScheduledService(ctx, store, "owner", environmentID, "web", serviceSpec())
	if err != nil {
		t.Fatal(err)
	}
	serviceID = service.ID
	volume, err := store.catalog.createScheduledVolume(ctx, testUser("owner"), environmentID, "data", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	volumeID = volume.ID
	binding, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, testUser("owner"), "web.mesh.test", serviceID, 8080)
	if err != nil {
		t.Fatal(err)
	}
	hostname = binding.Hostname
	for _, member := range []struct{ id, role string }{{"editor", "editor"}, {"viewer", "viewer"}} {
		if _, err := store.db.ExecContext(ctx,
			`INSERT INTO project_memberships(user_id, project_id, role) VALUES ($1, $2, $3)`,
			member.id, projectID, member.role); err != nil {
			t.Fatal(err)
		}
	}
	return projectID, environmentID, serviceID, volumeID, hostname
}

func TestAuthorizerRoleMatrix(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	projectID, environmentID, serviceID, volumeID, hostname := seedAuthzProject(t, store, ctx)
	authorizer := authz.NewAuthorizer(store.db)

	cases := []struct {
		user  string
		need  authz.Access
		allow bool
	}{
		{"owner", authz.Read, true},
		{"owner", authz.Write, true},
		{"editor", authz.Read, true},
		{"editor", authz.Write, true},
		{"viewer", authz.Read, true},
		{"viewer", authz.Write, false},
		{"outsider", authz.Read, false},
		{"outsider", authz.Write, false},
	}
	assertDenial := func(kind, user string, err error) {
		t.Helper()
		if !errors.Is(err, authz.ErrDenied) || !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s denial for %q = %v, want ErrDenied wrapping ErrNoRows", kind, user, err)
		}
	}
	for _, tc := range cases {
		user := testUser(tc.user)
		scope, err := authorizer.AuthorizeProject(ctx, user, projectID, tc.need)
		if (err == nil) != tc.allow {
			t.Fatalf("project %s/%d allow=%v", tc.user, tc.need, err == nil)
		} else if tc.allow && (scope.ID() != projectID || scope.UserID() != tc.user) {
			t.Fatalf("project scope carries wrong ids: %+v", scope)
		} else if !tc.allow {
			assertDenial("project", tc.user, err)
		}
		if _, err := authorizer.AuthorizeEnvironment(ctx, user, environmentID, tc.need); (err == nil) != tc.allow {
			t.Fatalf("environment %s/%d allow=%v", tc.user, tc.need, err == nil)
		} else if !tc.allow {
			assertDenial("environment", tc.user, err)
		}
		serviceScope, err := authorizer.AuthorizeService(ctx, user, serviceID, tc.need)
		if (err == nil) != tc.allow {
			t.Fatalf("service %s/%d allow=%v", tc.user, tc.need, err == nil)
		} else if tc.allow && (serviceScope.ID() != serviceID || serviceScope.EnvironmentID() != environmentID || serviceScope.ProjectID() != projectID) {
			t.Fatalf("service scope carries wrong ids: %+v", serviceScope)
		} else if !tc.allow {
			assertDenial("service", tc.user, err)
		}
		if _, err := authorizer.AuthorizeVolume(ctx, user, volumeID, tc.need); (err == nil) != tc.allow {
			t.Fatalf("volume %s/%d allow=%v", tc.user, tc.need, err == nil)
		} else if !tc.allow {
			assertDenial("volume", tc.user, err)
		}
		if _, err := authorizer.AuthorizeDomainBinding(ctx, user, hostname, tc.need); (err == nil) != tc.allow {
			t.Fatalf("binding %s/%d allow=%v", tc.user, tc.need, err == nil)
		} else if !tc.allow {
			assertDenial("binding", tc.user, err)
		}
	}

	for _, id := range []string{"missing", ""} {
		user := testUser("owner")
		if _, err := authorizer.AuthorizeProject(ctx, user, id, authz.Read); err == nil {
			t.Fatalf("missing project %q authorized", id)
		} else {
			assertDenial("project", "owner", err)
		}
		if _, err := authorizer.AuthorizeEnvironment(ctx, user, id, authz.Read); err == nil {
			t.Fatalf("missing environment %q authorized", id)
		} else {
			assertDenial("environment", "owner", err)
		}
		if _, err := authorizer.AuthorizeService(ctx, user, id, authz.Read); err == nil {
			t.Fatalf("missing service %q authorized", id)
		} else {
			assertDenial("service", "owner", err)
		}
		if _, err := authorizer.AuthorizeVolume(ctx, user, id, authz.Read); err == nil {
			t.Fatalf("missing volume %q authorized", id)
		} else {
			assertDenial("volume", "owner", err)
		}
		if _, err := authorizer.AuthorizeDomainBinding(ctx, user, id, authz.Read); err == nil {
			t.Fatalf("missing binding %q authorized", id)
		} else {
			assertDenial("binding", "owner", err)
		}
	}
	if _, err := authorizer.AuthorizeOperator(ctx, testUser("owner")); err == nil {
		t.Fatal("non-operator authorized as operator")
	}
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO platform_operators(user_id, created_at) VALUES ('owner', statement_timestamp())`); err != nil {
		t.Fatal(err)
	}
	if _, err := authorizer.AuthorizeOperator(ctx, testUser("owner")); err != nil {
		t.Fatalf("operator denied: %v", err)
	}
}

func TestAuthorizerExcludesManagedProjects(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	projectID, environmentID, serviceID, volumeID, hostname := seedAuthzProject(t, store, ctx)
	if _, err := store.db.ExecContext(ctx,
		`UPDATE projects SET kind = 'managed', system_key = 'dashboard' WHERE id = $1`, projectID); err != nil {
		t.Fatal(err)
	}
	authorizer := authz.NewAuthorizer(store.db)
	user := testUser("owner")
	_, projectErr := authorizer.AuthorizeProject(ctx, user, projectID, authz.Read)
	_, environmentErr := authorizer.AuthorizeEnvironment(ctx, user, environmentID, authz.Read)
	_, serviceErr := authorizer.AuthorizeService(ctx, user, serviceID, authz.Read)
	_, volumeErr := authorizer.AuthorizeVolume(ctx, user, volumeID, authz.Read)
	_, bindingErr := authorizer.AuthorizeDomainBinding(ctx, user, hostname, authz.Read)
	for _, denial := range []struct {
		kind string
		err  error
	}{
		{"project", projectErr},
		{"environment", environmentErr},
		{"service", serviceErr},
		{"volume", volumeErr},
		{"binding", bindingErr},
	} {
		if !errors.Is(denial.err, authz.ErrDenied) || !errors.Is(denial.err, sql.ErrNoRows) {
			t.Fatalf("managed %s denial = %v, want ErrDenied wrapping ErrNoRows", denial.kind, denial.err)
		}
	}
}

// TestAuthorizerReadsLiveMembership pins the property the outside-transaction
// minting relies on: every Authorize call reads current membership, so a role
// change or removal takes effect on the very next call. There is no cached or
// long-lived grant to go stale.
func TestAuthorizerReadsLiveMembership(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	projectID, _, serviceID, _, _ := seedAuthzProject(t, store, ctx)
	authorizer := authz.NewAuthorizer(store.db)
	viewer := testUser("viewer")
	if _, err := authorizer.AuthorizeService(ctx, viewer, serviceID, authz.Write); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("viewer write = %v, want ErrDenied", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE project_memberships SET role = 'editor' WHERE user_id = 'viewer' AND project_id = $1`, projectID); err != nil {
		t.Fatal(err)
	}
	if _, err := authorizer.AuthorizeService(ctx, viewer, serviceID, authz.Write); err != nil {
		t.Fatalf("promoted viewer write = %v, want allow", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`DELETE FROM project_memberships WHERE user_id = 'viewer' AND project_id = $1`, projectID); err != nil {
		t.Fatal(err)
	}
	if _, err := authorizer.AuthorizeService(ctx, viewer, serviceID, authz.Read); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("removed member read = %v, want ErrDenied", err)
	}
}

func TestAuthorizerDenialMapsToNotFound(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	projectID, _, _, _, _ := seedAuthzProject(t, store, ctx)
	authorizer := authz.NewAuthorizer(store.db)
	_, err := authorizer.AuthorizeProject(ctx, testUser("outsider"), projectID, authz.Read)
	if !errors.Is(err, authz.ErrDenied) || !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("denial = %v, want ErrDenied wrapping ErrNoRows", err)
	}
}

func TestListAgentsRequiresOperator(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	service := newPlatformService(store.platform(), noopNotifier{}, noopIngress{}, newTestDelivery(store, noopNotifier{}, noopIngress{}, nil))
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ListAgents(contextWithDelegatedUser("user-1", ""), &emptypb.Empty{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-operator ListAgents = %v, want PermissionDenied", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO platform_operators(user_id, created_at) VALUES ('user-1', statement_timestamp())`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ListAgents(contextWithDelegatedUser("user-1", ""), &emptypb.Empty{}); err != nil {
		t.Fatalf("operator ListAgents: %v", err)
	}
}

func TestCreateVolumeRequiresWrite(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	_, environmentID, _, _, _ := seedAuthzProject(t, store, ctx)
	service := newPlatformService(store.platform(), noopNotifier{}, noopIngress{}, newTestDelivery(store, noopNotifier{}, noopIngress{}, nil))
	if _, err := service.CreateVolume(contextWithDelegatedUser("viewer", ""), &platformv1.CreateVolumeRequest{
		EnvironmentId: environmentID,
		Name:          "sneaky",
		SizeBytes:     64 << 20,
	}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer create volume = %v, want PermissionDenied", err)
	}
	if _, err := store.catalog.createScheduledVolume(ctx, testUser("editor"), environmentID, "data-2", 64<<20); err != nil {
		t.Fatalf("editor create volume: %v", err)
	}
}

func TestLinkGitHubRepositoryRequiresProjectAccess(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	projectID, _, _, _, _ := seedAuthzProject(t, store, context.Background())
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store.source, client)
	service := newPlatformService(store.platform(), noopNotifier{}, noopIngress{}, newTestDelivery(store, noopNotifier{}, noopIngress{}, nil),
		withGitHubSourceInspection(catalog, client, authz.NewAuthorizer(store.db)))
	if _, err := service.LinkGitHubRepository(contextWithDelegatedUser("outsider", ""), &platformv1.LinkGitHubRepositoryRequest{
		ProjectId:             projectID,
		RepositorySelector:    "acme/web",
		GithubUserAccessToken: "user-token",
	}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("outsider link = %v, want PermissionDenied", err)
	}
	if got := server.requestCount(); got != 0 {
		t.Fatalf("denied link made %d github calls", got)
	}
}

func TestEnvironmentNetworkIdentitiesAreUniqueAndDeliveredToAgents(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{Users: []config.BootstrapUser{
		{ID: "user-1", Email: "user-1@example.com", Projects: []string{"one", "two"}},
	}}); err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil {
		t.Fatalf("listProjects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("expected two projects, got %d", len(projects))
	}
	environmentsOne, err := store.catalog.listEnvironments(ctx, testUser("user-1"), projects[0].ID, false)
	if err != nil || len(environmentsOne) != 1 {
		t.Fatalf("list first project environments: %#v: %v", environmentsOne, err)
	}
	environmentsTwo, err := store.catalog.listEnvironments(ctx, testUser("user-1"), projects[1].ID, false)
	if err != nil || len(environmentsTwo) != 1 {
		t.Fatalf("list second project environments: %#v: %v", environmentsTwo, err)
	}
	if environmentsOne[0].NetworkIdentity == 0 || environmentsTwo[0].NetworkIdentity == 0 || environmentsOne[0].NetworkIdentity == environmentsTwo[0].NetworkIdentity {
		t.Fatalf("expected distinct non-zero network identities: %#v %#v", environmentsOne, environmentsTwo)
	}
	staging, err := store.catalog.createEnvironment(ctx, testUser("user-1"), projects[0].ID, "Staging")
	if err != nil {
		t.Fatalf("create staging environment: %v", err)
	}
	if staging.NetworkIdentity == environmentsOne[0].NetworkIdentity {
		t.Fatalf("environments in one project shared a network identity: %#v %#v", environmentsOne[0], staging)
	}

	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatalf("upsertAgent: %v", err)
	}
	service, err := createScheduledService(ctx, store, "user-1", environmentsOne[0].ID, "web", directImageServiceSpec("nginx:1.27", &platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})}))
	if err != nil {
		t.Fatalf("createScheduledService: %v", err)
	}
	deployed, _, err := releaseEnvironmentForTest(ctx, store, "user-1", environmentsOne[0].ID)
	if err != nil || len(deployed) != 1 {
		t.Fatalf("releaseEnvironment: %#v: %v", deployed, err)
	}
	service = deployed[0]
	stagingService, err := createScheduledService(ctx, store, "user-1", staging.ID, "web", directImageServiceSpec("nginx:1.27", &platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})}))
	if err != nil {
		t.Fatalf("create staging service: %v", err)
	}
	stagingDeployed, _, err := releaseEnvironmentForTest(ctx, store, "user-1", staging.ID)
	if err != nil || len(stagingDeployed) != 1 {
		t.Fatalf("release staging environment: %#v: %v", stagingDeployed, err)
	}
	stagingService = stagingDeployed[0]
	for label, item := range map[string]deliverycore.ServiceRecord{"production": service, "staging": stagingService} {
		allocation, err := store.primaryAllocationForTest(ctx, item.ID)
		if err != nil {
			t.Fatalf("load %s allocation: %v", label, err)
		}
		if err := store.markAllocationHealthyForTest(ctx, item.ID, allocation.AllocationIPv4, 8080); err != nil {
			t.Fatalf("mark %s healthy: %v", label, err)
		}
		obs, ok := fixtureLive(store).Observation(allocation.ID, allocation.DesiredRolloutGeneration)
		if !ok {
			t.Fatalf("missing live observation for %s", label)
		}
		obs.HealthyIPv6Ports = []int32{8080}
		if _, err := fixtureLive(store).RecordObservation(obs); err != nil {
			t.Fatalf("mark %s IPv6 healthy: %v", label, err)
		}
	}
	state, err := desiredStateForAgent(ctx, store, service.AllocatedAgentID)
	if err != nil {
		t.Fatalf("desiredStateForAgent: %v", err)
	}
	if len(state.GetServices()) != 2 {
		t.Fatalf("expected both environment services in desired state: %#v", state.GetServices())
	}
	servicesByID := make(map[string]*agentv1.DesiredService, len(state.GetServices()))
	for _, desired := range state.GetServices() {
		servicesByID[desired.GetServiceId()] = desired
	}
	productionDesired := servicesByID[service.ID]
	stagingDesired := servicesByID[stagingService.ID]
	if productionDesired.GetEnvironmentId() != environmentsOne[0].ID || productionDesired.GetNetworkIdentity() != environmentsOne[0].NetworkIdentity {
		t.Fatalf("production network identity was not delivered: %#v", productionDesired)
	}
	if stagingDesired.GetEnvironmentId() != staging.ID || stagingDesired.GetNetworkIdentity() != staging.NetworkIdentity {
		t.Fatalf("staging network identity was not delivered: %#v", stagingDesired)
	}
	if productionDesired.GetPrivateIpv6() == stagingDesired.GetPrivateIpv6() {
		t.Fatalf("environment workload addresses collided: %q", productionDesired.GetPrivateIpv6())
	}
	for label, desired := range map[string]*agentv1.DesiredService{
		"production": productionDesired,
		"staging":    stagingDesired,
	} {
		if desired.GetInternalHostname() != "web.mesh.internal" {
			t.Fatalf("%s service got internal hostname %q", label, desired.GetInternalHostname())
		}
		if len(desired.GetInternalHosts()) != 1 || desired.GetInternalHosts()[0].GetHostname() != "web.mesh.internal" {
			t.Fatalf("%s service received cross-environment internal hosts: %#v", label, desired.GetInternalHosts())
		}
		if desired.GetInternalHosts()[0].GetIpv4() != desired.GetPrivateIpv4() ||
			desired.GetInternalHosts()[0].GetIpv6() != desired.GetPrivateIpv6() {
			t.Fatalf("%s internal host points at %q/%q, want %q/%q", label,
				desired.GetInternalHosts()[0].GetIpv4(), desired.GetInternalHosts()[0].GetIpv6(),
				desired.GetPrivateIpv4(), desired.GetPrivateIpv6())
		}
	}
}

func TestAgentBootstrapTokensAreBoundDurableAndSingleUse(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	tokens := []config.AgentBootstrapToken{{AgentID: "node-1", Token: "one-time-secret"}}
	if err := store.fleet.ensureAgentBootstrapTokens(ctx, tokens); err != nil {
		t.Fatalf("ensureAgentBootstrapTokens: %v", err)
	}
	if err := store.fleet.ConsumeAgentBootstrapToken(ctx, "node-2", "one-time-secret"); !errors.Is(err, errInvalidBootstrapToken) {
		t.Fatalf("expected agent binding rejection, got %v", err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() {
			results <- store.fleet.ConsumeAgentBootstrapToken(ctx, "node-1", "one-time-secret")
		}()
	}
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		} else if !errors.Is(err, errInvalidBootstrapToken) {
			t.Fatalf("unexpected concurrent consumption error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly one token consumer, got %d", successes)
	}
	if err := store.fleet.ensureAgentBootstrapTokens(ctx, tokens); err != nil {
		t.Fatalf("reseed bootstrap tokens: %v", err)
	}
	if err := store.fleet.ConsumeAgentBootstrapToken(ctx, "node-1", "one-time-secret"); !errors.Is(err, errInvalidBootstrapToken) {
		t.Fatalf("expected consumed token rejection after reseed, got %v", err)
	}
}

func TestConsumeOrRecoverAgentBootstrapTokenAllowsSameKeyRetry(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.fleet.ensureAgentBootstrapTokens(ctx, []config.AgentBootstrapToken{{AgentID: "node-1", Token: "retry-secret"}}); err != nil {
		t.Fatalf("ensureAgentBootstrapTokens: %v", err)
	}
	keyA := bytes.Repeat([]byte{1}, 32)
	keyB := bytes.Repeat([]byte{2}, 32)
	if err := store.fleet.ConsumeOrRecoverAgentBootstrapToken(ctx, "node-1", "retry-secret", keyA); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if err := store.fleet.ConsumeOrRecoverAgentBootstrapToken(ctx, "node-1", "retry-secret", keyA); err != nil {
		t.Fatalf("same-key recovery: %v", err)
	}
	if err := store.fleet.ConsumeOrRecoverAgentBootstrapToken(ctx, "node-1", "retry-secret", keyB); !errors.Is(err, errInvalidBootstrapToken) {
		t.Fatalf("different-key recovery = %v", err)
	}
	if err := store.fleet.ConsumeOrRecoverAgentBootstrapToken(ctx, "node-2", "retry-secret", keyA); !errors.Is(err, errInvalidBootstrapToken) {
		t.Fatalf("wrong-agent recovery = %v", err)
	}
}

func TestRemovedAgentBootstrapTokenIsRevoked(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.fleet.ensureAgentBootstrapTokens(ctx, []config.AgentBootstrapToken{{AgentID: "node-1", Token: "old-secret"}}); err != nil {
		t.Fatalf("seed old token: %v", err)
	}
	if err := store.fleet.ensureAgentBootstrapTokens(ctx, []config.AgentBootstrapToken{{AgentID: "node-1", Token: "new-secret"}}); err != nil {
		t.Fatalf("rotate token: %v", err)
	}
	if err := store.fleet.ConsumeAgentBootstrapToken(ctx, "node-1", "old-secret"); !errors.Is(err, errInvalidBootstrapToken) {
		t.Fatalf("expected removed token rejection, got %v", err)
	}
	if err := store.fleet.ConsumeAgentBootstrapToken(ctx, "node-1", "new-secret"); err != nil {
		t.Fatalf("consume replacement token: %v", err)
	}
}

func TestProjectCreationCreatesExactlyOneProductionEnvironment(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("user-1"), "demo")
	if err != nil {
		t.Fatalf("createProject: %v", err)
	}
	environments, err := store.catalog.listEnvironments(ctx, testUser("user-1"), project.ID, false)
	if err != nil {
		t.Fatalf("listEnvironments: %v", err)
	}
	if len(environments) != 1 || !environments[0].IsProduction || environments[0].NetworkIdentity == 0 {
		t.Fatalf("unexpected production environments: %#v", environments)
	}
}
