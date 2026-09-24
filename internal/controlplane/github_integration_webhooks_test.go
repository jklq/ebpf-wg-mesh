//go:build integration

package controlplane

import (
	"bytes"
	"context"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/durablework"
	"ebof-wg-mesh/internal/controlplane/source"
	"errors"
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
	catalog := NewGitHubCatalog(store.source, client)
	service := NewPlatformService(store.platform(), noopNotifier{}, noopIngress{}, newTestDelivery(store, noopNotifier{}, noopIngress{}, nil), WithGitHubSourceInspection(catalog, client, authz.NewAuthorizer(store.db)))
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
				BuildRecipe:        &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."},
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
	if got := countSourceWorkItems(t, store, ctx, source.SourceWorkKindSourceSpecChanged); got != 0 {
		t.Fatalf("expected staged service to queue no work, got %d", got)
	}
	if _, err := service.ReleaseEnvironment(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.ReleaseEnvironmentRequest{
		EnvironmentId: productionEnvironmentID(t, store, projectID),
	}); err != nil {
		t.Fatalf("ReleaseEnvironment: %v", err)
	}
	if got := countSourceWorkItems(t, store, ctx, source.SourceWorkKindSourceSpecChanged); got != 1 {
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
	catalog := NewGitHubCatalog(store.source, client)
	service := NewPlatformService(store.platform(), noopNotifier{}, noopIngress{}, newTestDelivery(store, noopNotifier{}, noopIngress{}, nil), WithGitHubSourceInspection(catalog, client, authz.NewAuthorizer(store.db)))
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
	if _, err := store.db.ExecContext(ctx, `DELETE FROM durable_work_items`); err != nil {
		t.Fatalf("clear source work items: %v", err)
	}

	updateResp, err := service.UpdateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.UpdateServiceRequest{
		ServiceId: serviceID,
		Service: &platformv1.ServiceUpdate{
			Spec: repositoryServiceSpec(&platformv1.ServiceRuntime{CpuMillis: 250, MemoryMebibytes: 256, Ports: runtimePortsFromInts([]int32{8080})}, &platformv1.ServiceSourceSpec{
				Provider:           "github",
				RepositorySelector: "public/hello",
				TrackedRef:         "release",
				BuildRecipe:        &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."},
			}),
		},
	})
	if err != nil {
		t.Fatalf("UpdateService: %v", err)
	}
	if updateResp.GetLatestBuild().GetBuildId() != "" {
		t.Fatalf("expected no synchronous build on source update, got %+v", updateResp.GetLatestBuild())
	}
	if got := countSourceWorkItems(t, store, ctx, source.SourceWorkKindSourceSpecChanged); got != 0 {
		t.Fatalf("expected source update to remain staged until deployment, got %d queued items", got)
	}

	if _, err := store.db.ExecContext(ctx, `DELETE FROM durable_work_items`); err != nil {
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
	if got := countSourceWorkItems(t, store, ctx, source.SourceWorkKindSourceSpecChanged); got != 1 {
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
	catalog := NewGitHubCatalog(store.source, client)
	coordinator := NewGitHubCoordinator(store.source, store.source.Work(), testDelivery(store).Delivery, catalog, client, 5*time.Minute)
	reconciler := NewGitHubReconciler(store.source, coordinator, time.Minute)
	ctx := context.Background()

	projectID := bootstrapProjectAndAgent(t, store, ctx)
	linkTestProjectRepository(t, store, catalog, projectID, "public/hello")
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projectID), "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "public/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	for i := 0; i < 3; i++ {
		processed, err := reconciler.ProcessNext(ctx)
		if err != nil {
			t.Fatalf("processNext(%d): %v", i, err)
		}
		if !processed {
			break
		}
	}
	status, _, err := store.reads.ServiceStatus(ctx, testUser("user-1"), service.ID)
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

	if _, err := store.source.Work().Enqueue(ctx, source.SourceSpecChangedParams(service.ID, service.SpecRevision, false)); err != nil {
		t.Fatalf("Enqueue(sync retry): %v", err)
	}
	for i := 0; i < 3; i++ {
		processed, err := reconciler.ProcessNext(ctx)
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
	catalog := NewGitHubCatalog(store.source, client)
	coordinator := NewGitHubCoordinator(store.source, store.source.Work(), testDelivery(store).Delivery, catalog, client, 5*time.Minute)
	reconciler := NewGitHubReconciler(store.source, coordinator, time.Minute)
	ctx := context.Background()

	projectID := bootstrapProjectAndAgent(t, store, ctx)
	linkTestProjectRepository(t, store, catalog, projectID, "public/hello")
	mainSpec := repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "public/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	)
	productionID := productionEnvironmentID(t, store, projectID)
	staging, err := store.catalog.createEnvironment(ctx, testUser("user-1"), projectID, "Staging")
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
		processed, err := reconciler.ProcessNext(ctx)
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
		status, _, err := store.reads.ServiceStatus(ctx, testUser("user-1"), service.ID)
		if err != nil {
			t.Fatalf("ServiceStatus(%s): %v", service.Name, err)
		}
		if status.LatestBuild == nil || status.LatestBuild.GetCommitSha() != item.commit {
			t.Fatalf("expected %s build for environment %s, got %+v", item.commit, service.EnvironmentID, status.LatestBuild)
		}
	}
	mainBindings, err := store.source.SourceBindingsForGitHubRepositoryAndRef(ctx, "1", "main")
	if err != nil || len(mainBindings) != 1 || mainBindings[0].ServiceID != first.ID {
		t.Fatalf("main matched the wrong environment services: %#v: %v", mainBindings, err)
	}
	releaseBindings, err := store.source.SourceBindingsForGitHubRepositoryAndRef(ctx, "1", "release")
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
	catalog := NewGitHubCatalog(store.source, client)
	coordinator := NewGitHubCoordinator(store.source, store.source.Work(), testDelivery(store).Delivery, catalog, client, 5*time.Minute)
	processor := NewGitHubWebhookProcessor(store.source, coordinator)
	ctx := context.Background()

	_, serviceID := createRepoBackedTestService(t, store, ctx, "private/secret", 7, "main")

	pushPayload := []byte(`{
		"ref":"refs/heads/main",
		"after":"commit-123",
		"repository":{"id":2,"name":"secret","full_name":"private/secret","owner":{"login":"private"}},
		"installation":{"id":7}
	}`)
	if err := processor.ProcessPushEvent(ctx, pushPayload); err != nil {
		t.Fatalf("processPushEvent(first): %v", err)
	}
	if err := processor.ProcessPushEvent(ctx, pushPayload); err != nil {
		t.Fatalf("processPushEvent(second): %v", err)
	}
	var buildCount int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM build_runs WHERE service_id = $1`, serviceID).Scan(&buildCount); err != nil {
		t.Fatalf("count build runs: %v", err)
	}
	if buildCount != 0 {
		t.Fatalf("expected webhook to queue work, not build directly, got %d build rows", buildCount)
	}
	if got := countSourceWorkItems(t, store, ctx, source.SourceWorkKindRevisionObserved); got != 1 {
		t.Fatalf("expected one queued build command after duplicate push, got %d", got)
	}

	installationPayload := []byte(`{
		"action":"created",
		"installation":{"id":9,"target_type":"Organization","account":{"login":"octo","type":"Organization"}}
	}`)
	if err := processor.ProcessInstallationEvent(ctx, installationPayload); err != nil {
		t.Fatalf("processInstallationEvent: %v", err)
	}
	if got := countSourceWorkItems(t, store, ctx, source.SourceWorkKindProviderAccessChanged); got != 1 {
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
	catalog := NewGitHubCatalog(store.source, client)
	coordinator := NewGitHubCoordinator(store.source, store.source.Work(), testDelivery(store).Delivery, catalog, client, 5*time.Minute)
	processor := NewGitHubWebhookProcessor(store.source, coordinator)
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
	if err := processor.ProcessPushEvent(ctx, pushPayload); err != nil {
		t.Fatalf("processPushEvent: %v", err)
	}

	var commitMessage, commitAuthor string
	if err := store.db.QueryRowContext(ctx,
		`SELECT payload->>'commit_message', payload->>'commit_author'
		   FROM durable_work_items
		  WHERE kind = $1
		  ORDER BY created_at DESC
		  LIMIT 1`,
		source.SourceWorkKindRevisionObserved,
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

func TestGitHubWorkLeaseExpiryAllowsTakeover(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store.source, client)
	coordinator := NewGitHubCoordinator(store.source, store.source.Work(), testDelivery(store).Delivery, catalog, client, time.Minute)
	reconciler := NewGitHubReconciler(store.source, coordinator, time.Minute)
	ctx := context.Background()

	if _, err := store.source.Work().Enqueue(ctx, source.ProviderAccessChangedParams(7)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed, err := coordinator.ClaimNextWorkItem(ctx, "processor-1")
	if err != nil {
		t.Fatalf("ClaimNextWorkItem: %v", err)
	}
	if claimed.ID == "" {
		t.Fatal("expected claimed work item")
	}
	payload, err := source.DecodeWorkPayload(claimed.Payload)
	if err != nil {
		t.Fatalf("DecodeWorkPayload: %v", err)
	}
	if payload.ProviderScopeExternalID != source.ScopeExternalID(7) {
		t.Fatalf("claimed payload scope = %q, want %q", payload.ProviderScopeExternalID, source.ScopeExternalID(7))
	}

	// Bootstrap must leave live leases alone: no recovery scan requeues
	// source work anymore.
	if err := reconciler.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if live, err := coordinator.ClaimNextWorkItem(ctx, "processor-2"); err != nil {
		t.Fatalf("ClaimNextWorkItem(live lease): %v", err)
	} else if live.ID != "" {
		t.Fatal("live lease was handed to a second owner")
	}

	if _, err := store.db.ExecContext(ctx,
		`UPDATE durable_work_items SET lease_expires_at = statement_timestamp() - INTERVAL '1 minute' WHERE id = $1`,
		claimed.ID,
	); err != nil {
		t.Fatalf("expire source work lease: %v", err)
	}
	taken, err := coordinator.ClaimNextWorkItem(ctx, "processor-2")
	if err != nil {
		t.Fatalf("ClaimNextWorkItem(takeover): %v", err)
	}
	if taken.ID != claimed.ID {
		t.Fatalf("takeover claimed %q, want %q", taken.ID, claimed.ID)
	}
	if taken.OwnerEpoch != claimed.OwnerEpoch+1 || taken.OwnerID != "processor-2" {
		t.Fatalf("takeover owner = %s epoch %d, want processor-2 epoch %d", taken.OwnerID, taken.OwnerEpoch, claimed.OwnerEpoch+1)
	}
	if err := coordinator.CompleteWorkItem(ctx, claimed); !errors.Is(err, durablework.ErrLeaseLost) {
		t.Fatalf("stale owner complete = %v, want ErrLeaseLost", err)
	}
	if err := coordinator.CompleteWorkItem(ctx, taken); err != nil {
		t.Fatalf("current owner complete: %v", err)
	}
}

func TestSourceQueueClaimsAreAtomicAndDoNotAdvanceProductJournal(t *testing.T) {
	store, err := openPersistence(config.DatabaseConfig{
		URL: createTestDatabase(t), MaxOpenConns: 4, MaxIdleConns: 4,
	}, testMeshConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	before, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvent, err := store.events.currentGlobalRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	work := store.source.Work()
	if empty, err := work.Claim(ctx, "empty-worker", time.Minute, source.SourceWorkKinds...); err != nil || empty.ID != "" {
		t.Fatalf("empty claim = (%+v, %v)", empty, err)
	}
	inserted, err := work.Enqueue(ctx, durablework.EnqueueParams{
		Kind:         source.SourceWorkKindProviderAccessChanged,
		DedupKey:     "atomic-claim",
		ResourceType: "github_installation",
		ResourceID:   "7",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("source work item was not inserted")
	}
	start := make(chan struct{})
	type claimResult struct {
		record durablework.Record
		err    error
	}
	results := make(chan claimResult, 2)
	for _, worker := range []string{"worker-a", "worker-b"} {
		go func() {
			<-start
			rec, err := work.Claim(ctx, worker, time.Minute, source.SourceWorkKinds...)
			results <- claimResult{record: rec, err: err}
		}()
	}
	close(start)
	claimed := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.record.ID != "" {
			claimed++
		}
	}
	if claimed != 1 {
		var state, ownerID string
		var ownerEpoch int64
		var availableAt, databaseNow time.Time
		if err := store.db.QueryRowContext(ctx, `SELECT state, owner_id, owner_epoch, available_at, statement_timestamp() FROM durable_work_items WHERE dedup_key = 'atomic-claim'`).Scan(&state, &ownerID, &ownerEpoch, &availableAt, &databaseNow); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("claimed work %d times, want once; row state=%s owner=%s epoch=%d available_at=%s database_now=%s", claimed, state, ownerID, ownerEpoch, availableAt, databaseNow)
	}
	after, err := store.journal.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	afterEvent, err := store.events.currentGlobalRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.LogIndex != after.LogIndex || beforeEvent != afterEvent {
		t.Fatalf("coordination changed product signals: head %d->%d event %d->%d", before.LogIndex, after.LogIndex, beforeEvent, afterEvent)
	}
}

func TestGitHubWebhookHandlerRejectsInvalidSignatureAndAcceptsValidSignature(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	handler := NewGitHubWebhookHandler(store.source, "topsecret", noopWebhookProcessor{})
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

// TestRecreatedRefPushBuildsNewHeadWithoutPredecessorChain is the branch
// recreation regression: GitHub sends an all-zero "before" when a tracked
// ref is created or recreated while the binding still stores the
// pre-deletion tip. That predecessor can never be observed, so the push
// must prove currency by fetching the tracked head instead of pending as
// an early successor until its retries run out — and when the fetch shows
// a newer head than the pushed commit, the delivery is stale and must not
// build at all.
func TestRecreatedRefPushBuildsNewHeadWithoutPredecessorChain(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store.source, client)
	coordinator := NewGitHubCoordinator(store.source, store.source.Work(), testDelivery(store).Delivery, catalog, client, 5*time.Minute)
	reconciler := NewGitHubReconciler(store.source, coordinator, time.Minute)
	ctx := context.Background()

	projectID := bootstrapProjectAndAgent(t, store, ctx)
	linkTestProjectRepository(t, store, catalog, projectID, "public/hello")
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projectID), "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "public/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	for i := 0; i < 3; i++ {
		processed, err := reconciler.ProcessNext(ctx)
		if err != nil {
			t.Fatalf("processNext(%d): %v", i, err)
		}
		if !processed {
			break
		}
	}
	binding, err := store.source.SourceBindingByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatalf("SourceBindingByServiceID: %v", err)
	}
	// The binding holds the pre-deletion tip (commit-public-main) as its
	// proven head. The branch is deleted and recreated at another commit;
	// GitHub reports the recreate push with an all-zero before SHA.
	const zeroSHA = "0000000000000000000000000000000000000000"
	server.setBranchHead("/repos/public/hello/git/ref/heads/main", "commit-public-release")
	if err := coordinator.ObserveRepositoryRevision(ctx, "1", "main", "commit-public-release", zeroSHA, "Public release commit", "Octocat Release"); err != nil {
		t.Fatalf("ObserveRepositoryRevision: %v", err)
	}
	processed, err := reconciler.ProcessNext(ctx)
	if err != nil || !processed {
		t.Fatalf("processNext(recreate push) = %v, %v; the zero predecessor must not pend the push until its retries run out", processed, err)
	}
	status, _, err := store.reads.ServiceStatus(ctx, testUser("user-1"), service.ID)
	if err != nil {
		t.Fatalf("ServiceStatus: %v", err)
	}
	if status.LatestBuild == nil || status.LatestBuild.GetCommitSha() != "commit-public-release" {
		t.Fatalf("expected recreated-ref build for commit-public-release, got %+v", status.LatestBuild)
	}
	head, err := store.source.SourceBindingHeadCommit(ctx, binding.ID)
	if err != nil || head != "commit-public-release" {
		t.Fatalf("proven head = %q, %v; the recreated ref head must advance the binding", head, err)
	}

	// A recreate push whose commit is no longer the tracked head is
	// stale: a newer push already moved the ref on, so it must complete
	// without building and without moving the head.
	server.setBranchHead("/repos/public/hello/git/ref/heads/main", "commit-public-release")
	if err := coordinator.ObserveRepositoryRevision(ctx, "1", "main", "commit-stale-recreate", zeroSHA, "Stale recreate", "Octocat"); err != nil {
		t.Fatalf("ObserveRepositoryRevision(stale): %v", err)
	}
	processed, err = reconciler.ProcessNext(ctx)
	if err != nil || !processed {
		t.Fatalf("processNext(stale recreate) = %v, %v, want clean completion", processed, err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM build_runs WHERE service_id = $1`, service.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("build_runs rows = %d, want 2 (the stale recreate push must not build)", count)
	}
	head, err = store.source.SourceBindingHeadCommit(ctx, binding.ID)
	if err != nil || head != "commit-public-release" {
		t.Fatalf("proven head = %q, %v; the stale recreate push must not move it", head, err)
	}

	// A stale create push arriving before the binding has any recorded
	// head must not establish one either: history-only observations never
	// become the head that a later current push would have to chain
	// against. (Clearing the head stands in for a binding that has not
	// proven one yet.)
	if _, err := store.db.ExecContext(ctx, `UPDATE source_bindings SET head_commit_sha = '' WHERE id = $1`, binding.ID); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.ObserveRepositoryRevision(ctx, "1", "main", "commit-orphan-create", zeroSHA, "Orphan create", "Octocat"); err != nil {
		t.Fatalf("ObserveRepositoryRevision(orphan create): %v", err)
	}
	processed, err = reconciler.ProcessNext(ctx)
	if err != nil || !processed {
		t.Fatalf("processNext(orphan create) = %v, %v, want clean completion", processed, err)
	}
	head, err = store.source.SourceBindingHeadCommit(ctx, binding.ID)
	if err != nil || head != "" {
		t.Fatalf("proven head = %q, %v; a stale create event must never establish the head", head, err)
	}
	var recorded int
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM source_revisions WHERE service_id = $1 AND commit_sha = 'commit-orphan-create'`, service.ID).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 1 {
		t.Fatalf("stale create history rows = %d, want the observation recorded without becoming the head", recorded)
	}
}

func TestPushWithUnobservedPredecessorFetchesTrackedHeadInsteadOfPending(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store.source, client)
	coordinator := NewGitHubCoordinator(store.source, store.source.Work(), testDelivery(store).Delivery, catalog, client, 5*time.Minute)
	reconciler := NewGitHubReconciler(store.source, coordinator, time.Minute)
	ctx := context.Background()

	projectID := bootstrapProjectAndAgent(t, store, ctx)
	linkTestProjectRepository(t, store, catalog, projectID, "public/hello")
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projectID), "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "public/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	for i := 0; i < 3; i++ {
		processed, err := reconciler.ProcessNext(ctx)
		if err != nil {
			t.Fatalf("processNext(%d): %v", i, err)
		}
		if !processed {
			break
		}
	}
	binding, err := store.source.SourceBindingByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatalf("SourceBindingByServiceID: %v", err)
	}

	// A push names a predecessor this binding has never observed — it is
	// not zero (a recreated ref), just outside the recorded history. The
	// successor must not pend until the binding expires: the tracked
	// head is fetched and the commit builds because it is still current.
	server.setBranchHead("/repos/public/hello/git/ref/heads/main", "commit-public-release")
	if err := coordinator.ObserveRepositoryRevision(ctx, "1", "main", "commit-public-release", "commit-never-observed", "Successor commit", "Octocat"); err != nil {
		t.Fatalf("ObserveRepositoryRevision: %v", err)
	}
	processed, err := reconciler.ProcessNext(ctx)
	if err != nil || !processed {
		t.Fatalf("processNext(push) = %v, %v, want clean completion", processed, err)
	}
	processed, err = reconciler.ProcessNext(ctx)
	if err != nil || !processed {
		t.Fatalf("processNext(tracked-head sync) = %v, %v; an unknown predecessor must fetch the tracked head now, not wait for the binding to expire", processed, err)
	}
	status, _, err := store.reads.ServiceStatus(ctx, testUser("user-1"), service.ID)
	if err != nil {
		t.Fatalf("ServiceStatus: %v", err)
	}
	if status.LatestBuild == nil || status.LatestBuild.GetCommitSha() != "commit-public-release" {
		t.Fatalf("expected build for commit-public-release, got %+v", status.LatestBuild)
	}
	head, err := store.source.SourceBindingHeadCommit(ctx, binding.ID)
	if err != nil || head != "commit-public-release" {
		t.Fatalf("proven head = %q, %v; the tracked head must advance the binding", head, err)
	}

	// The same shape of push for a commit that is no longer the tracked
	// head must not build that commit: a newer push already moved the ref
	// on, and the fetch-verified sync queues only the commit still
	// current (a fresh attempt at the head, superseding its predecessor).
	if err := coordinator.ObserveRepositoryRevision(ctx, "1", "main", "commit-public-main", "commit-never-observed", "Superseded push", "Octocat"); err != nil {
		t.Fatalf("ObserveRepositoryRevision(stale): %v", err)
	}
	for i := 0; i < 2; i++ {
		processed, err = reconciler.ProcessNext(ctx)
		if err != nil || !processed {
			t.Fatalf("processNext(stale %d) = %v, %v, want clean completion", i, processed, err)
		}
	}
	var count int
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM build_runs WHERE service_id = $1 AND commit_sha = 'commit-public-main'`, service.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("builds of the superseded commit = %d, want only its original one", count)
	}
	head, err = store.source.SourceBindingHeadCommit(ctx, binding.ID)
	if err != nil || head != "commit-public-release" {
		t.Fatalf("proven head = %q, %v; the superseded push must not move it", head, err)
	}
}

