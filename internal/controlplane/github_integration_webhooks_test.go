//go:build integration

package controlplane

import (
	"bytes"
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/proto"
)

func TestPlatformServiceCreateRepoBackedServiceQueuesSyncWithoutBranchLookup(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store, client)
	service := NewPlatformService(store, noopNotifier{}, noopIngress{}, newTestDelivery(store, noopNotifier{}, noopIngress{}, nil), WithGitHubSourceInspection(catalog, client))
	ctx := context.Background()

	projectID := bootstrapProjectAndAgent(t, store, ctx)
	if _, err := service.LinkGitHubRepository(contextWithDelegatedUser("user-1", ""), &platformv1.LinkGitHubRepositoryRequest{
		ProjectId:             projectID,
		RepositorySelector:    "public/hello",
		GithubUserAccessToken: "user-token",
	}); err != nil {
		t.Fatalf("LinkGitHubRepository: %v", err)
	}
	branchHitsBeforeCreate := server.branchHeadHits()
	resp, err := service.CreateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateServiceRequest{
		EnvironmentId: productionEnvironmentID(t, store, projectID),
		Service: &platformv1.ServiceInput{
			Name: "web",
			Spec: repositoryServiceSpec(&platformv1.ServiceRuntime{CpuMillis: 250, MemoryMebibytes: 256, Ports: runtimePortsFromInts([]int32{8080})}, &platformv1.ServiceSourceSpec{
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
	if resp.GetLatestBuild().GetBuildId() != "" {
		t.Fatalf("expected no synchronous build in create response, got %+v", resp.GetLatestBuild())
	}
	if got := server.branchHeadHits(); got != branchHitsBeforeCreate {
		t.Fatalf("expected no branch head lookup during create, got %d new calls", got-branchHitsBeforeCreate)
	}
	if got := countSourceWorkItems(t, store, ctx, deliverycore.SourceWorkKindSourceSpecChanged); got != 0 {
		t.Fatalf("expected staged service to queue no work, got %d", got)
	}
	if _, err := service.ReleaseEnvironment(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.ReleaseEnvironmentRequest{
		EnvironmentId: productionEnvironmentID(t, store, projectID),
	}); err != nil {
		t.Fatalf("ReleaseEnvironment: %v", err)
	}
	if got := countSourceWorkItems(t, store, ctx, deliverycore.SourceWorkKindSourceSpecChanged); got != 1 {
		t.Fatalf("expected release to queue 1 sync item, got %d", got)
	}
}

func TestPlatformServiceUpdateAndEnvironmentReleaseQueueSyncWithoutBranchLookup(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store, client)
	service := NewPlatformService(store, noopNotifier{}, noopIngress{}, newTestDelivery(store, noopNotifier{}, noopIngress{}, nil), WithGitHubSourceInspection(catalog, client))
	ctx := context.Background()

	projectID, serviceID := createRepoBackedTestService(t, store, ctx, "public/hello", 0, "main")
	if _, err := service.LinkGitHubRepository(contextWithDelegatedUser("user-1", ""), &platformv1.LinkGitHubRepositoryRequest{
		ProjectId:             projectID,
		RepositorySelector:    "public/hello",
		GithubUserAccessToken: "user-token",
	}); err != nil {
		t.Fatalf("LinkGitHubRepository: %v", err)
	}
	branchHitsBeforeMutations := server.branchHeadHits()
	if _, err := store.db.ExecContext(ctx, `DELETE FROM source_work_items`); err != nil {
		t.Fatalf("clear source work items: %v", err)
	}

	updateResp, err := service.UpdateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.UpdateServiceRequest{
		ServiceId: serviceID,
		Service: &platformv1.ServiceUpdate{
			Spec: repositoryServiceSpec(&platformv1.ServiceRuntime{CpuMillis: 250, MemoryMebibytes: 256, Ports: runtimePortsFromInts([]int32{8080})}, &platformv1.ServiceSourceSpec{
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
	if updateResp.GetLatestBuild().GetBuildId() != "" {
		t.Fatalf("expected no synchronous build on source update, got %+v", updateResp.GetLatestBuild())
	}
	if got := countSourceWorkItems(t, store, ctx, deliverycore.SourceWorkKindSourceSpecChanged); got != 0 {
		t.Fatalf("expected source update to remain staged until deployment, got %d queued items", got)
	}

	if _, err := store.db.ExecContext(ctx, `DELETE FROM source_work_items`); err != nil {
		t.Fatalf("clear source work items: %v", err)
	}
	statusResp, err := service.ReleaseEnvironment(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.ReleaseEnvironmentRequest{
		EnvironmentId: productionEnvironmentID(t, store, projectID),
	})
	if err != nil {
		t.Fatalf("ReleaseEnvironment: %v", err)
	}
	if len(statusResp.GetServices()) != 1 {
		t.Fatalf("expected one deployed service, got %d", len(statusResp.GetServices()))
	}
	if statusResp.GetServices()[0].GetService().GetLatestBuild().GetBuildId() != "" {
		t.Fatalf("expected no synchronous build on deploy, got %+v", statusResp.GetServices()[0].GetService().GetLatestBuild())
	}
	if got := server.branchHeadHits(); got != branchHitsBeforeMutations {
		t.Fatalf("expected no branch head lookup in update/deploy, got %d new calls", got-branchHitsBeforeMutations)
	}
	if got := countSourceWorkItems(t, store, ctx, deliverycore.SourceWorkKindSourceSpecChanged); got != 1 {
		t.Fatalf("expected 1 queued sync item after release, got %d", got)
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
	coordinator := NewGitHubCoordinator(store, testDelivery(store).Delivery, catalog, client, 5*time.Minute)
	reconciler := NewGitHubReconciler(store, coordinator, time.Minute, time.Minute, time.Minute)
	ctx := context.Background()

	projectID := bootstrapProjectAndAgent(t, store, ctx)
	linkTestProjectRepository(t, store, catalog, projectID, "public/hello")
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projectID), "web", repositoryServiceSpec(
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
	status, _, err := store.serviceStatus(ctx, "user-1", service.ID)
	if err != nil {
		t.Fatalf("serviceStatus: %v", err)
	}
	if status.LatestBuild == nil || status.LatestBuild.GetCommitSha() != "commit-public-main" {
		t.Fatalf("expected queued build for public main, got %+v", status.LatestBuild)
	}
	if status.LatestBuild.GetCommitMessage() != "Public main commit" {
		t.Fatalf("expected synced build commit message to be persisted, got %+v", status.LatestBuild)
	}
	if status.LatestBuild.GetCommitAuthor() != "Octocat" {
		t.Fatalf("expected synced build commit author to be persisted, got %+v", status.LatestBuild)
	}

	if _, err := store.enqueueSourceWorkItem(ctx, deliverycore.SourceWorkItemRecord{
		Kind:           deliverycore.SourceWorkKindSourceSpecChanged,
		IdempotencyKey: fmt.Sprintf("%s:%s:%d", deliverycore.SourceWorkKindSourceSpecChanged, service.ID, service.SpecRevision),
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
	if count != 2 {
		t.Fatalf("expected exactly 2 builds after sync retry, got %d", count)
	}
}

func TestGitHubSyncSameRepositoryUsesEnvironmentSpecificTrackedRefs(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store, client)
	coordinator := NewGitHubCoordinator(store, testDelivery(store).Delivery, catalog, client, 5*time.Minute)
	reconciler := NewGitHubReconciler(store, coordinator, time.Minute, time.Minute, time.Minute)
	ctx := context.Background()

	projectID := bootstrapProjectAndAgent(t, store, ctx)
	linkTestProjectRepository(t, store, catalog, projectID, "public/hello")
	mainSpec := repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "public/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	)
	productionID := productionEnvironmentID(t, store, projectID)
	staging, err := store.createEnvironment(ctx, "user-1", projectID, "Staging")
	if err != nil {
		t.Fatalf("create staging environment: %v", err)
	}
	first, err := createService(ctx, store, "user-1", productionID, "web", mainSpec, "node-1")
	if err != nil {
		t.Fatalf("createService(first): %v", err)
	}
	releaseSpec := proto.Clone(mainSpec).(*platformv1.ServiceSpec)
	releaseSpec.GetSource().GetSourceSpec().TrackedRef = "release"
	second, err := createService(ctx, store, "user-1", staging.ID, "web", releaseSpec, "node-1")
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

	for _, item := range []struct {
		service deliverycore.ServiceRecord
		commit  string
	}{{first, "commit-public-main"}, {second, "commit-public-release"}} {
		service := item.service
		status, _, err := store.serviceStatus(ctx, "user-1", service.ID)
		if err != nil {
			t.Fatalf("serviceStatus(%s): %v", service.Name, err)
		}
		if status.LatestBuild == nil || status.LatestBuild.GetCommitSha() != item.commit {
			t.Fatalf("expected %s build for environment %s, got %+v", item.commit, service.EnvironmentID, status.LatestBuild)
		}
	}
	mainBindings, err := store.sourceBindingsForGitHubRepositoryAndRef(ctx, "1", "main")
	if err != nil || len(mainBindings) != 1 || mainBindings[0].ServiceID != first.ID {
		t.Fatalf("main matched the wrong environment services: %#v: %v", mainBindings, err)
	}
	releaseBindings, err := store.sourceBindingsForGitHubRepositoryAndRef(ctx, "1", "release")
	if err != nil || len(releaseBindings) != 1 || releaseBindings[0].ServiceID != second.ID {
		t.Fatalf("release matched the wrong environment services: %#v: %v", releaseBindings, err)
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
	coordinator := NewGitHubCoordinator(store, testDelivery(store).Delivery, catalog, client, 5*time.Minute)
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
	if got := countSourceWorkItems(t, store, ctx, deliverycore.SourceWorkKindRevisionObserved); got != 1 {
		t.Fatalf("expected one queued build command after duplicate push, got %d", got)
	}

	installationPayload := []byte(`{
		"action":"created",
		"installation":{"id":9,"target_type":"Organization","account":{"login":"octo","type":"Organization"}}
	}`)
	if err := processor.processInstallationEvent(ctx, installationPayload); err != nil {
		t.Fatalf("processInstallationEvent: %v", err)
	}
	if got := countSourceWorkItems(t, store, ctx, deliverycore.SourceWorkKindProviderAccessChanged); got != 1 {
		t.Fatalf("expected one queued refresh command, got %d", got)
	}
}

func TestPushWebhookPersistsCommitMetadataOnQueuedWorkItem(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store, client)
	coordinator := NewGitHubCoordinator(store, testDelivery(store).Delivery, catalog, client, 5*time.Minute)
	processor := NewGitHubWebhookProcessor(store, coordinator)
	ctx := context.Background()

	_, _ = createRepoBackedTestService(t, store, ctx, "private/secret", 7, "main")

	pushPayload := []byte(`{
		"ref":"refs/heads/main",
		"after":"commit-123",
		"head_commit":{
			"message":"Persist dashboard deployment history",
			"author":{"name":"Alice"},
			"committer":{"name":"Bob"}
		},
		"repository":{"id":2,"name":"secret","full_name":"private/secret","owner":{"login":"private"}},
		"installation":{"id":7}
	}`)
	if err := processor.processPushEvent(ctx, pushPayload); err != nil {
		t.Fatalf("processPushEvent: %v", err)
	}

	var commitMessage, commitAuthor string
	if err := store.db.QueryRowContext(ctx,
		`SELECT commit_message, commit_author
		   FROM source_work_items
		  WHERE kind = $1
		  ORDER BY created_at DESC
		  LIMIT 1`,
		deliverycore.SourceWorkKindRevisionObserved,
	).Scan(&commitMessage, &commitAuthor); err != nil {
		t.Fatalf("query queued source work item: %v", err)
	}
	if commitMessage != "Persist dashboard deployment history" {
		t.Fatalf("expected queued work item commit message, got %q", commitMessage)
	}
	if commitAuthor != "Alice" {
		t.Fatalf("expected queued work item commit author, got %q", commitAuthor)
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
	coordinator := NewGitHubCoordinator(store, testDelivery(store).Delivery, catalog, client, time.Minute)
	reconciler := NewGitHubReconciler(store, coordinator, time.Minute, time.Minute, time.Minute)
	ctx := context.Background()

	if _, err := store.enqueueSourceWorkItem(ctx, deliverycore.SourceWorkItemRecord{
		Kind:                    deliverycore.SourceWorkKindProviderAccessChanged,
		IdempotencyKey:          "bootstrap-refresh-7",
		Provider:                "github",
		ProviderScopeExternalID: scopeExternalID(7),
	}); err != nil {
		t.Fatalf("enqueueSourceWorkItem: %v", err)
	}
	claimed, err := claimNextSourceWorkItem(ctx, store, "processor-1", 0)
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
	requeued, err := claimNextSourceWorkItem(ctx, store, "processor-2", 0)
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
