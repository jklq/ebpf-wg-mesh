//go:build integration

package controlplane

import (
	"context"
	"ebof-wg-mesh/internal/controlplane/logs"
	"strings"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"

	"google.golang.org/protobuf/types/known/timestamppb"
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

	service, err := createScheduledService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{80})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "octocat/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	))
	if err != nil {
		t.Fatalf("createScheduledService: %v", err)
	}
	if service.AllocatedAgentID != "" {
		t.Fatalf("first source build unexpectedly started with allocation %q", service.AllocatedAgentID)
	}
	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatalf("seedReadySourceState: %v", err)
	}
	build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)

	notifier := &recordingNotifier{}
	registry := NewRegistryPolicy(config.RegistryConfig{
		Host:                 "registry.example.test",
		NamespacePrefix:      "platform",
		CredentialTTLSeconds: 300,
	}, nil)
	builderService := NewBuilderService(NewBuildOperations(store.builds, store.reads, store.source, newDelivery(store, notifier, nil, nil, nil), registry, registry, 0))
	imageRef := registry.RuntimeDigestRef(registry.PushRef(build.ProjectID, build.EnvironmentID, build.ID, build.ServiceID, build.CommitSHA), "sha256:"+strings.Repeat("1", 64))

	_, err = builderService.CompleteBuild(
		contextWithClientIdentity(serviceCallerBuilder, "builder-1"),
		&platformv1.CompleteBuildRequest{
			BuilderId:   "builder-1",
			BuildId:     build.ID,
			State:       platformv1.BuildState_BUILD_STATE_SUCCEEDED,
			CommitSha:   "commit-1",
			ImageDigest: imageRef,
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
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{80})},
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
	build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)

	notifier := &recordingNotifier{}
	builderService := NewBuilderService(NewBuildOperations(store.builds, store.reads, store.source, newDelivery(store, notifier, nil, nil, nil), nil, nil, 0))

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

func TestBuilderServiceReportBuildLogsWritesTrustedBuildRows(t *testing.T) {
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
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{80})},
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
	build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)

	writer := &recordingLogWriter{enabled: true}
	builderService := NewBuilderService(NewBuildOperations(store.builds, store.reads, store.source, nil, nil, nil, 0, WithBuilderLogEmitter(logs.NewLogEmitter(writer))))
	observedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)

	_, err = builderService.ReportBuildLogs(
		contextWithClientIdentity(serviceCallerBuilder, "builder-1"),
		&platformv1.ReportBuildLogsRequest{
			BuildId: build.ID,
			Lines: []*platformv1.BuildLogLine{
				{
					ObservedAt: timestamppb.New(observedAt),
					Stream:     "stdout",
					Sequence:   7,
					Line:       "step 1/3\r\n",
				},
				{
					Stream:   "mystery",
					Sequence: 8,
					Line:     "unknown stream",
				},
				{
					Stream:   "stderr",
					Sequence: 9,
					Line:     "\r\n",
				},
			},
		},
	)
	if err != nil {
		t.Fatalf("ReportBuildLogs: %v", err)
	}

	lines := writer.Flatten()
	if len(lines) != 2 {
		t.Fatalf("expected 2 persisted lines, got %d", len(lines))
	}
	if lines[0].EnvironmentID != service.EnvironmentID || lines[0].ServiceID != service.ID {
		t.Fatalf("unexpected service scoping %+v", lines[0])
	}
	if lines[0].AgentID != "builder-1" {
		t.Fatalf("expected caller builder id to be recorded, got %q", lines[0].AgentID)
	}
	if lines[0].LogType != logs.LogTypeBuild || lines[0].BuildID != build.ID || lines[0].Stage != logs.StageBuild {
		t.Fatalf("unexpected build log metadata %+v", lines[0])
	}
	if lines[0].Stream != "stdout" || lines[0].Line != "step 1/3" || !lines[0].ObservedAt.Equal(observedAt) {
		t.Fatalf("unexpected first line %+v", lines[0])
	}
	if lines[1].Stream != "combined" || lines[1].Line != "unknown stream" || lines[1].Sequence != 8 {
		t.Fatalf("unexpected second line %+v", lines[1])
	}
}

func TestBuilderServiceReportBuildLogsNoOps(t *testing.T) {
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
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{80})},
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
	build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)

	disabledWriter := &recordingLogWriter{enabled: false}
	builderService := NewBuilderService(NewBuildOperations(store.builds, store.reads, store.source, nil, nil, nil, 0, WithBuilderLogEmitter(logs.NewLogEmitter(disabledWriter))))
	if _, err := builderService.ReportBuildLogs(
		contextWithClientIdentity(serviceCallerBuilder, "builder-1"),
		&platformv1.ReportBuildLogsRequest{
			BuildId: build.ID,
			Lines:   []*platformv1.BuildLogLine{{Line: "ignored"}},
		},
	); err != nil {
		t.Fatalf("ReportBuildLogs with disabled emitter: %v", err)
	}

	writer := &recordingLogWriter{enabled: true}
	builderService = NewBuilderService(NewBuildOperations(store.builds, store.reads, store.source, nil, nil, nil, 0, WithBuilderLogEmitter(logs.NewLogEmitter(writer))))
	if _, err := builderService.ReportBuildLogs(
		contextWithClientIdentity(serviceCallerBuilder, "builder-1"),
		&platformv1.ReportBuildLogsRequest{BuildId: build.ID},
	); err != nil {
		t.Fatalf("ReportBuildLogs: %v", err)
	}
	if got := len(writer.Flatten()); got != 0 {
		t.Fatalf("expected no persisted lines for empty batch, got %d", got)
	}
}

type recordingLogWriter struct {
	enabled bool
	batches [][]logs.LogLineInput
}

func (w *recordingLogWriter) Enabled() bool {
	return w != nil && w.enabled
}

func (w *recordingLogWriter) WriteLogLines(_ context.Context, inputs []logs.LogLineInput) error {
	if !w.Enabled() {
		return nil
	}
	clone := append([]logs.LogLineInput(nil), inputs...)
	w.batches = append(w.batches, clone)
	return nil
}

func (w *recordingLogWriter) Flatten() []logs.LogLineInput {
	var out []logs.LogLineInput
	for _, batch := range w.batches {
		out = append(out, batch...)
	}
	return out
}
