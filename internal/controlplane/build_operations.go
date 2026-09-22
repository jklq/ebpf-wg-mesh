package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/source"
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
	"google.golang.org/protobuf/types/known/timestamppb"
)

func timestamppbNew(t time.Time) *timestamppb.Timestamp {
	return timestamppb.New(t.UTC())
}

type BuildOperations struct {
	builds      *buildsPersistence
	reads       deliverycore.ReadModel
	snapshots   source.SnapshotService
	delivery    *deliverycore.Delivery
	registry    buildRegistry
	credentials buildCredentials

	emitter *logs.LogEmitter
}

type buildRegistry interface {
	Enabled() bool
	PushRef(projectID, environmentID, buildID, serviceID, commitSHA string) string
}

type buildCredentials interface {
	CredentialsForBuild(ctx context.Context, projectID, buildID, pushRef string) (string, string, error)
}

type BuildOperationsOption func(*BuildOperations)

func WithBuilderLogEmitter(emitter *logs.LogEmitter) BuildOperationsOption {
	return func(s *BuildOperations) {
		s.emitter = emitter
	}
}

func NewBuildOperations(builds *buildsPersistence, reads deliverycore.ReadModel, snapshots source.SnapshotService, delivery *deliverycore.Delivery, registry buildRegistry, credentials buildCredentials, opts ...BuildOperationsOption) *BuildOperations {
	service := &BuildOperations{
		builds:      builds,
		reads:       reads,
		snapshots:   snapshots,
		delivery:    delivery,
		registry:    registry,
		credentials: credentials,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(service)
		}
	}
	return service
}

func (s *BuildOperations) ClaimBuild(ctx context.Context, req *platformv1.ClaimBuildRequest) (*platformv1.BuildJob, error) {
	caller, err := identity.ServiceCallerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	builderID, err := authenticatedBuilderID(caller, req.GetBuilderId())
	if err != nil {
		return nil, err
	}
	if s.builds == nil || s.reads == nil || s.delivery == nil || s.registry == nil || s.credentials == nil || !s.registry.Enabled() {
		return nil, status.Error(codes.FailedPrecondition, "builder dependencies are not configured")
	}
	build, err := s.delivery.ClaimNextBuild(ctx, builderID, req.GetBuilderName())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "claim build: %v", err)
	}
	if build.ID == "" {
		return &platformv1.BuildJob{}, nil
	}
	slog.InfoContext(ctx, "build claimed", "build_id", build.ID, "builder_id", builderID, "builder_name", req.GetBuilderName(), "service_id", build.ServiceID, "project_id", build.ProjectID, "commit_sha", build.CommitSHA, "lease_epoch", build.OwnerEpoch)
	service, err := s.reads.ServiceSnapshot(ctx, build.ServiceID)
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
	// The claimed attempt's recorded start is the event's stable time:
	// a retried claim of the same lease attempt collapses into one
	// build.started row instead of duplicating.
	startedAt := build.QueuedAt
	if build.StartedAt.Valid {
		startedAt = build.StartedAt.Time
	}
	s.emitter.EmitEvent(ctx, logs.ServiceScope{EnvironmentID: service.EnvironmentID, ServiceID: service.ID, RolloutGeneration: service.RolloutGeneration}, logs.LogTypeBuild, build.ID, build.OwnerEpoch, startedAt, logs.EventBuildStarted,
		fmt.Sprintf("Builder %s claimed build for commit %s", builderID, shortSHA(build.CommitSHA)),
		map[string]string{"builder_id": builderID, "commit_sha": build.CommitSHA})
	job := &platformv1.BuildJob{
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
		LeaseEpoch:            build.OwnerEpoch,
	}
	if build.LeaseExpiresAt.Valid {
		job.LeaseExpiresAt = timestamppbNew(build.LeaseExpiresAt.Time)
	}
	if build.DeadlineAt.Valid {
		job.DeadlineAt = timestamppbNew(build.DeadlineAt.Time)
	}
	return job, nil
}

