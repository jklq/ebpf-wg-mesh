//go:build integration

package controlplane

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

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
		WithGitHubSourceInspection(catalog, client),
	)

	resp, err := service.InspectSource(
		contextWithDelegatedUser("user-1", "user@example.com"),
		&platformv1.InspectSourceRequest{
			Provider:           "github",
			RepositorySelector: "public/hello",
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
		WithGitHubSourceInspection(catalog, client),
	)

	resp, err := service.InspectSource(
		contextWithDelegatedUser("user-1", "user@example.com"),
		&platformv1.InspectSourceRequest{
			Provider:           "github",
			RepositorySelector: "private/secret",
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
		WithGitHubSourceInspection(catalog, client),
	)

	resp, err := service.InspectSource(
		contextWithDelegatedUser("user-1", "user@example.com"),
		&platformv1.InspectSourceRequest{
			Provider:           "github",
			RepositorySelector: "private/secret",
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
		WithGitHubSourceInspection(catalog, client),
	)

	resp, err := service.InspectSource(
		contextWithDelegatedUser("user-1", "user@example.com"),
		&platformv1.InspectSourceRequest{
			Provider:           "github",
			RepositorySelector: "private/secret",
		},
	)
	if err != nil {
		t.Fatalf("InspectSource: %v", err)
	}
	if got := resp.GetAccessState(); got != platformv1.SourceAccessState_SOURCE_ACCESS_STATE_AVAILABLE {
		t.Fatalf("unexpected access state %s", got)
	}
}

func TestPlatformServiceCreateRepoBackedServiceQueuesSyncWithoutBranchLookup(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	service := NewPlatformService(store, noopNotifier{}, noopIngress{})
	ctx := context.Background()

	projectID := bootstrapProjectAndAgent(t, store, ctx)
	resp, err := service.CreateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateServiceRequest{
		ProjectId: projectID,
		Service: &platformv1.ServiceInput{
			Name: "web",
			Spec: repositoryServiceSpec(&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})}, &platformv1.ServiceSourceSpec{
				Provider:           "github",
				RepositorySelector: "public/hello",
				TrackedRef:         "main",
				BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
			}),
		},
	})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if resp.GetLatestBuild() != nil {
		t.Fatalf("expected no synchronous build in create response, got %+v", resp.GetLatestBuild())
	}
	if got := server.branchHeadHits(); got != 0 {
		t.Fatalf("expected no branch head lookup in request path, got %d", got)
	}
	if got := countSourceWorkItems(t, store, ctx, sourceWorkKindSourceSpecChanged); got != 1 {
		t.Fatalf("expected 1 queued sync item, got %d", got)
	}
}

func TestPlatformServiceUpdateAndRedeployQueueSyncWithoutBranchLookup(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	service := NewPlatformService(store, noopNotifier{}, noopIngress{})
	ctx := context.Background()

	projectID, serviceID := createRepoBackedTestService(t, store, ctx, "public/hello", 0, "main")
	if _, err := store.db.ExecContext(ctx, `DELETE FROM source_work_items`); err != nil {
		t.Fatalf("clear source work items: %v", err)
	}

	updateResp, err := service.UpdateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.UpdateServiceRequest{
		ProjectId: projectID,
		ServiceId: serviceID,
		Service: &platformv1.ServiceUpdate{
			Spec: repositoryServiceSpec(&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})}, &platformv1.ServiceSourceSpec{
				Provider:           "github",
				RepositorySelector: "public/hello",
				TrackedRef:         "release",
				BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
			}),
		},
	})
	if err != nil {
		t.Fatalf("UpdateService: %v", err)
	}
	if updateResp.GetLatestBuild() != nil {
		t.Fatalf("expected no synchronous build on source update, got %+v", updateResp.GetLatestBuild())
	}
	if got := countSourceWorkItems(t, store, ctx, sourceWorkKindSourceSpecChanged); got != 1 {
		t.Fatalf("expected 1 queued sync item after source update, got %d", got)
	}

	if _, err := store.db.ExecContext(ctx, `DELETE FROM source_work_items`); err != nil {
		t.Fatalf("clear source work items: %v", err)
	}
	statusResp, err := service.RedeployService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.RedeployServiceRequest{
		ProjectId: projectID,
		ServiceId: serviceID,
	})
	if err != nil {
		t.Fatalf("RedeployService: %v", err)
	}
	if statusResp.GetService().GetRolloutGeneration() != 2 {
		t.Fatalf("expected rollout generation to reflect stored state, got %d", statusResp.GetService().GetRolloutGeneration())
	}
	if statusResp.GetService().GetLatestBuild() != nil {
		t.Fatalf("expected no synchronous build on redeploy, got %+v", statusResp.GetService().GetLatestBuild())
	}
	if got := server.branchHeadHits(); got != 0 {
		t.Fatalf("expected no branch head lookup in request paths, got %d", got)
	}
	if got := countSourceWorkItems(t, store, ctx, sourceWorkKindSourceSpecChanged); got != 1 {
		t.Fatalf("expected 1 queued sync item after redeploy, got %d", got)
	}
}

