//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/source"
	"errors"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func claimBuildForTest(t *testing.T, store *persistence, ctx context.Context, builderID, expectedBuildID string) {
	t.Helper()
	claimed, err := claimNextBuild(ctx, store, builderID, builderID, 0)
	if err != nil {
		t.Fatalf("claimNextBuild: %v", err)
	}
	if claimed.ID != expectedBuildID {
		t.Fatalf("expected build %q to be claimed, got %q", expectedBuildID, claimed.ID)
	}
}

func TestRepoBackedServiceSkipsDesiredStateUntilBuildSucceeds(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "octocat/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}

	state, err := desiredStateForAgent(ctx, store, "node-1")
	if err != nil {
		t.Fatalf("desiredStateForAgent(before build): %v", err)
	}
	if len(state.GetServices()) != 0 {
		t.Fatalf("expected repo-backed service to stay out of desired state before resolution, got %d services", len(state.GetServices()))
	}

	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatalf("seedReadySourceState: %v", err)
	}
	build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	if err := completeBuildForTest(ctx, store, "builder-2", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", "registry.example.test/platform/evil@sha256:999", ""); !errors.Is(err, deliverycore.ErrBuildNotOwned) {
		t.Fatalf("expected foreign builder completion to be rejected, got %v", err)
	}
	if err := completeBuildForTest(ctx, store, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", "registry.example.test/platform/web@sha256:111", ""); err != nil {
		t.Fatalf("completeBuild: %v", err)
	}

	current, err := store.reads.ServiceByID(ctx, "user-1", service.ID)
	if err != nil {
		t.Fatalf("ServiceByID(after build): %v", err)
	}
	if current.ResolvedImage != "registry.example.test/platform/web@sha256:111" {
		t.Fatalf("expected resolved image to be updated, got %q", current.ResolvedImage)
	}
	if current.LastSuccessfulCommitSHA != "commit-1" {
		t.Fatalf("expected last successful commit to be recorded, got %q", current.LastSuccessfulCommitSHA)
	}
	if current.RolloutGeneration != 1 {
		t.Fatalf("expected rollout generation 1 after first successful build, got %d", current.RolloutGeneration)
	}

	state, err = desiredStateForAgent(ctx, store, "node-1")
	if err != nil {
		t.Fatalf("desiredStateForAgent(after build): %v", err)
	}
	if len(state.GetServices()) != 1 {
		t.Fatalf("expected resolved repo-backed service in desired state, got %d", len(state.GetServices()))
	}
	if got := state.GetServices()[0].GetSpec().GetImage(); got != "registry.example.test/platform/web@sha256:111" {
		t.Fatalf("expected resolved desired image digest, got %q", got)
	}

	if err := store.source.LinkProjectGitHubRepository(ctx, projects[0].ID, "user-1", source.GitHubRepositoryView{
		RepositoryID:   1,
		FullName:       "octocat/hello",
		InstallationID: 1,
	}); err != nil {
		t.Fatalf("grant source repository: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM source_work_items`); err != nil {
		t.Fatalf("clear source work items: %v", err)
	}
	if _, _, err := scaleService(ctx, store, "user-1", service.ID, 2); err != nil {
		t.Fatalf("queue replica change: %v", err)
	}
	if _, _, err := releaseEnvironmentForTest(ctx, store, "user-1", service.EnvironmentID); err != nil {
		t.Fatalf("deploy replica change: %v", err)
	}
	if got := countSourceWorkItems(t, store, ctx, source.SourceWorkKindSourceSpecChanged); got != 0 {
		t.Fatalf("replica-only deploy queued %d source builds, want 0", got)
	}
	current, err = store.reads.ServiceByID(ctx, "user-1", service.ID)
	if err != nil {
		t.Fatalf("ServiceByID(after replica deploy): %v", err)
	}
	if current.DesiredReplicaCount != 2 {
		t.Fatalf("live desired replica count = %d, want 2", current.DesiredReplicaCount)
	}
	if current.ResolvedImage != "registry.example.test/platform/web@sha256:111" {
		t.Fatalf("replica deploy changed resolved image to %q", current.ResolvedImage)
	}
}

