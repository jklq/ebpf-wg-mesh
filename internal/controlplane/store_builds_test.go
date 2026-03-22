//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func TestRepoBackedServiceSkipsDesiredStateUntilBuildSucceeds(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
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

	service, err := store.createService(ctx, "user-1", projects[0].ID, "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{ContainerPort: 8080},
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
	if current.RolloutGeneration != 2 {
		t.Fatalf("expected rollout generation 2 after first successful build, got %d", current.RolloutGeneration)
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
}

func TestFailedBuildPreservesLastGoodResolvedImage(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
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

	service, err := store.createService(ctx, "user-1", projects[0].ID, "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{ContainerPort: 8080},
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
	if current.RolloutGeneration != 2 {
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
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
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

	service, err := store.createService(ctx, "user-1", projects[0].ID, "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{ContainerPort: 8080},
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
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
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

	service, err := store.createService(ctx, "user-1", projects[0].ID, "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{ContainerPort: 8080},
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

func seedReadySourceState(t *testing.T, store *Store, service serviceRecord, commitSHA string) error {
	t.Helper()

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
			ObservedAt:                   time.Now().UTC(),
		})
		if err != nil {
			return err
		}
		archive := []byte("snapshot-" + commitSHA)
		_, err = store.upsertSourceSnapshotTx(context.Background(), tx, sourceSnapshotRecord{
			SourceRevisionID:             revision.ID,
			Provider:                     binding.Provider,
			ProviderRepositoryExternalID: binding.ProviderRepositoryExternalID,
			CommitSHA:                    commitSHA,
			Digest:                       snapshotDigest(archive),
			ArchiveTGZ:                   archive,
			Ready:                        true,
			FetchedAt:                    sql.NullTime{Time: time.Now().UTC(), Valid: true},
		})
		return err
	})
}
