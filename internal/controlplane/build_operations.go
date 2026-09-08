package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
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

// BuildOperations owns builder authorization, job preparation, and committed build effects.
type BuildOperations struct {
	store       *buildsPersistence
	delivery    *deliverycore.Delivery
	registry    buildRegistry
	credentials buildCredentials

	emitter    *LogEmitter
	staleAfter time.Duration
}

type buildRegistry interface {
	Enabled() bool
	PushRef(projectID, environmentID, buildID, serviceID, commitSHA string) string
}

type buildCredentials interface {
	CredentialsForBuild(ctx context.Context, projectID, buildID, pushRef string) (string, string, error)
}

// BuildOperationsOption configures optional build observability.
type BuildOperationsOption func(*BuildOperations)

// WithBuilderLogEmitter wires a LogEmitter into BuildOperations so that
// build state transitions (claim, complete) persist human-readable log lines
// under the "build" log type. A nil emitter is treated as a no-op.
func WithBuilderLogEmitter(emitter *LogEmitter) BuildOperationsOption {
	return func(s *BuildOperations) {
		s.emitter = emitter
	}
}

func NewBuildOperations(store *buildsPersistence, delivery *deliverycore.Delivery, registry buildRegistry, credentials buildCredentials, staleAfter time.Duration, opts ...BuildOperationsOption) *BuildOperations {
	service := &BuildOperations{
		store:       store,
		delivery:    delivery,
		registry:    registry,
		credentials: credentials,
		staleAfter:  staleAfter,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(service)
		}
	}
	return service
}

func (s *BuildOperations) ClaimBuild(ctx context.Context, req *platformv1.ClaimBuildRequest) (*platformv1.BuildJob, error) {
	caller, err := ServiceCallerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	builderID, err := authenticatedBuilderID(caller, req.GetBuilderId())
	if err != nil {
		return nil, err
	}
	if s.store == nil || s.delivery == nil || s.registry == nil || s.credentials == nil || !s.registry.Enabled() {
		return nil, status.Error(codes.FailedPrecondition, "builder dependencies are not configured")
	}
	build, err := s.delivery.ClaimNextBuild(ctx, builderID, req.GetBuilderName(), s.staleAfter)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "claim build: %v", err)
	}
	if build.ID == "" {
		return &platformv1.BuildJob{}, nil
	}
	slog.InfoContext(ctx, "build claimed", "build_id", build.ID, "builder_id", builderID, "builder_name", req.GetBuilderName(), "service_id", build.ServiceID, "project_id", build.ProjectID, "commit_sha", build.CommitSHA)
	service, err := s.store.reads.deliveryQueries().ServiceByIDInternalQuerier(ctx, s.store.db, build.ServiceID)
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

func (s *BuildOperations) ReportBuildHeartbeat(ctx context.Context, req *platformv1.BuilderHeartbeatRequest) (*emptypb.Empty, error) {
	caller, err := ServiceCallerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	builderID, err := authenticatedBuilderID(caller, req.GetBuilderId())
	if err != nil {
		return nil, err
	}
	if err := s.store.recordBuilderHeartbeat(ctx, builderID, req.GetBuildId()); err != nil {
		if errors.Is(err, deliverycore.ErrBuildNotOwned) {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
		return nil, status.Errorf(codes.Internal, "builder heartbeat: %v", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *BuildOperations) ReportBuildLogs(ctx context.Context, req *platformv1.ReportBuildLogsRequest) (*emptypb.Empty, error) {
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
	build, err := s.store.reads.deliveryQueries().BuildRunByIDQuerier(ctx, s.store.db, req.GetBuildId())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(codes.Internal, "build not found")
		}
		return nil, status.Errorf(codes.Internal, "load build for log report: %v", err)
	}
	if build.State != deliverycore.BuildStateRunning || build.BuilderID != builderID {
		return nil, status.Error(codes.PermissionDenied, "build is not assigned to this builder")
	}
	service, err := s.store.reads.deliveryQueries().ServiceByIDInternalQuerier(ctx, s.store.db, build.ServiceID)
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

func (s *BuildOperations) CompleteBuild(ctx context.Context, req *platformv1.CompleteBuildRequest) (*emptypb.Empty, error) {
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
	build, err := s.store.reads.deliveryQueries().BuildRunByIDQuerier(ctx, s.store.db, req.GetBuildId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load build before completion: %v", err)
	}
	if build.BuilderID != builderID {
		return nil, status.Error(codes.PermissionDenied, "build is not assigned to this builder")
	}
	if deliverycore.BuildStateTerminal(build.State) {
		return &emptypb.Empty{}, nil
	}
	if build.State != deliverycore.BuildStateRunning {
		return nil, status.Error(codes.PermissionDenied, "build is not assigned to this builder")
	}
	if req.GetCommitSha() != build.CommitSHA {
		return nil, status.Error(codes.InvalidArgument, "commit_sha does not match the claimed build")
	}
	service, err := s.store.reads.deliveryQueries().ServiceByIDInternalQuerier(ctx, s.store.db, build.ServiceID)
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
	completion, err := s.delivery.CompleteBuild(ctx, builderID, req.GetBuildId(), req.GetState(), req.GetCommitSha(), req.GetImageDigest(), req.GetFailureReason())
	if err != nil {
		if errors.Is(err, deliverycore.ErrBuildCommitMismatch) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if errors.Is(err, deliverycore.ErrBuildNotOwned) {
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
		// A first source build has no allocation before completion. Completing the
		// build creates its rollout allocations, so use the durable post-completion
		// allocation set when describing the rollout in synthetic logs.
		allocations, err := s.store.reads.deliveryQueries().ListAllocationsByServiceID(ctx, build.ServiceID)
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
	slog.InfoContext(ctx, "build completed", "build_id", req.GetBuildId(), "builder_id", builderID, "state", req.GetState().String(), "commit_sha", req.GetCommitSha(), "image_digest", req.GetImageDigest(), "failure_reason", req.GetFailureReason())

	switch req.GetState() {
	case platformv1.BuildState_BUILD_STATE_SUCCEEDED:
		s.emitter.EmitBuildf(ctx, service, build, StageBuild, "Image build succeeded for commit %s (digest %s)", shortSHA(req.GetCommitSha()), shortDigest(req.GetImageDigest()))
		// We synthesize a deploy-stage line so the "Deploy" tab shows
		// activity immediately even before the agent applies the new
		// rollout; the runtime condition stream later adds more detail.
		target := strings.Join(allocationAgentIDs, ", ")
		if target == "" {
			target = "pending placement"
		}
		if completion.RolloutScheduled {
			s.emitter.EmitDeployf(ctx, service, "", req.GetBuildId(), StageDeploy, "Scheduling rollout to agent %s", target)
		}
	case platformv1.BuildState_BUILD_STATE_FAILED:
		reason := strings.TrimSpace(req.GetFailureReason())
		if reason == "" {
			reason = "no reason provided"
		}
		s.emitter.EmitBuildf(ctx, service, build, StageBuild, "Image build failed: %s", reason)
	case platformv1.BuildState_BUILD_STATE_SUPERSEDED:
		s.emitter.EmitBuild(ctx, service, build, StageBuild, "Build superseded by a newer commit")
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
