package controlplane

import (
	"context"
	"io"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/identity"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

const sourceSnapshotChunkBytes = 64 << 10

type BuilderService struct {
	platformv1.UnimplementedBuilderServiceServer
	operations builderOperations
}

type builderOperations interface {
	ClaimBuild(context.Context, *platformv1.ClaimBuildRequest) (*platformv1.BuildJob, error)
	OpenSourceSnapshot(context.Context, string) (SourceSnapshotMetadata, io.Reader, error)
	ReportBuildHeartbeat(context.Context, *platformv1.BuilderHeartbeatRequest) (*emptypb.Empty, error)
	ReportBuildLogs(context.Context, *platformv1.ReportBuildLogsRequest) (*emptypb.Empty, error)
	CompleteBuild(context.Context, *platformv1.CompleteBuildRequest) (*emptypb.Empty, error)
}

func NewBuilderService(operations builderOperations) *BuilderService {
	return &BuilderService{operations: operations}
}

func (s *BuilderService) ClaimBuild(ctx context.Context, req *platformv1.ClaimBuildRequest) (*platformv1.BuildJob, error) {
	if _, err := identity.ServiceCallerFromContext(ctx); err != nil {
		return nil, err
	}
	return s.operations.ClaimBuild(ctx, req)
}

func (s *BuilderService) DownloadSourceSnapshot(req *platformv1.DownloadSourceSnapshotRequest, stream platformv1.BuilderService_DownloadSourceSnapshotServer) error {
	snapshot, reader, err := s.operations.OpenSourceSnapshot(stream.Context(), req.GetSnapshotId())
	if err != nil {
		return err
	}
	buffer := make([]byte, sourceSnapshotChunkBytes)
	for offset := int64(0); ; {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			if err := stream.Send(&platformv1.SourceSnapshotChunk{
				SnapshotId: snapshot.ID, Digest: snapshot.Digest, TotalSize: snapshot.ArchiveSizeBytes,
				Offset: offset, Data: append([]byte(nil), buffer[:n]...),
			}); err != nil {
				return err
			}
			offset += int64(n)
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if n == 0 {
			return status.Error(codes.DataLoss, "source snapshot reader made no progress")
		}
	}
}

func (s *BuilderService) ReportBuildHeartbeat(ctx context.Context, req *platformv1.BuilderHeartbeatRequest) (*emptypb.Empty, error) {
	if _, err := identity.ServiceCallerFromContext(ctx); err != nil {
		return nil, err
	}
	return s.operations.ReportBuildHeartbeat(ctx, req)
}

func (s *BuilderService) ReportBuildLogs(ctx context.Context, req *platformv1.ReportBuildLogsRequest) (*emptypb.Empty, error) {
	if _, err := identity.ServiceCallerFromContext(ctx); err != nil {
		return nil, err
	}
	return s.operations.ReportBuildLogs(ctx, req)
}

func (s *BuilderService) CompleteBuild(ctx context.Context, req *platformv1.CompleteBuildRequest) (*emptypb.Empty, error) {
	if _, err := identity.ServiceCallerFromContext(ctx); err != nil {
		return nil, err
	}
	return s.operations.CompleteBuild(ctx, req)
}