func TestGitHubSyncServiceSourceQueuesBuildIdempotently(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store, client)
	coordinator := NewGitHubCoordinator(store, catalog, client, 5*time.Minute)
	reconciler := NewGitHubReconciler(store, coordinator, time.Minute, time.Minute, time.Minute)
	ctx := context.Background()

	projectID := bootstrapProjectAndAgent(t, store, ctx)
	service, err := store.createService(ctx, "user-1", projectID, "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "public/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	for i := 0; i < 3; i++ {
		processed, err := reconciler.processNext(ctx)
		if err != nil {
			t.Fatalf("processNext(%d): %v", i, err)
		}
		if !processed {
			break
		}
	}
	status, _, err := store.serviceStatus(ctx, "user-1", projectID, service.ID)
	if err != nil {
		t.Fatalf("serviceStatus: %v", err)
	}
	if status.LatestBuild == nil || status.LatestBuild.GetCommitSha() != "commit-public-main" {
		t.Fatalf("expected queued build for public main, got %+v", status.LatestBuild)
	}

	if _, err := store.enqueueSourceWorkItem(ctx, sourceWorkItemRecord{
		Kind:           sourceWorkKindSourceSpecChanged,
		IdempotencyKey: fmt.Sprintf("%s:%s:%d", sourceWorkKindSourceSpecChanged, service.ID, service.SpecRevision),
		ServiceID:      service.ID,
		SpecRevision:   service.SpecRevision,
	}); err != nil {
		t.Fatalf("enqueueSourceWorkItem(sync retry): %v", err)
	}
	for i := 0; i < 3; i++ {
		processed, err := reconciler.processNext(ctx)
		if err != nil {
			t.Fatalf("processNext(retry %d): %v", i, err)
		}
		if !processed {
			break
		}
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM build_runs WHERE service_id = $1`, service.ID).Scan(&count); err != nil {
		t.Fatalf("count build runs: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 build after sync retry, got %d", count)
	}
}

func TestGitHubSyncSameRepositoryServicesEachQueueBuild(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store, client)
	coordinator := NewGitHubCoordinator(store, catalog, client, 5*time.Minute)
	reconciler := NewGitHubReconciler(store, coordinator, time.Minute, time.Minute, time.Minute)
	ctx := context.Background()

	projectID := bootstrapProjectAndAgent(t, store, ctx)
	spec := repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "public/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	)
	first, err := store.createService(ctx, "user-1", projectID, "web-a", spec, "node-1")
	if err != nil {
		t.Fatalf("createService(first): %v", err)
	}
	second, err := store.createService(ctx, "user-1", projectID, "web-b", spec, "node-1")
	if err != nil {
		t.Fatalf("createService(second): %v", err)
	}

	for i := 0; i < 4; i++ {
		processed, err := reconciler.processNext(ctx)
		if err != nil {
			t.Fatalf("processNext(%d): %v", i, err)
		}
		if !processed {
			break
		}
	}

	for _, service := range []serviceRecord{first, second} {
		status, _, err := store.serviceStatus(ctx, "user-1", projectID, service.ID)
		if err != nil {
			t.Fatalf("serviceStatus(%s): %v", service.Name, err)
		}
		if status.LatestBuild == nil || status.LatestBuild.GetCommitSha() != "commit-public-main" {
			t.Fatalf("expected queued build for %s, got %+v", service.Name, status.LatestBuild)
		}
	}
}

func TestPushAndInstallationWebhooksOnlyQueueCoordinatorWork(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store, client)
	coordinator := NewGitHubCoordinator(store, catalog, client, 5*time.Minute)
	processor := NewGitHubWebhookProcessor(store, coordinator)
	ctx := context.Background()

	_, serviceID := createRepoBackedTestService(t, store, ctx, "private/secret", 7, "main")

	pushPayload := []byte(`{
		"ref":"refs/heads/main",
		"after":"commit-123",
		"repository":{"id":2,"name":"secret","full_name":"private/secret","owner":{"login":"private"}},
		"installation":{"id":7}
	}`)
	if err := processor.processPushEvent(ctx, pushPayload); err != nil {
		t.Fatalf("processPushEvent(first): %v", err)
	}
	if err := processor.processPushEvent(ctx, pushPayload); err != nil {
		t.Fatalf("processPushEvent(second): %v", err)
	}
	var buildCount int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM build_runs WHERE service_id = $1`, serviceID).Scan(&buildCount); err != nil {
		t.Fatalf("count build runs: %v", err)
	}
	if buildCount != 0 {
		t.Fatalf("expected webhook to queue work, not build directly, got %d build rows", buildCount)
	}
	if got := countSourceWorkItems(t, store, ctx, sourceWorkKindRevisionObserved); got != 1 {
		t.Fatalf("expected one queued build command after duplicate push, got %d", got)
	}

	installationPayload := []byte(`{
		"action":"created",
		"installation":{"id":9,"target_type":"Organization","account":{"login":"octo","type":"Organization"}}
	}`)
	if err := processor.processInstallationEvent(ctx, installationPayload); err != nil {
		t.Fatalf("processInstallationEvent: %v", err)
	}
	if got := countSourceWorkItems(t, store, ctx, sourceWorkKindProviderAccessChanged); got != 1 {
		t.Fatalf("expected one queued refresh command, got %d", got)
	}
}

