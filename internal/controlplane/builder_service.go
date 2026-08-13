package controlplane

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

const sourceSnapshotChunkBytes = 64 << 10

type BuilderService struct {
	platformv1.UnimplementedBuilderServiceServer
	store    *Store
	notifier interface {
		Notify(agentID string)
	}
	registry interface {
		Enabled() bool
		PushRef(projectID, environmentID, buildID, serviceID, commitSHA string) string
	}
	credentials interface {
		CredentialsForBuild(ctx context.Context, projectID, buildID, pushRef string) (string, string, error)
	}
	emitter    *LogEmitter
	events     *PlatformEvents
	staleAfter time.Duration
}

// BuilderServiceOption configures optional dependencies for BuilderService. We
// use a variadic option list so that wiring (for example, log capture) stays
// opt-in for tests and stubs that do not need the full production stack.
type BuilderServiceOption func(*BuilderService)

// WithBuilderLogEmitter wires a LogEmitter into the BuilderService so that
// build state transitions (claim, complete) persist human-readable log lines
// under the "build" log type. A nil emitter is treated as a no-op.
func WithBuilderLogEmitter(emitter *LogEmitter) BuilderServiceOption {
	return func(s *BuilderService) {
		s.emitter = emitter
	}
}

func WithBuilderPlatformEvents(events *PlatformEvents) BuilderServiceOption {
	return func(s *BuilderService) {
		s.events = events
	}
}

func NewBuilderService(store *Store, notifier interface{ Notify(agentID string) }, registry *RegistryPolicy, staleAfter time.Duration, opts ...BuilderServiceOption) *BuilderService {
	service := &BuilderService{
		store:       store,
		notifier:    notifier,
		registry:    registry,
		credentials: registry,
		staleAfter:  staleAfter,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(service)
		}
	}
	return service
}

