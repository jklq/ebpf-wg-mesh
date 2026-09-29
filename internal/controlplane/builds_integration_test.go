//go:build integration

package controlplane

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/registry"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
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
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
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
			BuildRecipe:        &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."},
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
	claimed := claimBuildForTest(t, store, ctx, "builder-1", build.ID)

	notifier := &recordingNotifier{}
	policy := registry.NewPolicy(config.RegistryConfig{
		Host:                 "registry.example.test",
		NamespacePrefix:      "platform",
		CredentialTTLSeconds: 300,
	}, nil)
	builderService := newBuilderService(newBuildOperations(store.db, store.source, newDelivery(store, notifier, nil, nil, nil), policy, policy))
	imageRef := policy.RuntimeDigestRef(policy.PushRef(build.ProjectID, build.EnvironmentID, build.ID, build.ServiceID, build.CommitSHA), "sha256:"+strings.Repeat("1", 64))

	_, err = builderService.CompleteBuild(
		contextWithClientIdentity(serviceCallerBuilder, "builder-1"),
		&platformv1.CompleteBuildRequest{
			BuilderId:   "builder-1",
			BuildId:     build.ID,
			State:       platformv1.BuildState_BUILD_STATE_SUCCEEDED,
			CommitSha:   "commit-1",
			ImageDigest: imageRef,
			LeaseEpoch:  claimed.OwnerEpoch,
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
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
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
			BuildRecipe:        &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."},
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
	claimed := claimBuildForTest(t, store, ctx, "builder-1", build.ID)

	notifier := &recordingNotifier{}
	builderService := newBuilderService(newBuildOperations(store.db, store.source, newDelivery(store, notifier, nil, nil, nil), nil, nil))

	_, err = builderService.CompleteBuild(
		contextWithClientIdentity(serviceCallerBuilder, "builder-1"),
		&platformv1.CompleteBuildRequest{
			BuilderId:     "builder-1",
			BuildId:       build.ID,
			State:         platformv1.BuildState_BUILD_STATE_FAILED,
			CommitSha:     "commit-1",
			FailureReason: "build failed",
			LeaseEpoch:    claimed.OwnerEpoch,
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
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
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
			BuildRecipe:        &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."},
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
	claimed := claimBuildForTest(t, store, ctx, "builder-1", build.ID)

	writer := &recordingLogWriter{enabled: true}
	builderService := newBuilderService(newBuildOperations(store.db, store.source, testDelivery(store).Delivery, nil, nil, withBuilderLogEmitter(logs.NewLogEmitter(writer, nil))))
	observedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)

	_, err = builderService.ReportBuildLogs(
		contextWithClientIdentity(serviceCallerBuilder, "builder-1"),
		&platformv1.ReportBuildLogsRequest{
			BuildId:    build.ID,
			LeaseEpoch: claimed.OwnerEpoch,
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
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
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
			BuildRecipe:        &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."},
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
	claimed := claimBuildForTest(t, store, ctx, "builder-1", build.ID)

	disabledWriter := &recordingLogWriter{enabled: false}
	builderService := newBuilderService(newBuildOperations(store.db, store.source, testDelivery(store).Delivery, nil, nil, withBuilderLogEmitter(logs.NewLogEmitter(disabledWriter, nil))))
	if _, err := builderService.ReportBuildLogs(
		contextWithClientIdentity(serviceCallerBuilder, "builder-1"),
		&platformv1.ReportBuildLogsRequest{
			BuildId:    build.ID,
			LeaseEpoch: claimed.OwnerEpoch,
			Lines:      []*platformv1.BuildLogLine{{Line: "ignored"}},
		},
	); err != nil {
		t.Fatalf("ReportBuildLogs with disabled emitter: %v", err)
	}

	writer := &recordingLogWriter{enabled: true}
	builderService = newBuilderService(newBuildOperations(store.db, store.source, testDelivery(store).Delivery, nil, nil, withBuilderLogEmitter(logs.NewLogEmitter(writer, nil))))
	if _, err := builderService.ReportBuildLogs(
		contextWithClientIdentity(serviceCallerBuilder, "builder-1"),
		&platformv1.ReportBuildLogsRequest{BuildId: build.ID, LeaseEpoch: claimed.OwnerEpoch},
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
	gaps    []logs.GapInput
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

func (w *recordingLogWriter) WriteGaps(_ context.Context, gaps []logs.GapInput) error {
	if !w.Enabled() {
		return nil
	}
	w.gaps = append(w.gaps, gaps...)
	return nil
}

func (w *recordingLogWriter) Flatten() []logs.LogLineInput {
	var out []logs.LogLineInput
	for _, batch := range w.batches {
		out = append(out, batch...)
	}
	return out
}

type retryBuildCredentials struct{ attempts int }

func (c *retryBuildCredentials) CredentialsForBuild(context.Context, string, string, string) (string, string, error) {
	c.attempts++
	if c.attempts == 1 {
		return "", "", errors.New("credentials temporarily unavailable")
	}
	return "builder", "password", nil
}

func TestBuildOperationsRetriesPreparationWithoutLosingLease(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createScheduledService(ctx, store, "owner", productionEnvironmentID(t, store, project.ID), "web", repositoryServiceSpec(nil, &platformv1.ServiceSourceSpec{
		Provider: "github", RepositorySelector: "octocat/hello", TrackedRef: "main", BuildRecipe: &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatal(err)
	}
	build, err := enqueueBuildForTest(ctx, store, "owner", service.ID, "commit-1")
	if err != nil {
		t.Fatal(err)
	}
	policy := registry.NewPolicy(config.RegistryConfig{Host: "registry.example.test", NamespacePrefix: "platform", CredentialTTLSeconds: 300}, nil)
	credentials := &retryBuildCredentials{}
	notifier := &recordingNotifier{}
	logWriter := &recordingLogWriter{enabled: true}
	operations := newBuildOperations(store.db, store.source, newDelivery(store, notifier, nil, nil, nil), policy, credentials, withBuilderLogEmitter(logs.NewLogEmitter(logWriter, nil)))
	builder := contextWithClientIdentity(serviceCallerBuilder, "builder-1")
	claim := &platformv1.ClaimBuildRequest{BuilderId: "builder-1"}
	if _, err := operations.ClaimBuild(builder, claim); err == nil {
		t.Fatal("expected credential preparation failure")
	}
	job, err := operations.ClaimBuild(builder, claim)
	if err != nil || job.GetBuildId() != build.ID || job.GetRegistryPassword() != "password" {
		t.Fatalf("retry lost prepared job: %v, %v", job, err)
	}
	if job.GetSource().GetBuildRecipe().GetBuilder() != platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE {
		t.Fatalf("claimed job lost builder choice: %+v", job.GetSource().GetBuildRecipe())
	}
	image := policy.RuntimeDigestRef(job.RegistryPushReference, "sha256:"+strings.Repeat("a", 64))
	completion := &platformv1.CompleteBuildRequest{BuilderId: "builder-1", BuildId: build.ID, State: platformv1.BuildState_BUILD_STATE_SUCCEEDED, CommitSha: "wrong-commit", ImageDigest: image, LeaseEpoch: job.GetLeaseEpoch()}
	if _, err := operations.CompleteBuild(builder, completion); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("mismatched commit accepted: %v", err)
	}
	completion.CommitSha = "commit-1"
	completion.ImageDigest = "other.example/repository@sha256:" + strings.Repeat("a", 64)
	if _, err := operations.CompleteBuild(builder, completion); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("foreign repository accepted: %v", err)
	}
	if len(notifier.agentIDs) != 0 {
		t.Fatal("invalid completion woke agents")
	}
	completion.ImageDigest = image
	if _, err := operations.CompleteBuild(builder, completion); err != nil {
		t.Fatal(err)
	}
	beforeLogs, beforeWakes := len(logWriter.Flatten()), len(notifier.agentIDs)
	if beforeWakes == 0 {
		t.Fatal("completion did not wake allocated agent")
	}
	if _, err := operations.CompleteBuild(builder, completion); err != nil {
		t.Fatal(err)
	}
	if len(logWriter.Flatten()) != beforeLogs || len(notifier.agentIDs) != beforeWakes {
		t.Fatal("repeated completion repeated post-commit work")
	}
}

func TestSourceSummaryIsMetadataOnlyAndBuilderDownloadStreams(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
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
			BuildRecipe:        &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatalf("seedReadySourceState: %v", err)
	}

	binding, err := store.source.SourceBindingByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.source.SourceRevisionByBindingAndCommit(ctx, binding.ID, "commit-1")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.source.SourceSnapshotByRevisionID(ctx, revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	archive := dockerfileMarkerArchive(string(bytes.Repeat([]byte("bounded-source-snapshot"), sourceSnapshotChunkBytes/8)) + "final-chunk")
	digest, objectKey, err := storeTestArchive(ctx, store, archive)
	if err != nil {
		t.Fatalf("storeSourceArchive: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE source_snapshots
		    SET object_key = $2, archive_size_bytes = $3, digest = $4, updated_at = $5
		  WHERE id = $1`,
		snapshot.ID, objectKey, len(archive), digest, time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}

	summary, err := sourceSummaryForTest(ctx, store, service.ID)
	if err != nil {
		t.Fatalf("loadServiceSourceSummaryQuerier: %v", err)
	}
	if summary.GetSourceState().GetLatestSnapshot().GetId() != snapshot.ID {
		t.Fatalf("unexpected source summary snapshot: %v", summary)
	}
	metadataOnly, err := store.source.SourceSnapshotByRevisionID(ctx, revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if metadataOnly.ArchiveSizeBytes != int64(len(archive)) {
		t.Fatalf("archive size = %d, want %d", metadataOnly.ArchiveSizeBytes, len(archive))
	}

	build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	stream := &recordingSourceSnapshotServerStream{
		ctx: contextWithClientIdentity(serviceCallerBuilder, "builder-1"),
	}
	if err := newBuilderService(newBuildOperations(store.db, store.source, testDelivery(store).Delivery, nil, nil)).DownloadSourceSnapshot(
		&platformv1.DownloadSourceSnapshotRequest{SnapshotId: snapshot.ID}, stream,
	); err != nil {
		t.Fatalf("DownloadSourceSnapshot: %v", err)
	}
	operations := newBuildOperations(store.db, store.source, testDelivery(store).Delivery, nil, nil)
	if _, _, err := operations.OpenSourceSnapshot(contextWithClientIdentity(serviceCallerBuilder, "builder-other"), snapshot.ID); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unassigned builder: %v", err)
	}
	metadata, reader, err := operations.OpenSourceSnapshot(stream.ctx, snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, err := io.ReadAll(reader)
	if err != nil || metadata.Digest != digest || !bytes.Equal(downloaded, archive) {
		t.Fatalf("open snapshot: metadata=%+v, err=%v", metadata, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE source_snapshots SET digest = $2 WHERE id = $1`, snapshot.ID, "sha256:"+string(bytes.Repeat([]byte("0"), 64))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := operations.OpenSourceSnapshot(stream.ctx, snapshot.ID); status.Code(err) != codes.DataLoss {
		t.Fatalf("corrupt digest: %v", err)
	}
	if len(stream.chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(stream.chunks))
	}
	var reconstructed []byte
	for i, chunk := range stream.chunks {
		if len(chunk.GetData()) == 0 || len(chunk.GetData()) > sourceSnapshotChunkBytes {
			t.Fatalf("chunk %d has invalid size %d", i, len(chunk.GetData()))
		}
		if chunk.GetOffset() != int64(len(reconstructed)) || chunk.GetTotalSize() != int64(len(archive)) || chunk.GetDigest() != digest {
			t.Fatalf("chunk %d has inconsistent metadata: %v", i, chunk)
		}
		reconstructed = append(reconstructed, chunk.GetData()...)
	}
	if !bytes.Equal(reconstructed, archive) {
		t.Fatal("streamed archive did not reconstruct the stored content")
	}
}

type recordingSourceSnapshotServerStream struct {
	ctx    context.Context
	chunks []*platformv1.SourceSnapshotChunk
}

func (s *recordingSourceSnapshotServerStream) Send(chunk *platformv1.SourceSnapshotChunk) error {
	clone := *chunk
	clone.Data = append([]byte(nil), chunk.GetData()...)
	s.chunks = append(s.chunks, &clone)
	return nil
}

func (s *recordingSourceSnapshotServerStream) SetHeader(metadata.MD) error { return nil }

func (s *recordingSourceSnapshotServerStream) SendHeader(metadata.MD) error { return nil }

func (s *recordingSourceSnapshotServerStream) SetTrailer(metadata.MD) {}

func (s *recordingSourceSnapshotServerStream) Context() context.Context { return s.ctx }

func (s *recordingSourceSnapshotServerStream) SendMsg(message any) error {
	chunk, ok := message.(*platformv1.SourceSnapshotChunk)
	if !ok {
		return io.ErrUnexpectedEOF
	}
	return s.Send(chunk)
}

func (s *recordingSourceSnapshotServerStream) RecvMsg(any) error { return io.EOF }
