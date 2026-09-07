//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func linkTestProjectRepository(t *testing.T, store *Store, catalog *GitHubCatalog, projectID, repositorySelector string) {
	t.Helper()
	owner, repo, err := splitGitHubRepositorySelector(repositorySelector)
	if err != nil {
		t.Fatal(err)
	}
	view, err := catalog.ResolveRepositoryView(context.Background(), owner, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.linkProjectGitHubRepository(context.Background(), projectID, "user-1", view); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubCatalogResolveRepositoryUsesStoredStateOnly(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store, client)
	ctx := context.Background()

	repo, err := catalog.RepositoryView(ctx, "public", "hello", 0)
	if err != nil {
		t.Fatalf("RepositoryView(empty): %v", err)
	}
	if repo.AccessState != sourceAccessStateInstallationRequired {
		t.Fatalf("expected installation required for empty catalog, got %s", repo.AccessState)
	}
	if server.requestCount() != 0 {
		t.Fatalf("expected zero github api calls, got %d", server.requestCount())
	}

	if err := store.upsertGitHubRepositorySnapshot(ctx, githubRepositorySnapshotRecord{
		RepositoryID:  1,
		Owner:         "public",
		Repo:          "hello",
		FullName:      "public/hello",
		Private:       false,
		DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("upsertGitHubRepositorySnapshot: %v", err)
	}
	repo, err = catalog.RepositoryView(ctx, "public", "hello", 0)
	if err != nil {
		t.Fatalf("RepositoryView(snapshot): %v", err)
	}
	if repo.AccessState != sourceAccessStateAvailable {
		t.Fatalf("expected available repo from stored snapshot, got %s", repo.AccessState)
	}

	if err := store.replaceGitHubInstallationRepositories(ctx, githubInstallationRecord{
		InstallationID: 7,
		AccountLogin:   "private",
		AccountType:    "Organization",
		TargetType:     "Organization",
		Active:         true,
	}, []githubRepositoryRecord{{
		InstallationID: 7,
		RepositoryID:   2,
		Owner:          "private",
		Repo:           "secret",
		FullName:       "private/secret",
		Private:        true,
		DefaultBranch:  "main",
	}}); err != nil {
		t.Fatalf("replaceGitHubInstallationRepositories: %v", err)
	}

	view, err := catalog.RepositoryView(ctx, "private", "secret", 7)
	if err != nil {
		t.Fatalf("RepositoryView(private): %v", err)
	}
	if view.AccessState != sourceAccessStateAvailable {
		t.Fatalf("expected available repository view, got %+v", view)
	}
	if server.requestCount() != 0 {
		t.Fatalf("expected zero github api calls, got %d", server.requestCount())
	}
}

func TestPlatformServiceInspectSourceReturnsPublicRepositoryBuildHints(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store, client)
	service := NewPlatformService(
		store,
		noopNotifier{},
		noopIngress{},
		NewDelivery(store, noopNotifier{}, noopIngress{}, nil),
		WithGitHubSourceInspection(catalog, client),
	)
	projectID := bootstrapProjectAndAgent(t, store, context.Background())

	resp, err := service.LinkGitHubRepository(
		contextWithDelegatedUser("user-1", "user@example.com"),
		&platformv1.LinkGitHubRepositoryRequest{
			ProjectId:             projectID,
			RepositorySelector:    "public/hello",
			GithubUserAccessToken: "user-token",
		},
	)
	if err != nil {
		t.Fatalf("InspectSource: %v", err)
	}
	if got := resp.GetAccessState(); got != platformv1.SourceAccessState_SOURCE_ACCESS_STATE_AVAILABLE {
		t.Fatalf("unexpected access state %s", got)
	}
	if resp.GetDefaultBranch() != "main" {
		t.Fatalf("unexpected default branch %q", resp.GetDefaultBranch())
	}
	if len(resp.GetDockerfileCandidates()) != 1 || resp.GetDockerfileCandidates()[0] != "Dockerfile" {
		t.Fatalf("unexpected dockerfile candidates %+v", resp.GetDockerfileCandidates())
	}
	if resp.GetRecommendedBuildRecipe().GetDockerfilePath() != "Dockerfile" || resp.GetRecommendedBuildRecipe().GetContextDir() != "." {
		t.Fatalf("unexpected recommended build recipe %+v", resp.GetRecommendedBuildRecipe())
	}
}