func (s *BuildOperations) ReportBuildHeartbeat(ctx context.Context, req *platformv1.BuilderHeartbeatRequest) (*emptypb.Empty, error) {
	caller, err := identity.ServiceCallerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	builderID, err := authenticatedBuilderID(caller, req.GetBuilderId())
	if err != nil {
		return nil, err
	}
	if err := s.delivery.HeartbeatBuild(ctx, builderID, req.GetBuildId(), req.GetLeaseEpoch()); err != nil {
		if errors.Is(err, deliverycore.ErrBuildNotOwned) || errors.Is(err, deliverycore.ErrBuildLeaseLost) {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
		if errors.Is(err, deliverycore.ErrBuildCancelled) {
			return nil, status.Error(codes.Canceled, err.Error())
		}
		return nil, status.Errorf(codes.Internal, "builder heartbeat: %v", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *BuildOperations) ReportBuildLogs(ctx context.Context, req *platformv1.ReportBuildLogsRequest) (*emptypb.Empty, error) {
	caller, err := identity.ServiceCallerFromContext(ctx)
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
	if len(req.GetLines()) == 0 && len(req.GetDrops()) == 0 {
		return &emptypb.Empty{}, nil
	}
	build, err := s.reads.BuildByID(ctx, req.GetBuildId())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(codes.Internal, "build not found")
		}
		return nil, status.Errorf(codes.Internal, "load build for log report: %v", err)
	}
	if build.State != deliverycore.BuildStateRunning || build.BuilderID != builderID {
		return nil, status.Error(codes.PermissionDenied, "build is not assigned to this builder")
	}
	if build.OwnerEpoch != req.GetLeaseEpoch() {
		return nil, status.Error(codes.PermissionDenied, deliverycore.ErrBuildLeaseLost.Error())
	}
	service, err := s.reads.ServiceSnapshot(ctx, build.ServiceID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load service for log report: %v", err)
	}
	if s.emitter == nil || !s.emitter.Enabled() {
		return &emptypb.Empty{}, nil
	}
	if err := s.emitter.EmitBuildLines(ctx, logs.ServiceScope{EnvironmentID: service.EnvironmentID, ServiceID: service.ID, RolloutGeneration: service.RolloutGeneration}, build.ID, builderID, req.GetLines()); err != nil {
		return nil, status.Errorf(codes.Internal, "persist build logs: %v", err)
	}
	if err := s.emitter.EmitBuildDrops(ctx, logs.ServiceScope{EnvironmentID: service.EnvironmentID, ServiceID: service.ID, RolloutGeneration: service.RolloutGeneration}, build.ID, builderID, req.GetDrops()); err != nil {
		return nil, status.Errorf(codes.Internal, "persist build log gaps: %v", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *BuildOperations) CompleteBuild(ctx context.Context, req *platformv1.CompleteBuildRequest) (*emptypb.Empty, error) {
	caller, err := identity.ServiceCallerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	builderID, err := authenticatedBuilderID(caller, req.GetBuilderId())
	if err != nil {
		return nil, err
	}
	build, err := s.reads.BuildByID(ctx, req.GetBuildId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load build before completion: %v", err)
	}
	if deliverycore.BuildStateTerminal(build.State) {
		return &emptypb.Empty{}, nil
	}
	if build.State != deliverycore.BuildStateRunning {
		return nil, status.Error(codes.PermissionDenied, "build is not assigned to this builder")
	}
	if build.BuilderID != builderID {
		return nil, status.Error(codes.PermissionDenied, "build is not assigned to this builder")
	}
	if build.OwnerEpoch != req.GetLeaseEpoch() {
		return nil, status.Error(codes.PermissionDenied, deliverycore.ErrBuildLeaseLost.Error())
	}
	if req.GetCommitSha() != build.CommitSHA {
		return nil, status.Error(codes.InvalidArgument, "commit_sha does not match the claimed build")
	}
	service, err := s.reads.ServiceSnapshot(ctx, build.ServiceID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load service before completion: %v", err)
	}
	if req.GetState() == platformv1.BuildState_BUILD_STATE_SUCCEEDED {
		if s.registry == nil || !s.registry.Enabled() {
			return nil, status.Error(codes.FailedPrecondition, "registry policy is not configured")
		}
		pushRef := s.registry.PushRef(build.ProjectID, build.EnvironmentID, build.ID, build.ServiceID, build.CommitSHA)
		if err := validateRuntimeImageRef(pushRef, req.GetImageDigest()); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "image_digest: %v", err)
		}
	}
	completion, err := s.delivery.CompleteBuild(ctx, builderID, req.GetBuildId(), req.GetLeaseEpoch(), req.GetState(), req.GetCommitSha(), req.GetImageDigest(), req.GetFailureReason())
	if err != nil {
		if errors.Is(err, deliverycore.ErrBuildCommitMismatch) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if errors.Is(err, deliverycore.ErrBuildNotOwned) || errors.Is(err, deliverycore.ErrBuildLeaseLost) {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
		return nil, status.Errorf(codes.Internal, "complete build: %v", err)
	}
	if !completion.Changed {
		return &emptypb.Empty{}, nil
	}
	build = completion.Build
	if build.State == deliverycore.BuildStateCancelled {
		return &emptypb.Empty{}, nil
	}
	var allocationAgentIDs []string
	if req.GetState() == platformv1.BuildState_BUILD_STATE_SUCCEEDED {
		allocations, err := s.reads.ListAllocationsByServiceID(ctx, build.ServiceID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "load build allocations after completion: %v", err)
		}
		seen := make(map[string]struct{}, len(allocations))
		for _, allocation := range allocations {
			if allocation.AgentID == "" {
				continue
			}
			if _, ok := seen[allocation.AgentID]; ok {
				continue
			}
			seen[allocation.AgentID] = struct{}{}
			allocationAgentIDs = append(allocationAgentIDs, allocation.AgentID)
		}
	}
	slog.InfoContext(ctx, "build completed", "build_id", req.GetBuildId(), "builder_id", builderID, "state", req.GetState().String(), "recorded_state", build.State, "commit_sha", req.GetCommitSha(), "image_digest", req.GetImageDigest(), "failure_reason", req.GetFailureReason())

	scope := logs.ServiceScope{EnvironmentID: service.EnvironmentID, ServiceID: service.ID, RolloutGeneration: service.RolloutGeneration}
	// Completion events identify by the recorded transition time so a
	// re-delivered report collapses instead of duplicating.
	finishedAt := time.Now().UTC()
	if build.FinishedAt.Valid {
		finishedAt = build.FinishedAt.Time
	}
	switch req.GetState() {
	case platformv1.BuildState_BUILD_STATE_SUCCEEDED:
		if build.State == deliverycore.BuildStateSuperseded {
			s.emitter.EmitEvent(ctx, scope, logs.LogTypeBuild, build.ID, build.OwnerEpoch, finishedAt, logs.EventBuildFinished, "Build superseded by a newer commit", map[string]string{"outcome": "superseded"})
			break
		}
		s.emitter.EmitEvent(ctx, scope, logs.LogTypeBuild, build.ID, build.OwnerEpoch, finishedAt, logs.EventBuildFinished,
			fmt.Sprintf("Image build succeeded for commit %s (digest %s)", shortSHA(req.GetCommitSha()), shortDigest(req.GetImageDigest())),
			map[string]string{"outcome": "succeeded", "commit_sha": req.GetCommitSha(), "image_digest": req.GetImageDigest()})
		target := strings.Join(allocationAgentIDs, ", ")
		if target == "" {
			target = "pending placement"
		}
		if completion.RolloutScheduled {
			s.emitter.EmitEvent(ctx, logs.ServiceScope{EnvironmentID: service.EnvironmentID, ServiceID: service.ID, RolloutGeneration: service.RolloutGeneration, AgentID: service.AllocatedAgentID}, logs.LogTypeDeploy, req.GetBuildId(), build.OwnerEpoch, finishedAt, logs.EventDeployStarted,
				fmt.Sprintf("Scheduling rollout to agent %s", target),
				map[string]string{"target_agents": target})
		}
	case platformv1.BuildState_BUILD_STATE_FAILED:
		reason := strings.TrimSpace(req.GetFailureReason())
		if reason == "" {
			reason = "no reason provided"
		}
		s.emitter.EmitEvent(ctx, scope, logs.LogTypeBuild, build.ID, build.OwnerEpoch, finishedAt, logs.EventBuildFinished,
			fmt.Sprintf("Image build failed: %s", reason),
			map[string]string{"outcome": "failed", "reason": reason})
	case platformv1.BuildState_BUILD_STATE_SUPERSEDED:
		s.emitter.EmitEvent(ctx, scope, logs.LogTypeBuild, build.ID, build.OwnerEpoch, finishedAt, logs.EventBuildFinished, "Build superseded by a newer commit", map[string]string{"outcome": "superseded"})
	}

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

func authenticatedBuilderID(caller identity.ServiceCaller, requested string) (string, error) {
	if caller.Class != identity.CallerBuilder {
		return "", status.Error(codes.PermissionDenied, "builder client certificate required")
	}
	requested = strings.TrimSpace(requested)
	if requested != "" && requested != caller.ID {
		return "", status.Error(codes.PermissionDenied, "builder_id does not match client certificate")
	}
	return caller.ID, nil
}

func buildJobSourceFromRecord(rec deliverycore.BuildRunRecord) *platformv1.BuildJobSource {
	if rec.SourceRevisionID == "" && rec.SourceSnapshotID == "" {
		return nil
	}
	return &platformv1.BuildJobSource{
		SourceRevisionId:     rec.SourceRevisionID,
		SourceSnapshotId:     rec.SourceSnapshotID,
		SourceSnapshotDigest: rec.SourceSnapshotDigest,
		BuildRecipe:          source.CloneBuildRecipe(rec.BuildRecipe),
	}
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

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