func TestFailedBuildPreservesLastGoodResolvedImage(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "octocat/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatalf("seedReadySourceState(commit-1): %v", err)
	}
	firstBuild, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest(first): %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", firstBuild.ID)
	if err := completeBuildForTest(ctx, store, "builder-1", firstBuild.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", "registry.example.test/platform/web@sha256:111", ""); err != nil {
		t.Fatalf("completeBuild(first): %v", err)
	}

	if err := seedReadySourceState(t, store, service, "commit-2"); err != nil {
		t.Fatalf("seedReadySourceState(commit-2): %v", err)
	}
	secondBuild, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-2")
	if err != nil {
		t.Fatalf("enqueueBuildForTest(second): %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", secondBuild.ID)
	if err := completeBuildForTest(ctx, store, "builder-1", secondBuild.ID, platformv1.BuildState_BUILD_STATE_FAILED, "commit-2", "", "docker build failed"); err != nil {
		t.Fatalf("completeBuild(second): %v", err)
	}

	current, err := store.reads.ServiceByID(ctx, "user-1", service.ID)
	if err != nil {
		t.Fatalf("serviceByID: %v", err)
	}
	if current.ResolvedImage != "registry.example.test/platform/web@sha256:111" {
		t.Fatalf("expected failed build to preserve last good image, got %q", current.ResolvedImage)
	}
	if current.RolloutGeneration != 1 {
		t.Fatalf("expected failed build not to advance rollout generation, got %d", current.RolloutGeneration)
	}
	if current.LatestBuild == nil || current.LatestBuild.GetState() != platformv1.BuildState_BUILD_STATE_FAILED {
		t.Fatalf("expected latest build status to show failure, got %+v", current.LatestBuild)
	}
}

func TestOlderRunningBuildCannotOverwriteNewerSuccessfulResolution(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "octocat/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatalf("seedReadySourceState(commit-1): %v", err)
	}
	build1, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest(build1): %v", err)
	}
	claimed, err := claimNextBuild(ctx, store, "builder-1", "builder-1", 0)
	if err != nil {
		t.Fatalf("claimNextBuild: %v", err)
	}
	if claimed.ID != build1.ID {
		t.Fatalf("expected first build to be claimed, got %q", claimed.ID)
	}

	if err := seedReadySourceState(t, store, service, "commit-2"); err != nil {
		t.Fatalf("seedReadySourceState(commit-2): %v", err)
	}
	build2, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-2")
	if err != nil {
		t.Fatalf("enqueueBuildForTest(build2): %v", err)
	}
	if err := completeBuildForTest(ctx, store, "builder-1", build1.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", "registry.example.test/platform/web@sha256:111", ""); err != nil {
		t.Fatalf("completeBuild(build1): %v", err)
	}

	current, err := store.reads.ServiceByID(ctx, "user-1", service.ID)
	if err != nil {
		t.Fatalf("ServiceByID(after old success): %v", err)
	}
	if current.ResolvedImage != "" {
		t.Fatalf("expected older build success to stay unapplied while newer work exists, got %q", current.ResolvedImage)
	}

	claimBuildForTest(t, store, ctx, "builder-1", build2.ID)
	if err := completeBuildForTest(ctx, store, "builder-1", build2.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-2", "registry.example.test/platform/web@sha256:222", ""); err != nil {
		t.Fatalf("completeBuild(build2): %v", err)
	}
	current, err = store.reads.ServiceByID(ctx, "user-1", service.ID)
	if err != nil {
		t.Fatalf("ServiceByID(after new success): %v", err)
	}
	if current.ResolvedImage != "registry.example.test/platform/web@sha256:222" {
		t.Fatalf("expected newer successful build to win, got %q", current.ResolvedImage)
	}
}