func TestPlatformServiceGitHubLinkRequiresUserRepositoryAuthorization(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	service := NewPlatformService(
		store,
		noopNotifier{},
		noopIngress{},
		NewDelivery(store, noopNotifier{}, noopIngress{}, nil),
		WithGitHubSourceInspection(NewGitHubCatalog(store, client), client),
	)
	projectID := bootstrapProjectAndAgent(t, store, context.Background())
	ctx := contextWithDelegatedUser("user-1", "user@example.com")

	for _, test := range []struct {
		name  string
		token string
		code  codes.Code
	}{
		{name: "app installation access alone is insufficient", code: codes.Unauthenticated},
		{name: "wrong user token", token: "wrong-user-token", code: codes.Unauthenticated},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.LinkGitHubRepository(ctx, &platformv1.LinkGitHubRepositoryRequest{
				ProjectId:             projectID,
				RepositorySelector:    "private/secret",
				GithubUserAccessToken: test.token,
			})
			if status.Code(err) != test.code {
				t.Fatalf("LinkGitHubRepository code = %s, want %s (error: %v)", status.Code(err), test.code, err)
			}
			if _, err := store.projectGitHubRepositoryInstallation(context.Background(), projectID, "private", "secret"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("repository link was persisted after denied user authorization: %v", err)
			}
		})
	}

	if _, err := service.LinkGitHubRepository(ctx, &platformv1.LinkGitHubRepositoryRequest{
		ProjectId:             projectID,
		RepositorySelector:    "private/secret",
		GithubUserAccessToken: "user-token",
	}); err != nil {
		t.Fatalf("LinkGitHubRepository with authorized user token: %v", err)
	}
	if _, err := service.InspectSource(ctx, &platformv1.InspectSourceRequest{
		ProjectId:          projectID,
		Provider:           "github",
		RepositorySelector: "private/secret",
	}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("InspectSource without user token code = %s, want %s (error: %v)", status.Code(err), codes.Unauthenticated, err)
	}
	if _, err := service.InspectSource(ctx, &platformv1.InspectSourceRequest{
		ProjectId:             projectID,
		Provider:              "github",
		RepositorySelector:    "private/secret",
		GithubUserAccessToken: "user-token",
	}); err != nil {
		t.Fatalf("InspectSource with authorized user token: %v", err)
	}

	created, err := service.CreateService(ctx, &platformv1.CreateServiceRequest{
		EnvironmentId: productionEnvironmentID(t, store, projectID),
		Service: &platformv1.ServiceInput{
			Name: "authorized-private-service",
			Spec: repositoryServiceSpec(
				&platformv1.ServiceRuntime{CpuMillis: 250, MemoryMebibytes: 256, Ports: runtimePortsFromInts([]int32{8080})},
				&platformv1.ServiceSourceSpec{
					Provider:           "github",
					RepositorySelector: "private/secret",
					TrackedRef:         "main",
					BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
				},
			),
		},
	})
	if err != nil {
		t.Fatalf("CreateService from user-authorized project link: %v", err)
	}
	if got := created.GetSpec().GetSource().GetSourceSpec().GetRepositorySelector(); got != "private/secret" {
		t.Fatalf("created service repository = %q", got)
	}
}

