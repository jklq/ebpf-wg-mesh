package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type BuilderService struct {
	platformv1.UnimplementedBuilderServiceServer
	store    *Store
	notifier interface {
		Notify(agentID string)
	}
	registry interface {
		Enabled() bool
		PushRef(projectID, serviceID, commitSHA string) string
	}
	credentials interface {
		CredentialsForPushRef(pushRef string) (string, string, error)
	}
	staleAfter time.Duration
}

func NewBuilderService(store *Store, notifier interface{ Notify(agentID string) }, registry *RegistryPolicy, staleAfter time.Duration) *BuilderService {
	return &BuilderService{
		store:       store,
		notifier:    notifier,
		registry:    registry,
		credentials: registry,
		staleAfter:  staleAfter,
	}
}

func (s *BuilderService) ClaimBuild(ctx context.Context, req *platformv1.ClaimBuildRequest) (*platformv1.BuildJob, error) {
	caller, err := ServiceCallerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	builderID := req.GetBuilderId()
	if builderID == "" {
		builderID = caller.ID
	}
	build, err := s.store.claimNextBuild(ctx, builderID, req.GetBuilderName(), s.staleAfter)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "claim build: %v", err)
	}
	if build.ID == "" {
		return &platformv1.BuildJob{}, nil
	}
	slog.InfoContext(ctx, "build claimed", "build_id", build.ID, "builder_id", builderID, "builder_name", req.GetBuilderName(), "service_id", build.ServiceID, "project_id", build.ProjectID, "commit_sha", build.CommitSHA)
	if s.store == nil || s.registry == nil || s.credentials == nil || !s.registry.Enabled() {
		return nil, status.Error(codes.FailedPrecondition, "builder dependencies are not configured")
	}
	service, err := s.store.serviceByIDInternalQuerier(ctx, s.store.db, build.ServiceID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load build service: %v", err)
	}
	if build.SourceSnapshotID == "" {
		return nil, status.Error(codes.FailedPrecondition, "build source snapshot is missing")
	}
	pushRef := s.registry.PushRef(service.ProjectID, service.ID, build.CommitSHA)
	registryUsername, registryPassword, err := s.credentials.CredentialsForPushRef(pushRef)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve registry credentials: %v", err)
	}
	return &platformv1.BuildJob{
		BuildId:               build.ID,
		ServiceId:             service.ID,
		ProjectId:             service.ProjectID,
		ServiceName:           service.Name,
		CommitSha:             build.CommitSHA,
		Source:                buildJobSourceFromRecord(build),
		RegistryPushReference: pushRef,
		RegistryUsername:      registryUsername,
		RegistryPassword:      registryPassword,
	}, nil
}

func (s *BuilderService) DownloadSourceSnapshot(ctx context.Context, req *platformv1.DownloadSourceSnapshotRequest) (*platformv1.SourceSnapshotArtifact, error) {
	if _, err := ServiceCallerFromContext(ctx); err != nil {
		return nil, err
	}
	snapshotID := req.GetSnapshotId()
	if snapshotID == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot id is required")
	}
	snapshot, err := s.store.sourceSnapshotByID(ctx, snapshotID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "source snapshot not found")
		}
		return nil, status.Errorf(codes.Internal, "load source snapshot: %v", err)
	}
	if err := ensureReadySnapshot(snapshot); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "source snapshot not ready: %v", err)
	}
	return &platformv1.SourceSnapshotArtifact{
		SnapshotId: snapshot.ID,
		Digest:     snapshot.Digest,
		ArchiveTgz: snapshot.ArchiveTGZ,
	}, nil
}

func (s *BuilderService) ReportBuildHeartbeat(ctx context.Context, req *platformv1.BuilderHeartbeatRequest) (*emptypb.Empty, error) {
	caller, err := ServiceCallerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	builderID := req.GetBuilderId()
	if builderID == "" {
		builderID = caller.ID
	}
	if err := s.store.recordBuilderHeartbeat(ctx, builderID, req.GetBuildId()); err != nil {
		return nil, status.Errorf(codes.Internal, "builder heartbeat: %v", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *BuilderService) CompleteBuild(ctx context.Context, req *platformv1.CompleteBuildRequest) (*emptypb.Empty, error) {
	caller, err := ServiceCallerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	builderID := req.GetBuilderId()
	if builderID == "" {
		builderID = caller.ID
	}
	var agentID string
	if req.GetState() == platformv1.BuildState_BUILD_STATE_SUCCEEDED {
		build, err := s.store.buildRunByIDQuerier(ctx, s.store.db, req.GetBuildId())
		if err != nil {
			return nil, status.Errorf(codes.Internal, "load build before completion: %v", err)
		}
		service, err := s.store.serviceByIDInternalQuerier(ctx, s.store.db, build.ServiceID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "load service before completion: %v", err)
		}
		agentID = service.AllocatedAgentID
	}
	if err := s.store.completeBuild(ctx, builderID, req.GetBuildId(), req.GetState(), req.GetCommitSha(), req.GetImageDigest(), req.GetFailureReason()); err != nil {
		return nil, status.Errorf(codes.Internal, "complete build: %v", err)
	}
	slog.InfoContext(ctx, "build completed", "build_id", req.GetBuildId(), "builder_id", builderID, "state", req.GetState().String(), "commit_sha", req.GetCommitSha(), "image_digest", req.GetImageDigest(), "failure_reason", req.GetFailureReason())
	if agentID != "" && s.notifier != nil {
		s.notifier.Notify(agentID)
	}
	return &emptypb.Empty{}, nil
}
