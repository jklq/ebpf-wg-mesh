//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestEnvironmentAutoDeployDefaultsAndAuthorization(t *testing.T) {
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
	if !production.AutoDeploy {
		t.Fatalf("expected production auto-deploy on by default, got %#v", production)
	}
	staging, err := store.catalog.createEnvironment(ctx, testUser("owner"), project.ID, "Staging")
	if err != nil {
		t.Fatal(err)
	}
	if !staging.AutoDeploy {
		t.Fatalf("expected non-production auto-deploy on by default, got %#v", staging)
	}

	disabled, err := store.catalog.updateEnvironmentAutoDeploy(ctx, testUser("owner"), staging.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.AutoDeploy {
		t.Fatalf("expected auto-deploy override off, got %#v", disabled)
	}
	enabled, err := store.catalog.updateEnvironmentAutoDeploy(ctx, testUser("owner"), staging.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled.AutoDeploy {
		t.Fatalf("expected auto-deploy override on, got %#v", enabled)
	}
	productionDisabled, err := store.catalog.updateEnvironmentAutoDeploy(ctx, testUser("owner"), production.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if productionDisabled.AutoDeploy {
		t.Fatalf("expected production auto-deploy override off, got %#v", productionDisabled)
	}
	productionEnabled, err := store.catalog.updateEnvironmentAutoDeploy(ctx, testUser("owner"), production.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !productionEnabled.AutoDeploy {
		t.Fatalf("expected production auto-deploy override on, got %#v", productionEnabled)
	}

	if _, err := store.db.ExecContext(ctx, `INSERT INTO project_memberships(user_id, project_id, role)
		VALUES ('editor', $1, 'editor'), ('viewer', $1, 'viewer')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.updateEnvironmentAutoDeploy(ctx, testUser("editor"), staging.ID, false); err != nil {
		t.Fatalf("editor could not update auto-deploy: %v", err)
	}
	if _, err := store.catalog.updateEnvironmentAutoDeploy(ctx, testUser("viewer"), staging.ID, true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("viewer updated auto-deploy: %v", err)
	}
	environments, err := store.catalog.listEnvironments(ctx, testUser("viewer"), project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, environment := range environments {
		switch environment.ID {
		case production.ID:
			if !environment.AutoDeploy {
				t.Fatalf("expected listed production auto-deploy on, got %#v", environment)
			}
		case staging.ID:
			if environment.AutoDeploy {
				t.Fatalf("expected listed staging auto-deploy off, got %#v", environment)
			}
		}
	}

	if autoDeploy, err := store.source.EnvironmentAutoDeploy(ctx, production.ID); err != nil || !autoDeploy {
		t.Fatalf("source store production auto-deploy = %v, %v", autoDeploy, err)
	}
	if autoDeploy, err := store.source.EnvironmentAutoDeploy(ctx, staging.ID); err != nil || autoDeploy {
		t.Fatalf("source store staging auto-deploy = %v, %v", autoDeploy, err)
	}
	if _, err := store.source.EnvironmentAutoDeploy(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected missing environment lookup to fail, got %v", err)
	}
}

func TestGitHubPushQueuesBuildWhenAutoDeployOn(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store.source, client)
	coordinator := NewGitHubCoordinator(store.source, store.source.Work(), testDelivery(store).Delivery, catalog, client, 5*time.Minute)
	reconciler := NewGitHubReconciler(store.source, coordinator, time.Minute, time.Minute)
	processor := NewGitHubWebhookProcessor(store.source, coordinator)
	ctx := context.Background()

	projectID := bootstrapProjectAndAgent(t, store, ctx)
	linkTestProjectRepository(t, store, catalog, projectID, "public/hello")
	staging, err := store.catalog.createEnvironment(ctx, testUser("user-1"), projectID, "Staging")
	if err != nil {
		t.Fatalf("create staging environment: %v", err)
	}
	if !staging.AutoDeploy {
		t.Fatalf("expected staging auto-deploy on, got %#v", staging)
	}
	service, err := createService(ctx, store, "user-1", staging.ID, "web", repositoryServiceSpec(
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
	if _, _, err := releaseEnvironmentForTest(ctx, store, "user-1", staging.ID); err != nil {
		t.Fatalf("releaseEnvironment: %v", err)
	}
	drainSourceWork(t, reconciler, ctx)

	buildsBefore := countBuildRuns(t, store, ctx, service.ID)
	deploymentsBefore := countDeployments(t, store, ctx, service.ID)
	if buildsBefore != 1 {
		t.Fatalf("expected initial sync to queue 1 build, got %d", buildsBefore)
	}

	pushPayload := []byte(`{
		"ref":"refs/heads/main",
		"after":"commit-public-main",
		"head_commit":{"message":"Public main commit","author":{"name":"Octocat"}},
		"repository":{"id":1,"name":"hello","full_name":"public/hello","owner":{"login":"public"}},
		"installation":{"id":0}
	}`)
	if err := processor.ProcessPushEvent(ctx, pushPayload); err != nil {
		t.Fatalf("processPushEvent: %v", err)
	}
	drainSourceWork(t, reconciler, ctx)

	if got := countBuildRuns(t, store, ctx, service.ID); got != buildsBefore+1 {
		t.Fatalf("expected push to queue a build, got %d builds (was %d)", got, buildsBefore)
	}
	if got := countDeployments(t, store, ctx, service.ID); got != deploymentsBefore+1 {
		t.Fatalf("expected push to queue a deployment, got %d deployments (was %d)", got, deploymentsBefore)
	}
	var commitSHA string
	if err := store.db.QueryRowContext(ctx, `SELECT commit_sha FROM build_runs WHERE service_id = $1 ORDER BY queued_at DESC, id DESC LIMIT 1`, service.ID).Scan(&commitSHA); err != nil {
		t.Fatalf("latest build commit: %v", err)
	}
	if commitSHA != "commit-public-main" {
		t.Fatalf("expected queued build for pushed commit, got %q", commitSHA)
	}
}

func TestGitHubPushRecordsRevisionWithoutBuildWhenAutoDeployOff(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store.source, client)
	coordinator := NewGitHubCoordinator(store.source, store.source.Work(), testDelivery(store).Delivery, catalog, client, 5*time.Minute)
	reconciler := NewGitHubReconciler(store.source, coordinator, time.Minute, time.Minute)
	processor := NewGitHubWebhookProcessor(store.source, coordinator)
	ctx := context.Background()

	projectID := bootstrapProjectAndAgent(t, store, ctx)
	linkTestProjectRepository(t, store, catalog, projectID, "public/hello")
	productionID := productionEnvironmentID(t, store, projectID)
	production, err := store.catalog.productionEnvironmentByProjectInternal(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if !production.AutoDeploy {
		t.Fatalf("expected production auto-deploy on by default, got %#v", production)
	}
	if _, err := store.catalog.updateEnvironmentAutoDeploy(ctx, testUser("user-1"), production.ID, false); err != nil {
		t.Fatalf("disable production auto-deploy: %v", err)
	}
	service, err := createService(ctx, store, "user-1", productionID, "web", repositoryServiceSpec(
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
	if _, _, err := releaseEnvironmentForTest(ctx, store, "user-1", productionID); err != nil {
		t.Fatalf("releaseEnvironment: %v", err)
	}
	drainSourceWork(t, reconciler, ctx)
	if got := countBuildRuns(t, store, ctx, service.ID); got != 1 {
		t.Fatalf("expected manual release to queue the initial build while off, got %d", got)
	}
	deploymentsBefore := countDeployments(t, store, ctx, service.ID)

	pushPayload := []byte(`{
		"ref":"refs/heads/main",
		"after":"commit-manual-9",
		"head_commit":{"message":"Manual revision","author":{"name":"Octocat"}},
		"repository":{"id":1,"name":"hello","full_name":"public/hello","owner":{"login":"public"}},
		"installation":{"id":0}
	}`)
	if err := processor.ProcessPushEvent(ctx, pushPayload); err != nil {
		t.Fatalf("processPushEvent: %v", err)
	}
	drainSourceWork(t, reconciler, ctx)

	var recorded string
	if err := store.db.QueryRowContext(ctx, `SELECT commit_sha FROM source_revisions WHERE service_id = $1 ORDER BY observed_at DESC, created_at DESC, id DESC LIMIT 1`, service.ID).Scan(&recorded); err != nil {
		t.Fatalf("latest source revision: %v", err)
	}
	if recorded != "commit-manual-9" {
		t.Fatalf("expected pushed revision to be recorded, got %q", recorded)
	}
	if got := countBuildRuns(t, store, ctx, service.ID); got != 1 {
		t.Fatalf("expected no build while auto-deploy is off, got %d builds", got)
	}
	if got := countDeployments(t, store, ctx, service.ID); got != deploymentsBefore {
		t.Fatalf("expected no rollout while auto-deploy is off, got %d deployments (was %d)", got, deploymentsBefore)
	}
	if got := server.pathHits("/repos/public/hello/tarball/commit-manual-9"); got != 0 {
		t.Fatalf("expected no snapshot fetch while auto-deploy is off, got %d fetches", got)
	}
	status, _, err := store.reads.ServiceStatus(ctx, testUser("user-1"), service.ID)
	if err != nil {
		t.Fatalf("serviceStatus: %v", err)
	}
	if got := status.SourceSummary.GetSourceState().GetLatestRevision().GetCommitSha(); got != "commit-manual-9" {
		t.Fatalf("expected service summary to expose the recorded revision, got %q", got)
	}

	released, _, err := releaseEnvironmentForTest(ctx, store, "user-1", productionID)
	if err != nil {
		t.Fatalf("manual releaseEnvironment: %v", err)
	}
	if len(released) != 1 || released[0].ID != service.ID {
		t.Fatalf("expected manual release to pick up the recorded revision, got %d services", len(released))
	}
	drainSourceWork(t, reconciler, ctx)
	if got := countBuildRuns(t, store, ctx, service.ID); got != 2 {
		t.Fatalf("expected manual deploy to queue a build for the recorded revision, got %d builds", got)
	}
	var manualCommit string
	if err := store.db.QueryRowContext(ctx, `SELECT commit_sha FROM build_runs WHERE service_id = $1 ORDER BY queued_at DESC, id DESC LIMIT 1`, service.ID).Scan(&manualCommit); err != nil {
		t.Fatalf("manual build commit: %v", err)
	}
	if manualCommit != "commit-public-main" {
		t.Fatalf("expected manual deploy to build the tracked head, got %q", manualCommit)
	}
}

func TestGitHubStaleBindingHoldsBuildWhenAutoDeployOff(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	server := newTestGitHubServer(t, nil)
	client, err := NewGitHubClient(server.config())
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	catalog := NewGitHubCatalog(store.source, client)
	coordinator := NewGitHubCoordinator(store.source, store.source.Work(), testDelivery(store).Delivery, catalog, client, 5*time.Minute)
	reconciler := NewGitHubReconciler(store.source, coordinator, time.Minute, time.Minute)
	processor := NewGitHubWebhookProcessor(store.source, coordinator)
	ctx := context.Background()

	projectID := bootstrapProjectAndAgent(t, store, ctx)
	linkTestProjectRepository(t, store, catalog, projectID, "public/hello")
	productionID := productionEnvironmentID(t, store, projectID)
	production, err := store.catalog.productionEnvironmentByProjectInternal(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if !production.AutoDeploy {
		t.Fatalf("expected production auto-deploy on by default, got %#v", production)
	}
	if _, err := store.catalog.updateEnvironmentAutoDeploy(ctx, testUser("user-1"), production.ID, false); err != nil {
		t.Fatalf("disable production auto-deploy: %v", err)
	}
	service, err := createService(ctx, store, "user-1", productionID, "web", repositoryServiceSpec(
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
	if _, _, err := releaseEnvironmentForTest(ctx, store, "user-1", productionID); err != nil {
		t.Fatalf("releaseEnvironment: %v", err)
	}
	drainSourceWork(t, reconciler, ctx)
	if got := countBuildRuns(t, store, ctx, service.ID); got != 1 {
		t.Fatalf("expected manual release to queue the initial build while off, got %d", got)
	}
	deploymentsBefore := countDeployments(t, store, ctx, service.ID)

	// Expire the binding so the next push takes the stale refresh path.
	if _, err := store.db.ExecContext(ctx, `UPDATE source_bindings SET fresh_until = $1 WHERE service_id = $2`, time.Now().UTC().Add(-time.Hour), service.ID); err != nil {
		t.Fatalf("expire source binding: %v", err)
	}

	pushPayload := []byte(`{
		"ref":"refs/heads/main",
		"after":"commit-stale-9",
		"head_commit":{"message":"Stale revision","author":{"name":"Octocat"}},
		"repository":{"id":1,"name":"hello","full_name":"public/hello","owner":{"login":"public"}},
		"installation":{"id":0}
	}`)
	if err := processor.ProcessPushEvent(ctx, pushPayload); err != nil {
		t.Fatalf("processPushEvent: %v", err)
	}
	drainSourceWork(t, reconciler, ctx)

	var recorded string
	if err := store.db.QueryRowContext(ctx, `SELECT commit_sha FROM source_revisions WHERE service_id = $1 ORDER BY observed_at DESC, created_at DESC, id DESC LIMIT 1`, service.ID).Scan(&recorded); err != nil {
		t.Fatalf("latest source revision: %v", err)
	}
	if recorded != "commit-stale-9" {
		t.Fatalf("expected pushed revision to be recorded, got %q", recorded)
	}
	if got := countBuildRuns(t, store, ctx, service.ID); got != 1 {
		t.Fatalf("expected no build while auto-deploy is off, got %d builds", got)
	}
	if got := countDeployments(t, store, ctx, service.ID); got != deploymentsBefore {
		t.Fatalf("expected no rollout while auto-deploy is off, got %d deployments (was %d)", got, deploymentsBefore)
	}
	if got := server.pathHits("/repos/public/hello/tarball/commit-stale-9"); got != 0 {
		t.Fatalf("expected no snapshot fetch while auto-deploy is off, got %d fetches", got)
	}

	released, _, err := releaseEnvironmentForTest(ctx, store, "user-1", productionID)
	if err != nil {
		t.Fatalf("manual releaseEnvironment: %v", err)
	}
	if len(released) != 1 || released[0].ID != service.ID {
		t.Fatalf("expected manual release to pick up the recorded revision, got %d services", len(released))
	}
	drainSourceWork(t, reconciler, ctx)
	if got := countBuildRuns(t, store, ctx, service.ID); got != 2 {
		t.Fatalf("expected manual deploy to queue a build for the recorded revision, got %d builds", got)
	}
	var manualCommit string
	if err := store.db.QueryRowContext(ctx, `SELECT commit_sha FROM build_runs WHERE service_id = $1 ORDER BY queued_at DESC, id DESC LIMIT 1`, service.ID).Scan(&manualCommit); err != nil {
		t.Fatalf("manual build commit: %v", err)
	}
	if manualCommit != "commit-public-main" {
		t.Fatalf("expected manual deploy to build the tracked head, got %q", manualCommit)
	}
}

func drainSourceWork(t *testing.T, reconciler *GitHubReconciler, ctx context.Context) {
	t.Helper()
	for i := 0; i < 10; i++ {
		processed, err := reconciler.ProcessNext(ctx)
		if err != nil {
			t.Fatalf("processNext(%d): %v", i, err)
		}
		if !processed {
			return
		}
	}
	t.Fatalf("source work did not drain")
}

func countBuildRuns(t *testing.T, store *persistence, ctx context.Context, serviceID string) int {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM build_runs WHERE service_id = $1`, serviceID).Scan(&count); err != nil {
		t.Fatalf("count build runs: %v", err)
	}
	return count
}

func countDeployments(t *testing.T, store *persistence, ctx context.Context, serviceID string) int {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM deployments WHERE service_id = $1`, serviceID).Scan(&count); err != nil {
		t.Fatalf("count deployments: %v", err)
	}
	return count
}