func TestPlatformServiceInspectSourceReturnsInstallationRequiredForPrivateRepoWithoutGrant(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	server.installationID = 0
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store, client)
	service := NewPlatformService(
		store,
		noopNotifier{},
		noopIngress{},
		NewDelivery(store, noopNotifier{}, noopIngress{}, nil),
		WithGitHubSourceInspection(catalog, client),
	)
	projectID := bootstrapProjectAndAgent(t, store, context.Background())

	resp, err := service.LinkGitHubRepository(
		contextWithDelegatedUser("user-1", "user@example.com"),
		&platformv1.LinkGitHubRepositoryRequest{
			ProjectId:             projectID,
			RepositorySelector:    "private/secret",
			GithubUserAccessToken: "user-token",
		},
	)
	if err != nil {
		t.Fatalf("InspectSource: %v", err)
	}
	if got := resp.GetAccessState(); got != platformv1.SourceAccessState_SOURCE_ACCESS_STATE_INSTALLATION_REQUIRED {
		t.Fatalf("unexpected access state %s", got)
	}
	if len(resp.GetDockerfileCandidates()) != 0 {
		t.Fatalf("expected no dockerfile candidates, got %+v", resp.GetDockerfileCandidates())
	}
}

func TestPlatformServiceInspectSourceResolvesPrivateRepositoryAfterInstallation(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store, client)
	service := NewPlatformService(
		store,
		noopNotifier{},
		noopIngress{},
		NewDelivery(store, noopNotifier{}, noopIngress{}, nil),
		WithGitHubSourceInspection(catalog, client),
	)
	projectID := bootstrapProjectAndAgent(t, store, context.Background())

	resp, err := service.LinkGitHubRepository(
		contextWithDelegatedUser("user-1", "user@example.com"),
		&platformv1.LinkGitHubRepositoryRequest{
			ProjectId:             projectID,
			RepositorySelector:    "private/secret",
			GithubUserAccessToken: "user-token",
		},
	)
	if err != nil {
		t.Fatalf("InspectSource: %v", err)
	}
	if got := resp.GetAccessState(); got != platformv1.SourceAccessState_SOURCE_ACCESS_STATE_AVAILABLE {
		t.Fatalf("unexpected access state %s", got)
	}
	if resp.GetDefaultBranch() != "main" {
		t.Fatalf("unexpected default branch %q", resp.GetDefaultBranch())
	}
	if len(resp.GetDockerfileCandidates()) != 1 || resp.GetDockerfileCandidates()[0] != "Dockerfile" {
		t.Fatalf("unexpected dockerfile candidates %+v", resp.GetDockerfileCandidates())
	}
	if got := server.pathHits("/repos/private/secret/installation"); got != 1 {
		t.Fatalf("expected one installation lookup, got %d", got)
	}
	if got := server.pathHits("/installation/repositories"); got != 1 {
		t.Fatalf("expected one installation repository refresh, got %d", got)
	}
	view, err := catalog.RepositoryView(context.Background(), "private", "secret", 0)
	if err != nil {
		t.Fatalf("RepositoryView: %v", err)
	}
	if view.AccessState != sourceAccessStateAvailable || view.InstallationID != 7 {
		t.Fatalf("unexpected stored repository view %+v", view)
	}
}

func TestPlatformServiceInspectSourceResolvesPrivateRepositoryAfterForbiddenRepositoryProbe(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	server.privateRepoStatus = http.StatusForbidden
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store, client)
	service := NewPlatformService(
		store,
		noopNotifier{},
		noopIngress{},
		NewDelivery(store, noopNotifier{}, noopIngress{}, nil),
		WithGitHubSourceInspection(catalog, client),
	)
	projectID := bootstrapProjectAndAgent(t, store, context.Background())

	resp, err := service.LinkGitHubRepository(
		contextWithDelegatedUser("user-1", "user@example.com"),
		&platformv1.LinkGitHubRepositoryRequest{
			ProjectId:             projectID,
			RepositorySelector:    "private/secret",
			GithubUserAccessToken: "user-token",
		},
	)
	if err != nil {
		t.Fatalf("InspectSource: %v", err)
	}
	if got := resp.GetAccessState(); got != platformv1.SourceAccessState_SOURCE_ACCESS_STATE_AVAILABLE {
		t.Fatalf("unexpected access state %s", got)
	}
}
