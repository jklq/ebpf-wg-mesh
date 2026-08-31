//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func claimBuildForTest(t *testing.T, store *Store, ctx context.Context, builderID, expectedBuildID string) {
	t.Helper()
	claimed, err := store.claimNextBuild(ctx, builderID, builderID, 0)
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

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
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

	state, err := store.desiredStateForAgent(ctx, "node-1")
	if err != nil {
		t.Fatalf("desiredStateForAgent(before build): %v", err)
	}
	if len(state.GetServices()) != 0 {
		t.Fatalf("expected repo-backed service to stay out of desired state before resolution, got %d services", len(state.GetServices()))
	}

	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatalf("seedReadySourceState: %v", err)
	}
	build, err := store.enqueueBuildForService(ctx, "user-1", projects[0].ID, service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForService: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	if err := store.completeBuild(ctx, "builder-2", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", "registry.example.test/platform/evil@sha256:999", ""); !errors.Is(err, errBuildNotOwned) {
		t.Fatalf("expected foreign builder completion to be rejected, got %v", err)
	}
	if err := store.completeBuild(ctx, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", "registry.example.test/platform/web@sha256:111", ""); err != nil {
		t.Fatalf("completeBuild: %v", err)
	}

	current, err := store.serviceByID(ctx, "user-1", projects[0].ID, service.ID)
	if err != nil {
		t.Fatalf("serviceByID(after build): %v", err)
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

	state, err = store.desiredStateForAgent(ctx, "node-1")
	if err != nil {
		t.Fatalf("desiredStateForAgent(after build): %v", err)
	}
	if len(state.GetServices()) != 1 {
		t.Fatalf("expected resolved repo-backed service in desired state, got %d", len(state.GetServices()))
	}
	if got := state.GetServices()[0].GetSpec().GetImage(); got != "registry.example.test/platform/web@sha256:111" {
		t.Fatalf("expected resolved desired image digest, got %q", got)
	}

	if err := store.linkProjectGitHubRepository(ctx, projects[0].ID, "user-1", GitHubRepositoryView{
		RepositoryID:   1,
		FullName:       "octocat/hello",
		InstallationID: 1,
	}); err != nil {
		t.Fatalf("grant source repository: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM source_work_items`); err != nil {
		t.Fatalf("clear source work items: %v", err)
	}
	if _, _, err := store.scaleService(ctx, "user-1", "", service.ID, 2); err != nil {
		t.Fatalf("queue replica change: %v", err)
	}
	if _, _, err := store.deployEnvironment(ctx, "user-1", service.EnvironmentID); err != nil {
		t.Fatalf("deploy replica change: %v", err)
	}
	if got := countSourceWorkItems(t, store, ctx, sourceWorkKindSourceSpecChanged); got != 0 {
		t.Fatalf("replica-only deploy queued %d source builds, want 0", got)
	}
	current, err = store.serviceByID(ctx, "user-1", projects[0].ID, service.ID)
	if err != nil {
		t.Fatalf("serviceByID(after replica deploy): %v", err)
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

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
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
	firstBuild, err := store.enqueueBuildForService(ctx, "user-1", projects[0].ID, service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForService(first): %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", firstBuild.ID)
	if err := store.completeBuild(ctx, "builder-1", firstBuild.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", "registry.example.test/platform/web@sha256:111", ""); err != nil {
		t.Fatalf("completeBuild(first): %v", err)
	}

	if err := seedReadySourceState(t, store, service, "commit-2"); err != nil {
		t.Fatalf("seedReadySourceState(commit-2): %v", err)
	}
	secondBuild, err := store.enqueueBuildForService(ctx, "user-1", projects[0].ID, service.ID, "commit-2")
	if err != nil {
		t.Fatalf("enqueueBuildForService(second): %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", secondBuild.ID)
	if err := store.completeBuild(ctx, "builder-1", secondBuild.ID, platformv1.BuildState_BUILD_STATE_FAILED, "commit-2", "", "docker build failed"); err != nil {
		t.Fatalf("completeBuild(second): %v", err)
	}

	current, err := store.serviceByID(ctx, "user-1", projects[0].ID, service.ID)
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

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
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
	build1, err := store.enqueueBuildForService(ctx, "user-1", projects[0].ID, service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForService(build1): %v", err)
	}
	claimed, err := store.claimNextBuild(ctx, "builder-1", "builder-1", 0)
	if err != nil {
		t.Fatalf("claimNextBuild: %v", err)
	}
	if claimed.ID != build1.ID {
		t.Fatalf("expected first build to be claimed, got %q", claimed.ID)
	}

	if err := seedReadySourceState(t, store, service, "commit-2"); err != nil {
		t.Fatalf("seedReadySourceState(commit-2): %v", err)
	}
	build2, err := store.enqueueBuildForService(ctx, "user-1", projects[0].ID, service.ID, "commit-2")
	if err != nil {
		t.Fatalf("enqueueBuildForService(build2): %v", err)
	}
	if err := store.completeBuild(ctx, "builder-1", build1.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", "registry.example.test/platform/web@sha256:111", ""); err != nil {
		t.Fatalf("completeBuild(build1): %v", err)
	}

	current, err := store.serviceByID(ctx, "user-1", projects[0].ID, service.ID)
	if err != nil {
		t.Fatalf("serviceByID(after old success): %v", err)
	}
	if current.ResolvedImage != "" {
		t.Fatalf("expected older build success to stay unapplied while newer work exists, got %q", current.ResolvedImage)
	}

	claimBuildForTest(t, store, ctx, "builder-1", build2.ID)
	if err := store.completeBuild(ctx, "builder-1", build2.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-2", "registry.example.test/platform/web@sha256:222", ""); err != nil {
		t.Fatalf("completeBuild(build2): %v", err)
	}
	current, err = store.serviceByID(ctx, "user-1", projects[0].ID, service.ID)
	if err != nil {
		t.Fatalf("serviceByID(after new success): %v", err)
	}
	if current.ResolvedImage != "registry.example.test/platform/web@sha256:222" {
		t.Fatalf("expected newer successful build to win, got %q", current.ResolvedImage)
	}
}

func TestRepoBackedBuildRequiresPersistedSourceState(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
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

	if _, err := store.enqueueBuildForService(ctx, "user-1", projects[0].ID, service.ID, "commit-1"); err == nil {
		t.Fatal("expected repo-backed build enqueue to fail without persisted source state")
	}
}

func TestEnqueueBuildPersistsCommitMetadataAndTargetRolloutGeneration(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
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

	build, err := store.enqueueBuildForService(ctx, "user-1", projects[0].ID, service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForService: %v", err)
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

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
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

	build, err := store.enqueueBuildForService(ctx, "user-1", projects[0].ID, service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForService: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	if err := store.completeBuild(ctx, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", "registry.example.test/platform/web@sha256:111", ""); err != nil {
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

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	repoService, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "repo-web", repositoryServiceSpec(
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
	build, err := store.enqueueBuildForService(ctx, "user-1", projects[0].ID, repoService.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForService: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	if err := store.completeBuild(ctx, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", "registry.example.test/platform/repo-web@sha256:111", ""); err != nil {
		t.Fatalf("completeBuild: %v", err)
	}

	repoDeployments, err := store.listServiceDeployments(ctx, "user-1", projects[0].ID, repoService.ID, 10)
	if err != nil {
		t.Fatalf("listServiceDeployments(repo): %v", err)
	}
	if len(repoDeployments) < 2 {
		t.Fatalf("expected at least two repo deployments, got %d", len(repoDeployments))
	}
	if repoDeployments[0].Build == nil || repoDeployments[0].Build.CommitMessage != "Fix deploy history" {
		t.Fatalf("expected latest repo deployment to include persisted commit message, got %+v", repoDeployments[0].Build)
	}

	imageService, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "img-web", directImageServiceSpec("nginx:1.27", &platformv1.ServiceRuntime{
		Ports: runtimePortsFromInts([]int32{8081}),
	}), "node-1")
	if err != nil {
		t.Fatalf("create direct-image service: %v", err)
	}
	if _, err := store.redeployService(ctx, "user-1", projects[0].ID, imageService.ID); err != nil {
		t.Fatalf("redeployService: %v", err)
	}

	imageDeployments, err := store.listServiceDeployments(ctx, "user-1", projects[0].ID, imageService.ID, 10)
	if err != nil {
		t.Fatalf("listServiceDeployments(image): %v", err)
	}
	if len(imageDeployments) < 2 {
		t.Fatalf("expected at least two direct-image deployments, got %d", len(imageDeployments))
	}
	if imageDeployments[0].Build != nil {
		t.Fatalf("expected direct-image redeploy to have no build row, got %+v", imageDeployments[0].Build)
	}
	if imageDeployments[0].ReasonCode != reasonUserRedeploy && imageDeployments[0].Reason != reasonUserRedeploy {
		t.Fatalf("expected latest direct-image deployment reason %q, got %q", reasonUserRedeploy, imageDeployments[0].Reason)
	}
}

func TestListServiceDeploymentsIncludesFailedBuildAttempt(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
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

	build, err := store.enqueueBuildForService(ctx, "user-1", projects[0].ID, service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForService: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	if err := store.completeBuild(ctx, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_FAILED, "commit-1", "", "docker build failed"); err != nil {
		t.Fatalf("completeBuild: %v", err)
	}

	deployments, err := store.listServiceDeployments(ctx, "user-1", projects[0].ID, service.ID, 10)
	if err != nil {
		t.Fatalf("listServiceDeployments: %v", err)
	}
	if len(deployments) == 0 {
		t.Fatal("expected failed build deployment history")
	}
	if deployments[0].Build == nil || deployments[0].Build.State != buildStateFailed {
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

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
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

	firstBuild, err := store.enqueueBuildForService(ctx, "user-1", projects[0].ID, service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForService(first): %v", err)
	}
	secondBuild, err := store.enqueueBuildForService(ctx, "user-1", projects[0].ID, service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForService(second): %v", err)
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

func seedReadySourceState(t *testing.T, store *Store, service serviceRecord, commitSHA string) error {
	return seedReadySourceStateWithMetadata(t, store, service, commitSHA, "", "")
}

func seedReadySourceStateWithMetadata(t *testing.T, store *Store, service serviceRecord, commitSHA, commitMessage, commitAuthor string) error {
	t.Helper()
	archive := []byte("snapshot-" + commitSHA)
	digest, objectKey, err := store.storeSourceArchive(context.Background(), archive)
	if err != nil {
		return err
	}

	return store.withTx(context.Background(), func(tx *sql.Tx) error {
		binding, err := store.upsertSourceBindingTx(context.Background(), tx, sourceBindingRecord{
			ServiceID:                    service.ID,
			ProjectID:                    service.ProjectID,
			Provider:                     "github",
			RepositorySelector:           "octocat/hello",
			TrackedRef:                   "main",
			ProviderRepositoryExternalID: "repo-1",
			ProviderScopeExternalID:      "",
			AccessState:                  sourceAccessStateAvailable,
			BuildRecipe:                  &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
			ResolvedAt:                   time.Now().UTC(),
			FreshUntil:                   time.Now().UTC().Add(time.Hour),
		})
		if err != nil {
			return err
		}
		revision, err := store.upsertSourceRevisionTx(context.Background(), tx, sourceRevisionRecord{
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
		_, err = store.upsertSourceSnapshotTx(context.Background(), tx, sourceSnapshotRecord{
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