func (s *BuilderService) ClaimBuild(ctx context.Context, req *platformv1.ClaimBuildRequest) (*platformv1.BuildJob, error) {
	caller, err := ServiceCallerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	builderID, err := authenticatedBuilderID(caller, req.GetBuilderId())
	if err != nil {
		return nil, err
	}
	build, err := s.store.claimNextBuild(ctx, builderID, req.GetBuilderName(), s.staleAfter)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "claim build: %v", err)
	}
	if build.ID == "" {
		return &platformv1.BuildJob{}, nil
	}
	slog.InfoContext(ctx, "build claimed", "build_id", build.ID, "builder_id", builderID, "builder_name", req.GetBuilderName(), "service_id", build.ServiceID, "project_id", build.ProjectID, "commit_sha", build.CommitSHA)
	s.events.Publish(build.EnvironmentID)
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
	pushRef := s.registry.PushRef(service.ProjectID, service.EnvironmentID, build.ID, service.ID, build.CommitSHA)
	registryUsername, registryPassword, err := s.credentials.CredentialsForBuild(ctx, service.ProjectID, build.ID, pushRef)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve registry credentials: %v", err)
	}
	s.emitter.EmitBuildf(ctx, service, build, StageBuild, "Builder %s claimed build for commit %s", builderID, shortSHA(build.CommitSHA))
	return &platformv1.BuildJob{
		BuildId:               build.ID,
		ServiceId:             service.ID,
		ProjectId:             service.ProjectID,
		EnvironmentId:         service.EnvironmentID,
		ServiceName:           service.Name,
		CommitSha:             build.CommitSHA,
		Source:                buildJobSourceFromRecord(build),
		RegistryPushReference: pushRef,
		RegistryUsername:      registryUsername,
		RegistryPassword:      registryPassword,
	}, nil
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
	owned, err := s.store.builderOwnsSourceSnapshot(ctx, builderID, snapshotID)
	if err != nil {
		return status.Errorf(codes.Internal, "authorize source snapshot: %v", err)
	}
	if !owned {
		return status.Error(codes.PermissionDenied, "source snapshot is not assigned to this builder")
	}
	snapshot, err := s.store.sourceSnapshotByID(ctx, snapshotID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return status.Error(codes.NotFound, "source snapshot not found")
		}
		return status.Errorf(codes.Internal, "load source snapshot metadata: %v", err)
	}
	if err := ensureReadySnapshot(snapshot); err != nil {
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
		chunk, err := s.store.sourceSnapshotArchiveChunk(ctx, snapshot.ID, offset, limit)
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
	caller, err := ServiceCallerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	builderID, err := authenticatedBuilderID(caller, req.GetBuilderId())
	if err != nil {
		return nil, err
	}
	if err := s.store.recordBuilderHeartbeat(ctx, builderID, req.GetBuildId()); err != nil {
		if errors.Is(err, errBuildNotOwned) {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
		return nil, status.Errorf(codes.Internal, "builder heartbeat: %v", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *BuilderService) ReportBuildLogs(ctx context.Context, req *platformv1.ReportBuildLogsRequest) (*emptypb.Empty, error) {
	caller, err := ServiceCallerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	builderID, err := authenticatedBuilderID(caller, req.GetBuilderId())
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetBuildId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "build id is required")
	}
	if len(req.GetLines()) == 0 {
		return &emptypb.Empty{}, nil
	}
	build, err := s.store.buildRunByIDQuerier(ctx, s.store.db, req.GetBuildId())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(codes.Internal, "build not found")
		}
		return nil, status.Errorf(codes.Internal, "load build for log report: %v", err)
	}
	if build.State != buildStateRunning || build.BuilderID != builderID {
		return nil, status.Error(codes.PermissionDenied, "build is not assigned to this builder")
	}
	service, err := s.store.serviceByIDInternalQuerier(ctx, s.store.db, build.ServiceID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load service for log report: %v", err)
	}
	if s.emitter == nil || !s.emitter.Enabled() {
		return &emptypb.Empty{}, nil
	}
	if err := s.emitter.EmitBuildLines(ctx, service, build, builderID, req.GetLines()); err != nil {
		return nil, status.Errorf(codes.Internal, "persist build logs: %v", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *BuilderService) CompleteBuild(ctx context.Context, req *platformv1.CompleteBuildRequest) (*emptypb.Empty, error) {
	caller, err := ServiceCallerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	builderID, err := authenticatedBuilderID(caller, req.GetBuilderId())
	if err != nil {
		return nil, err
	}
	// Load build + service up-front so we can emit synthetic logs keyed to
	// the correct service/allocation regardless of the terminal state.
	build, err := s.store.buildRunByIDQuerier(ctx, s.store.db, req.GetBuildId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load build before completion: %v", err)
	}
	if build.State != buildStateRunning || build.BuilderID != builderID {
		return nil, status.Error(codes.PermissionDenied, "build is not assigned to this builder")
	}
	if req.GetCommitSha() != build.CommitSHA {
		return nil, status.Error(codes.InvalidArgument, "commit_sha does not match the claimed build")
	}
	service, err := s.store.serviceByIDInternalQuerier(ctx, s.store.db, build.ServiceID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load service before completion: %v", err)
	}
	var agentID string
	if req.GetState() == platformv1.BuildState_BUILD_STATE_SUCCEEDED {
		if s.registry == nil || !s.registry.Enabled() {
			return nil, status.Error(codes.FailedPrecondition, "registry policy is not configured")
		}
		pushRef := s.registry.PushRef(build.ProjectID, build.EnvironmentID, build.ID, build.ServiceID, build.CommitSHA)
		if err := validateRuntimeImageRef(pushRef, req.GetImageDigest()); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "image_digest: %v", err)
		}
		agentID = service.AllocatedAgentID
	}
	if err := s.store.completeBuild(ctx, builderID, req.GetBuildId(), req.GetState(), req.GetCommitSha(), req.GetImageDigest(), req.GetFailureReason()); err != nil {
		if errors.Is(err, errBuildNotOwned) {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
		return nil, status.Errorf(codes.Internal, "complete build: %v", err)
	}
	slog.InfoContext(ctx, "build completed", "build_id", req.GetBuildId(), "builder_id", builderID, "state", req.GetState().String(), "commit_sha", req.GetCommitSha(), "image_digest", req.GetImageDigest(), "failure_reason", req.GetFailureReason())

	switch req.GetState() {
	case platformv1.BuildState_BUILD_STATE_SUCCEEDED:
		s.emitter.EmitBuildf(ctx, service, build, StageBuild, "Image build succeeded for commit %s (digest %s)", shortSHA(req.GetCommitSha()), shortDigest(req.GetImageDigest()))
		// We synthesize a deploy-stage line so the "Deploy" tab shows
		// activity immediately even before the agent applies the new
		// rollout; the runtime condition stream later adds more detail.
		s.emitter.EmitDeployf(ctx, service, "", req.GetBuildId(), StageDeploy, "Scheduling rollout to agent %s", service.AllocatedAgentID)
	case platformv1.BuildState_BUILD_STATE_FAILED:
		reason := strings.TrimSpace(req.GetFailureReason())
		if reason == "" {
			reason = "no reason provided"
		}
		s.emitter.EmitBuildf(ctx, service, build, StageBuild, "Image build failed: %s", reason)
	case platformv1.BuildState_BUILD_STATE_SUPERSEDED:
		s.emitter.EmitBuild(ctx, service, build, StageBuild, "Build superseded by a newer commit")
	}

	if agentID != "" && s.notifier != nil {
		s.notifier.Notify(agentID)
	}
	s.events.Publish(build.EnvironmentID)
	return &emptypb.Empty{}, nil
}

func validateRuntimeImageRef(pushRef, imageRef string) error {
	lastSlash := strings.LastIndexByte(pushRef, '/')
	tagSeparator := strings.LastIndexByte(pushRef, ':')
	if tagSeparator <= lastSlash {
		return errors.New("assigned push reference has no tag")
	}
	repository := pushRef[:tagSeparator]
	prefix := repository + "@"
	if !strings.HasPrefix(imageRef, prefix) {
		return fmt.Errorf("must reference assigned repository %q", repository)
	}
	digest := strings.TrimPrefix(imageRef, prefix)
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return errors.New("must contain a full sha256 digest")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:")); err != nil {
		return errors.New("sha256 digest is not hexadecimal")
	}
	return nil
}

func authenticatedBuilderID(caller ServiceCaller, requested string) (string, error) {
	if caller.Class != serviceCallerBuilder {
		return "", status.Error(codes.PermissionDenied, "builder client certificate required")
	}
	requested = strings.TrimSpace(requested)
	if requested != "" && requested != caller.ID {
		return "", status.Error(codes.PermissionDenied, "builder_id does not match client certificate")
	}
	return caller.ID, nil
}

// shortSHA truncates a git SHA for human-friendly log lines. We keep the first
// 7 hex characters which is git's default short-hash width and enough to
// disambiguate commits in practical cases.
func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

// shortDigest trims a `sha256:…` image digest down to a human-sized prefix so
// log lines stay readable in the terminal-style log viewer.
func shortDigest(digest string) string {
	digest = strings.TrimSpace(digest)
	if colon := strings.Index(digest, ":"); colon >= 0 && colon+12 < len(digest) {
		return digest[:colon+13]
	}
	if len(digest) > 19 {
		return digest[:19]
	}
	return digest
}
