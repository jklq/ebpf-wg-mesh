//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/authz"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
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
	service := NewPlatformService(store.platform(), noopNotifier{}, noopIngress{}, newTestDelivery(store, noopNotifier{}, noopIngress{}, nil))
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
	service := NewPlatformService(store.platform(), noopNotifier{}, noopIngress{}, newTestDelivery(store, noopNotifier{}, noopIngress{}, nil))
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
	service := NewPlatformService(store.platform(), noopNotifier{}, noopIngress{}, newTestDelivery(store, noopNotifier{}, noopIngress{}, nil),
		WithGitHubSourceInspection(catalog, client, authz.NewAuthorizer(store.db)))
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
