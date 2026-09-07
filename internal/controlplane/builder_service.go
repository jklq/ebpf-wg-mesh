package controlplane

import (
	"context"
	"crypto/sha256"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"encoding/hex"
	"errors"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

const sourceSnapshotChunkBytes = 64 << 10

type BuilderService struct {
	platformv1.UnimplementedBuilderServiceServer
	operations *BuildOperations
}

func NewBuilderService(operations *BuildOperations) *BuilderService {
	return &BuilderService{operations: operations}
}

func (s *BuilderService) ClaimBuild(ctx context.Context, req *platformv1.ClaimBuildRequest) (*platformv1.BuildJob, error) {
	if _, err := ServiceCallerFromContext(ctx); err != nil {
		return nil, err
	}
	return s.operations.ClaimBuild(ctx, req)
}

func (s *BuilderService) DownloadSourceSnapshot(req *platformv1.DownloadSourceSnapshotRequest, stream platformv1.BuilderService_DownloadSourceSnapshotServer) error {
	ctx := stream.Context()
	caller, err := ServiceCallerFromContext(ctx)
	if err != nil {
		return err
	}
	builderID, err := authenticatedBuilderID(caller, "")
	if err != nil {
		return err
	}
	snapshotID := req.GetSnapshotId()
	if snapshotID == "" {
		return status.Error(codes.InvalidArgument, "snapshot id is required")
	}
	owned, err := s.operations.store.builderOwnsSourceSnapshot(ctx, builderID, snapshotID)
	if err != nil {
		return status.Errorf(codes.Internal, "authorize source snapshot: %v", err)
	}
	if !owned {
		return status.Error(codes.PermissionDenied, "source snapshot is not assigned to this builder")
	}
	snapshot, err := s.operations.store.sourceSnapshotByID(ctx, snapshotID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return status.Error(codes.NotFound, "source snapshot not found")
		}
		return status.Errorf(codes.Internal, "load source snapshot metadata: %v", err)
	}
	if err := deliverycore.EnsureReadySnapshot(snapshot); err != nil {
		return status.Errorf(codes.FailedPrecondition, "source snapshot not ready: %v", err)
	}
	if snapshot.ArchiveSizeBytes > maxSourceArchiveCompressedBytes {
		return status.Error(codes.ResourceExhausted, "source snapshot exceeds compressed size limit")
	}
	if !strings.HasPrefix(snapshot.Digest, "sha256:") || len(snapshot.Digest) != len("sha256:")+sha256.Size*2 {
		return status.Error(codes.DataLoss, "source snapshot digest is invalid")
	}

	hash := sha256.New()
	for offset := int64(0); offset < snapshot.ArchiveSizeBytes; {
		remaining := snapshot.ArchiveSizeBytes - offset
		limit := sourceSnapshotChunkBytes
		if remaining < int64(limit) {
			limit = int(remaining)
		}
		chunk, err := s.operations.store.sourceSnapshotArchiveChunk(ctx, snapshot.ID, offset, limit)
		if err != nil {
			return status.Errorf(codes.Internal, "read source snapshot chunk: %v", err)
		}
		if len(chunk) == 0 || len(chunk) > limit {
			return status.Error(codes.DataLoss, "source snapshot archive changed while streaming")
		}
		if _, err := hash.Write(chunk); err != nil {
			return status.Errorf(codes.Internal, "hash source snapshot chunk: %v", err)
		}
		if err := stream.Send(&platformv1.SourceSnapshotChunk{
			SnapshotId: snapshot.ID,
			Digest:     snapshot.Digest,
			TotalSize:  snapshot.ArchiveSizeBytes,
			Offset:     offset,
			Data:       chunk,
		}); err != nil {
			return err
		}
		offset += int64(len(chunk))
	}
	actualDigest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actualDigest != snapshot.Digest {
		return status.Error(codes.DataLoss, "source snapshot digest verification failed")
	}
	return nil
}

func (s *BuilderService) ReportBuildHeartbeat(ctx context.Context, req *platformv1.BuilderHeartbeatRequest) (*emptypb.Empty, error) {
	if _, err := ServiceCallerFromContext(ctx); err != nil {
		return nil, err
	}
	return s.operations.ReportBuildHeartbeat(ctx, req)
}

func (s *BuilderService) ReportBuildLogs(ctx context.Context, req *platformv1.ReportBuildLogsRequest) (*emptypb.Empty, error) {
	if _, err := ServiceCallerFromContext(ctx); err != nil {
		return nil, err
	}
	return s.operations.ReportBuildLogs(ctx, req)
}

func (s *BuilderService) CompleteBuild(ctx context.Context, req *platformv1.CompleteBuildRequest) (*emptypb.Empty, error) {
	if _, err := ServiceCallerFromContext(ctx); err != nil {
		return nil, err
	}
	return s.operations.CompleteBuild(ctx, req)
}
