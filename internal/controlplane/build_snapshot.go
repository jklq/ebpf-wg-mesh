package controlplane

import (
	"context"
	"errors"
	"io"

	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/source"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SourceSnapshotMetadata describes the archive returned to a builder.
type SourceSnapshotMetadata struct {
	ID               string
	Digest           string
	ArchiveSizeBytes int64
}

// OpenSourceSnapshot authorizes the builder and returns a bounded, digest-verified archive reader.
func (s *BuildOperations) OpenSourceSnapshot(ctx context.Context, snapshotID string) (SourceSnapshotMetadata, io.Reader, error) {
	caller, err := identity.ServiceCallerFromContext(ctx)
	if err != nil {
		return SourceSnapshotMetadata{}, nil, err
	}
	builderID, err := authenticatedBuilderID(caller, "")
	if err != nil {
		return SourceSnapshotMetadata{}, nil, err
	}

	if snapshotID == "" {
		return SourceSnapshotMetadata{}, nil, status.Error(codes.InvalidArgument, "snapshot id is required")
	}
	owned, err := s.builds.builderOwnsSourceSnapshot(ctx, builderID, snapshotID)
	if err != nil {
		return SourceSnapshotMetadata{}, nil, status.Errorf(codes.Internal, "authorize source snapshot: %v", err)
	}
	if !owned {
		return SourceSnapshotMetadata{}, nil, status.Error(codes.PermissionDenied, "source snapshot is not assigned to this builder")
	}
	if s.snapshots == nil {
		return SourceSnapshotMetadata{}, nil, status.Error(codes.Internal, "source snapshot archive is unavailable")
	}
	metadata, reader, err := s.snapshots.OpenSnapshotArchive(ctx, snapshotID)
	if err != nil {
		switch {
		case errors.Is(err, source.ErrSnapshotNotFound):
			return SourceSnapshotMetadata{}, nil, status.Error(codes.NotFound, "source snapshot not found")
		case errors.Is(err, source.ErrSnapshotNotReady):
			return SourceSnapshotMetadata{}, nil, status.Errorf(codes.FailedPrecondition, "source snapshot not ready: %v", err)
		case errors.Is(err, source.ErrSnapshotTooLarge):
			return SourceSnapshotMetadata{}, nil, status.Error(codes.ResourceExhausted, "source snapshot exceeds compressed size limit")
		case errors.Is(err, source.ErrSnapshotCorrupt):
			return SourceSnapshotMetadata{}, nil, status.Error(codes.DataLoss, "source snapshot digest is invalid")
		case errors.Is(err, source.ErrSnapshotUnavailable):
			return SourceSnapshotMetadata{}, nil, status.Error(codes.Internal, "source snapshot archive is unavailable")
		default:
			return SourceSnapshotMetadata{}, nil, status.Errorf(codes.Internal, "load source snapshot metadata: %v", err)
		}
	}
	out := SourceSnapshotMetadata{ID: metadata.ID, Digest: metadata.Digest, ArchiveSizeBytes: metadata.ArchiveSizeBytes}
	return out, snapshotReadWrapper{reader: reader}, nil
}

// snapshotReadWrapper maps source read failures to gRPC codes so a mid-stream
// digest failure stays DataLoss instead of surfacing as Internal.
type snapshotReadWrapper struct {
	reader io.Reader
}

func (w snapshotReadWrapper) Read(p []byte) (int, error) {
	n, err := w.reader.Read(p)
	if err == nil || errors.Is(err, io.EOF) {
		return n, err
	}
	if errors.Is(err, source.ErrSnapshotCorrupt) {
		return n, status.Error(codes.DataLoss, err.Error())
	}
	return n, status.Errorf(codes.Internal, "read source snapshot chunk: %v", err)
}
