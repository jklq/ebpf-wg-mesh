//go:build integration

package controlplane

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestSourceSummaryIsMetadataOnlyAndBuilderDownloadStreams(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"))
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
	archive := bytes.Repeat([]byte("bounded-source-snapshot"), sourceSnapshotChunkBytes/8)
	archive = append(archive, []byte("final-chunk")...)
	digest, objectKey, err := store.source.StoreSourceArchive(ctx, archive)
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
	if err := NewBuilderService(NewBuildOperations(store.builds, store.reads, store.source, nil, nil, nil, 0)).DownloadSourceSnapshot(
		&platformv1.DownloadSourceSnapshotRequest{SnapshotId: snapshot.ID}, stream,
	); err != nil {
		t.Fatalf("DownloadSourceSnapshot: %v", err)
	}
	operations := NewBuildOperations(store.builds, store.reads, store.source, nil, nil, nil, 0)
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
	_, reader, err = operations.OpenSourceSnapshot(stream.ctx, snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); status.Code(err) != codes.DataLoss {
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