func TestSuccessfulBuildSupersedesInProgressRollout(t *testing.T) {
	t.Parallel()

	store, _, service := newRepoBuildTestService(t)
	ctx := context.Background()

	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatalf("seedReadySourceState(commit-1): %v", err)
	}
	build1, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest(build1): %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build1.ID)
	if err := completeBuildForTest(ctx, store, "builder-1", build1.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", "registry.example.test/platform/web@sha256:111", ""); err != nil {
		t.Fatalf("completeBuild(build1): %v", err)
	}
	assertRolloutState(t, store, service.ID, 1, "in_progress", "")

	if err := seedReadySourceState(t, store, service, "commit-2"); err != nil {
		t.Fatalf("seedReadySourceState(commit-2): %v", err)
	}
	build2, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-2")
	if err != nil {
		t.Fatalf("enqueueBuildForTest(build2): %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build2.ID)
	if err := completeBuildForTest(ctx, store, "builder-1", build2.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-2", "registry.example.test/platform/web@sha256:222", ""); err != nil {
		t.Fatalf("completeBuild(build2): %v", err)
	}

	assertRolloutState(t, store, service.ID, 1, "superseded", "newer rollout")
	assertRolloutState(t, store, service.ID, 2, "in_progress", "")
	current, err := store.reads.ServiceByID(ctx, "user-1", service.ID)
	if err != nil {
		t.Fatalf("serviceByID: %v", err)
	}
	if current.RolloutGeneration != 2 || current.ResolvedImage != "registry.example.test/platform/web@sha256:222" {
		t.Fatalf("latest build was not scheduled: generation=%d image=%q", current.RolloutGeneration, current.ResolvedImage)
	}
	if old := allocationForGeneration(t, store, service.ID, 1); len(old) != 0 {
		t.Fatalf("superseded rollout retained unrouted allocations: %+v", old)
	}
	if next := allocationForGeneration(t, store, service.ID, 2); len(next) != 1 || next[0].RolloutState != deliverycore.AllocationRolloutStarting {
		t.Fatalf("new rollout allocations = %+v, want one starting allocation", next)
	}
	var deploymentState string
	if err := store.db.QueryRowContext(ctx,
		`SELECT state FROM deployments WHERE service_id = $1 AND build_id = $2`,
		service.ID, build2.ID,
	).Scan(&deploymentState); err != nil {
		t.Fatalf("load build2 deployment: %v", err)
	}
	if deploymentState == deliverycore.DeploymentStateFailed {
		t.Fatal("successful build was rejected")
	}
}

func TestSupersededDeploymentDoesNotBlockRolloutFinalization(t *testing.T) {
	t.Parallel()

	store, _, service := newRepoBuildTestService(t)
	ctx := context.Background()

	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatalf("seedReadySourceState(commit-1): %v", err)
	}
	build1, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest(build1): %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build1.ID)
	if err := completeBuildForTest(ctx, store, "builder-1", build1.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", "registry.example.test/platform/web@sha256:111", ""); err != nil {
		t.Fatalf("completeBuild(build1): %v", err)
	}
	target := allocationForGeneration(t, store, service.ID, 1)
	if len(target) != 1 {
		t.Fatalf("rollout 1 allocations = %+v, want one", target)
	}
	markRolloutAllocationReady(t, store, target[0])

	if err := seedReadySourceState(t, store, service, "commit-2"); err != nil {
		t.Fatalf("seedReadySourceState(commit-2): %v", err)
	}
	if _, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-2"); err != nil {
		t.Fatalf("enqueueBuildForTest(build2): %v", err)
	}
	if err := newTestDelivery(store, nil, nil, nil).ReconcileRollouts(ctx); err != nil {
		t.Fatalf("advance superseded deployment rollout: %v", err)
	}
	assertRolloutState(t, store, service.ID, 1, "succeeded", "")
}