func TestRecreatedBranchSuccessorBuildsViaTrackedHeadSync(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store.source, client)
	coordinator := NewGitHubCoordinator(store.source, store.source.Work(), testDelivery(store).Delivery, catalog, client, 5*time.Minute)
	reconciler := NewGitHubReconciler(store.source, coordinator, time.Minute)
	ctx := context.Background()

	projectID := bootstrapProjectAndAgent(t, store, ctx)
	linkTestProjectRepository(t, store, catalog, projectID, "public/hello")
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projectID), "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "public/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	for i := 0; i < 3; i++ {
		processed, err := reconciler.ProcessNext(ctx)
		if err != nil {
			t.Fatalf("processNext(%d): %v", i, err)
		}
		if !processed {
			break
		}
	}
	binding, err := store.source.SourceBindingByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatalf("SourceBindingByServiceID: %v", err)
	}

	// The branch is recreated and advances past its own tip before the
	// recreate push is processed: the recreate (zero predecessor) is
	// recorded as history and can never hold the head.
	const zeroSHA = "0000000000000000000000000000000000000000"
	server.setBranchHead("/repos/public/hello/git/ref/heads/main", "commit-public-release")
	if err := coordinator.ObserveRepositoryRevision(ctx, "1", "main", "commit-history-phantom", zeroSHA, "Recreated tip", "Octocat"); err != nil {
		t.Fatalf("ObserveRepositoryRevision(recreate): %v", err)
	}
	if processed, err := reconciler.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("processNext(recreate) = %v, %v, want clean completion", processed, err)
	}
	if head, err := store.source.SourceBindingHeadCommit(ctx, binding.ID); err != nil || head != "commit-public-main" {
		t.Fatalf("proven head = %q, %v; a history-only observation must not hold the head", head, err)
	}

	// The successor push names that recorded-but-never-head commit as its
	// predecessor, so the chain cannot prove currency — and that is not
	// proof of staleness. The tracked head must be reconciled now and
	// the successor builds because it is still current.
	if err := coordinator.ObserveRepositoryRevision(ctx, "1", "main", "commit-public-release", "commit-history-phantom", "Release successor", "Octocat"); err != nil {
		t.Fatalf("ObserveRepositoryRevision(successor): %v", err)
	}
	processed, err := reconciler.ProcessNext(ctx)
	if err != nil || !processed {
		t.Fatalf("processNext(successor push) = %v, %v, want clean completion", processed, err)
	}
	processed, err = reconciler.ProcessNext(ctx)
	if err != nil || !processed {
		t.Fatalf("processNext(tracked-head sync) = %v, %v; the successor must not be dropped until the periodic binding refresh", processed, err)
	}
	status, _, err := store.reads.ServiceStatus(ctx, testUser("user-1"), service.ID)
	if err != nil {
		t.Fatalf("ServiceStatus: %v", err)
	}
	if status.LatestBuild == nil || status.LatestBuild.GetCommitSha() != "commit-public-release" {
		t.Fatalf("expected build for commit-public-release, got %+v", status.LatestBuild)
	}
	head, err := store.source.SourceBindingHeadCommit(ctx, binding.ID)
	if err != nil || head != "commit-public-release" {
		t.Fatalf("proven head = %q, %v; the reconciled tracked head must advance the binding", head, err)
	}
	var phantomBuilds int
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM build_runs WHERE service_id = $1 AND commit_sha = 'commit-history-phantom'`, service.ID).Scan(&phantomBuilds); err != nil {
		t.Fatal(err)
	}
	if phantomBuilds != 0 {
		t.Fatalf("history-only commit built %d times; it never held the tracked head", phantomBuilds)
	}
}