func TestGitHubClientListInstallationRepositoriesPaginates(t *testing.T) {
	t.Parallel()

	repos := make([]map[string]any, 0, 101)
	for i := 0; i < 101; i++ {
		repos = append(repos, map[string]any{
			"id":             i + 1,
			"name":           fmt.Sprintf("repo-%d", i+1),
			"full_name":      fmt.Sprintf("acme/repo-%d", i+1),
			"private":        true,
			"default_branch": "main",
			"owner":          map[string]any{"login": "acme"},
		})
	}
	server := newTestGitHubServer(t, repos)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	items, err := client.ListInstallationRepositories(context.Background(), 7)
	if err != nil {
		t.Fatalf("ListInstallationRepositories: %v", err)
	}
	if len(items) != 101 {
		t.Fatalf("expected 101 repositories across pages, got %d", len(items))
	}
	if got := server.pathHits("/installation/repositories"); got < 2 {
		t.Fatalf("expected paginated repository listing to request multiple pages, got %d calls", got)
	}
}

func TestGitHubReconcilerBootstrapRequeuesStaleWork(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store, client)
	coordinator := NewGitHubCoordinator(store, catalog, client, time.Minute)
	reconciler := NewGitHubReconciler(store, coordinator, time.Minute, time.Minute, time.Minute)
	ctx := context.Background()

	if _, err := store.enqueueSourceWorkItem(ctx, sourceWorkItemRecord{
		Kind:                    sourceWorkKindProviderAccessChanged,
		IdempotencyKey:          "bootstrap-refresh-7",
		Provider:                "github",
		ProviderScopeExternalID: scopeExternalID(7),
	}); err != nil {
		t.Fatalf("enqueueSourceWorkItem: %v", err)
	}
	claimed, err := store.claimNextSourceWorkItem(ctx, "processor-1", 0)
	if err != nil {
		t.Fatalf("claimNextSourceWorkItem: %v", err)
	}
	if claimed.ID == "" {
		t.Fatal("expected claimed work item")
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE source_work_items SET updated_at = $1 WHERE id = $2`,
		time.Now().UTC().Add(-2*time.Minute), claimed.ID,
	); err != nil {
		t.Fatalf("age source work item: %v", err)
	}

	if err := reconciler.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	requeued, err := store.claimNextSourceWorkItem(ctx, "processor-2", 0)
	if err != nil {
		t.Fatalf("claimNextSourceWorkItem(requeued): %v", err)
	}
	if requeued.ID == "" {
		t.Fatal("expected stale in-flight work to be requeued")
	}
}

func TestGitHubWebhookHandlerRejectsInvalidSignatureAndAcceptsValidSignature(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	handler := NewGitHubWebhookHandler(store, "topsecret", &GitHubWebhookProcessor{requestCh: make(chan struct{}, 1)})
	payload := []byte(`{"zen":"ship it"}`)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Delivery", "delivery-1")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Delivery", "delivery-2")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", testWebhookSignature("topsecret", payload))
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.Code)
	}
}