func TestConcurrentBuildEnqueuesHaveOneCurrentWinner(t *testing.T) {
	t.Parallel()

	store, _, service := newRepoBuildTestService(t)
	ctx := context.Background()
	for _, commit := range []string{"commit-1", "commit-2"} {
		if err := seedReadySourceState(t, store, service, commit); err != nil {
			t.Fatalf("seedReadySourceState(%s): %v", commit, err)
		}
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, commit := range []string{"commit-1", "commit-2"} {
		commit := commit
		go func() {
			<-start
			_, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, commit)
			errs <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent enqueue: %v", err)
		}
	}

	var queued, superseded int
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FILTER (WHERE state = $1), count(*) FILTER (WHERE state = $2)
		   FROM build_runs WHERE service_id = $3`,
		deliverycore.BuildStateQueued, deliverycore.BuildStateSuperseded, service.ID,
	).Scan(&queued, &superseded); err != nil {
		t.Fatalf("count build states: %v", err)
	}
	if queued != 1 || superseded != 1 {
		t.Fatalf("build states: queued=%d superseded=%d, want one of each", queued, superseded)
	}
	var currentBuildID, queuedBuildID string
	if err := store.db.QueryRowContext(ctx,
		`SELECT d.build_id, b.id
		   FROM deployments d
		   JOIN build_runs b ON b.service_id = d.service_id AND b.state = $1
		  WHERE d.service_id = $2 AND d.is_current = TRUE`,
		deliverycore.BuildStateQueued, service.ID,
	).Scan(&currentBuildID, &queuedBuildID); err != nil {
		t.Fatalf("load current queued deployment: %v", err)
	}
	if currentBuildID != queuedBuildID {
		t.Fatalf("current deployment build=%q, queued build=%q", currentBuildID, queuedBuildID)
	}
}

func TestRepoBackedBuildRequiresPersistedSourceState(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "octocat/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}

	if _, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1"); err == nil {
		t.Fatal("expected repo-backed build enqueue to fail without persisted source state")
	}
}

func TestEnqueueBuildPersistsCommitMetadataAndTargetRolloutGeneration(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "octocat/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	if err := seedReadySourceStateWithMetadata(t, store, service, "commit-1", "Fix deploy history", "Alice"); err != nil {
		t.Fatalf("seedReadySourceStateWithMetadata: %v", err)
	}

	build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest: %v", err)
	}
	if build.CommitMessage != "Fix deploy history" {
		t.Fatalf("expected build commit message to persist, got %q", build.CommitMessage)
	}
	if build.CommitAuthor != "Alice" {
		t.Fatalf("expected build commit author to persist, got %q", build.CommitAuthor)
	}
	if build.TargetRolloutGeneration != 1 {
		t.Fatalf("expected target rollout generation 1, got %d", build.TargetRolloutGeneration)
	}
}

func TestCompleteBuildStoresRolloutBuildLink(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "octocat/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	if err := seedReadySourceStateWithMetadata(t, store, service, "commit-1", "Fix deploy history", "Alice"); err != nil {
		t.Fatalf("seedReadySourceStateWithMetadata: %v", err)
	}

	build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	if err := completeBuildForTest(ctx, store, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", "registry.example.test/platform/web@sha256:111", ""); err != nil {
		t.Fatalf("completeBuild: %v", err)
	}

	var rolloutBuildID string
	if err := store.db.QueryRowContext(ctx,
		`SELECT build_id
		   FROM service_rollouts
		  WHERE service_id = $1
		    AND rollout_generation = $2`,
		service.ID, 1,
	).Scan(&rolloutBuildID); err != nil {
		t.Fatalf("query rollout build id: %v", err)
	}
	if rolloutBuildID != build.ID {
		t.Fatalf("expected rollout build_id %q, got %q", build.ID, rolloutBuildID)
	}
}

func TestListServiceDeploymentsReturnsPersistedBuildAndDirectImageHistory(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	repoService, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "repo-web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "octocat/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("create repo service: %v", err)
	}
	if err := seedReadySourceStateWithMetadata(t, store, repoService, "commit-1", "Fix deploy history", "Alice"); err != nil {
		t.Fatalf("seedReadySourceStateWithMetadata: %v", err)
	}
	build, err := enqueueBuildForTest(ctx, store, "user-1", repoService.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	if err := completeBuildForTest(ctx, store, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", "registry.example.test/platform/repo-web@sha256:111", ""); err != nil {
		t.Fatalf("completeBuild: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE source_snapshots SET source_revision_id = NULL WHERE commit_sha = $1`,
		"commit-1",
	); err != nil {
		t.Fatalf("detach source snapshot revision: %v", err)
	}

	repoDeployments, err := store.reads.ListServiceDeployments(ctx, "user-1", repoService.ID, 10)
	if err != nil {
		t.Fatalf("ListServiceDeployments(repo): %v", err)
	}
	if len(repoDeployments) < 2 {
		t.Fatalf("expected at least two repo deployments, got %d", len(repoDeployments))
	}
	if repoDeployments[0].Build == nil || repoDeployments[0].Build.CommitMessage != "Fix deploy history" {
		t.Fatalf("expected latest repo deployment to include persisted commit message, got %+v", repoDeployments[0].Build)
	}

	imageService, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "img-web", directImageServiceSpec(pinnedImage("b"), &platformv1.ServiceRuntime{
		Ports: runtimePortsFromInts([]int32{8081}),
	}), "node-1")
	if err != nil {
		t.Fatalf("create direct-image service: %v", err)
	}
	imageDeployment := currentDeploymentForTest(t, store, ctx, imageService.ID)
	if _, _, err := applyDeploymentActionForTest(ctx, store, "user-1", imageService.ID, imageDeployment.ID, platformv1.DeploymentAction_DEPLOYMENT_ACTION_EXACT_REDEPLOY, "image-history-exact-redeploy", ""); err != nil {
		t.Fatalf("applyDeploymentAction(EXACT_REDEPLOY): %v", err)
	}

	imageDeployments, err := store.reads.ListServiceDeployments(ctx, "user-1", imageService.ID, 10)
	if err != nil {
		t.Fatalf("ListServiceDeployments(image): %v", err)
	}
	if len(imageDeployments) < 2 {
		t.Fatalf("expected at least two direct-image deployments, got %d", len(imageDeployments))
	}
	if imageDeployments[0].Build != nil {
		t.Fatalf("expected direct-image redeploy to have no build row, got %+v", imageDeployments[0].Build)
	}
	if imageDeployments[0].ReasonCode != "EXACT_REDEPLOY" {
		t.Fatalf("expected latest direct-image deployment reason %q, got %q", "EXACT_REDEPLOY", imageDeployments[0].ReasonCode)
	}
}

