//go:build integration

package controlplane

import (
	"context"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

type recordingNotifier struct {
	agentIDs []string
}

func (n *recordingNotifier) Notify(agentID string) {
	n.agentIDs = append(n.agentIDs, agentID)
}

func TestBuilderServiceCompleteBuildNotifiesAllocatedAgentOnSuccess(t *testing.T) {
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
		&platformv1.ServiceRuntime{ContainerPort: 80},
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
		t.Fatalf("seedReadySourceState: %v", err)
	}
	build, err := store.enqueueBuildForService(ctx, "user-1", projects[0].ID, service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForService: %v", err)
	}

	notifier := &recordingNotifier{}
	builderService := NewBuilderService(store, notifier, nil, 0)

	_, err = builderService.CompleteBuild(
		contextWithClientIdentity(serviceCallerBuilder, "builder-1"),
		&platformv1.CompleteBuildRequest{
			BuilderId:   "builder-1",
			BuildId:     build.ID,
			State:       platformv1.BuildState_BUILD_STATE_SUCCEEDED,
			CommitSha:   "commit-1",
			ImageDigest: "registry.example.test/platform/web@sha256:111",
		},
	)
	if err != nil {
		t.Fatalf("CompleteBuild: %v", err)
	}
	if len(notifier.agentIDs) != 1 || notifier.agentIDs[0] != "node-1" {
		t.Fatalf("expected success completion to notify node-1, got %v", notifier.agentIDs)
	}
}

func TestBuilderServiceCompleteBuildSkipsNotifyOnFailure(t *testing.T) {
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
		&platformv1.ServiceRuntime{ContainerPort: 80},
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
		t.Fatalf("seedReadySourceState: %v", err)
	}
	build, err := store.enqueueBuildForService(ctx, "user-1", projects[0].ID, service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForService: %v", err)
	}

	notifier := &recordingNotifier{}
	builderService := NewBuilderService(store, notifier, nil, 0)

	_, err = builderService.CompleteBuild(
		contextWithClientIdentity(serviceCallerBuilder, "builder-1"),
		&platformv1.CompleteBuildRequest{
			BuilderId:     "builder-1",
			BuildId:       build.ID,
			State:         platformv1.BuildState_BUILD_STATE_FAILED,
			CommitSha:     "commit-1",
			FailureReason: "build failed",
		},
	)
	if err != nil {
		t.Fatalf("CompleteBuild: %v", err)
	}
	if len(notifier.agentIDs) != 0 {
		t.Fatalf("expected failed completion not to notify agents, got %v", notifier.agentIDs)
	}
}