func TestListServiceDeploymentsIncludesFailedBuildAttempt(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "octocat/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	if err := seedReadySourceStateWithMetadata(t, store, service, "commit-1", "Break deploy history", "Alice"); err != nil {
		t.Fatalf("seedReadySourceStateWithMetadata: %v", err)
	}

	build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	if err := completeBuildForTest(ctx, store, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_FAILED, "commit-1", "", "docker build failed"); err != nil {
		t.Fatalf("completeBuild: %v", err)
	}

	deployments, err := store.reads.ListServiceDeployments(ctx, "user-1", service.ID, 10)
	if err != nil {
		t.Fatalf("listServiceDeployments: %v", err)
	}
	if len(deployments) == 0 {
		t.Fatal("expected failed build deployment history")
	}
	if deployments[0].Build == nil || deployments[0].Build.State != deliverycore.BuildStateFailed {
		t.Fatalf("expected latest history entry to be failed build, got %+v", deployments[0].Build)
	}
	if deployments[0].Build.CommitMessage != "Break deploy history" {
		t.Fatalf("expected failed build commit message to persist, got %+v", deployments[0].Build)
	}
}

func TestEnqueueBuildAllowsRepeatedSameCommitAttempts(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "octocat/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	if err := seedReadySourceStateWithMetadata(t, store, service, "commit-1", "Fix deploy history", "Alice"); err != nil {
		t.Fatalf("seedReadySourceStateWithMetadata: %v", err)
	}

	firstBuild, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest(first): %v", err)
	}
	secondBuild, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest(second): %v", err)
	}
	if firstBuild.ID == secondBuild.ID {
		t.Fatalf("expected repeated same-commit build to create a new row, got %q", firstBuild.ID)
	}

	var count int
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*)
		   FROM build_runs
		  WHERE service_id = $1
		    AND commit_sha = $2`,
		service.ID, "commit-1",
	).Scan(&count); err != nil {
		t.Fatalf("count repeated same-commit builds: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 persisted build attempts for same commit, got %d", count)
	}
}

func seedReadySourceState(t *testing.T, store *persistence, service deliverycore.ServiceRecord, commitSHA string) error {
	return seedReadySourceStateWithMetadata(t, store, service, commitSHA, "", "")
}

func newRepoBuildTestService(t *testing.T) (*persistence, string, deliverycore.ServiceRecord) {
	t.Helper()
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "octocat/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	return store, projects[0].ID, service
}

func seedReadySourceStateWithMetadata(t *testing.T, store *persistence, service deliverycore.ServiceRecord, commitSHA, commitMessage, commitAuthor string) error {
	t.Helper()
	archive := []byte("snapshot-" + commitSHA)
	digest, objectKey, err := store.source.StoreSourceArchive(context.Background(), archive)
	if err != nil {
		return err
	}

	return store.withTx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		binding, err := store.source.UpsertSourceBindingTx(context.Background(), tx, source.SourceBindingRecord{
			ServiceID:                    service.ID,
			ProjectID:                    service.ProjectID,
			Provider:                     "github",
			RepositorySelector:           "octocat/hello",
			TrackedRef:                   "main",
			ProviderRepositoryExternalID: "repo-1",
			ProviderScopeExternalID:      "",
			AccessState:                  source.SourceAccessStateAvailable,
			BuildRecipe:                  &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
			ResolvedAt:                   time.Now().UTC(),
			FreshUntil:                   time.Now().UTC().Add(time.Hour),
		})
		if err != nil {
			return err
		}
		revision, err := store.source.UpsertSourceRevisionTx(context.Background(), tx, source.SourceRevisionRecord{
			SourceBindingID:              binding.ID,
			ServiceID:                    service.ID,
			Provider:                     binding.Provider,
			ProviderRepositoryExternalID: binding.ProviderRepositoryExternalID,
			TrackedRef:                   binding.TrackedRef,
			CommitSHA:                    commitSHA,
			CommitMessage:                commitMessage,
			CommitAuthor:                 commitAuthor,
			ObservedAt:                   time.Now().UTC(),
		})
		if err != nil {
			return err
		}
		_, err = store.source.UpsertSourceSnapshotTx(context.Background(), tx, source.SourceSnapshotRecord{
			SourceRevisionID:             revision.ID,
			Provider:                     binding.Provider,
			ProviderRepositoryExternalID: binding.ProviderRepositoryExternalID,
			CommitSHA:                    commitSHA,
			Digest:                       digest,
			ObjectKey:                    objectKey,
			ArchiveSizeBytes:             int64(len(archive)),
			Ready:                        true,
			FetchedAt:                    sql.NullTime{Time: time.Now().UTC(), Valid: true},
		})
		return err
	})
}
