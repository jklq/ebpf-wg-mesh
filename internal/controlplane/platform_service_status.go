package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/url"
	"strings"

	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/restartpolicy"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func (s *PlatformService) GetServiceStatus(ctx context.Context, req *platformv1.GetServiceStatusRequest) (*platformv1.ServiceStatus, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	service, allocations, err := s.store.ServiceStatus(ctx, user, req.GetServiceId())
	if err != nil {
		return nil, s.serviceStatusError(ctx, err)
	}
	index, changed, err := s.events.Wait(ctx, req.GetWaitIndex(), platformWaitDuration(req.GetWaitTimeoutSeconds()))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "wait for service event: %v", err)
	}
	if !changed {
		return &platformv1.ServiceStatus{Index: index, NotModified: true}, nil
	}
	service, allocations, err = s.store.ServiceStatus(ctx, user, req.GetServiceId())
	if err != nil {
		return nil, s.serviceStatusError(ctx, err)
	}
	service, err = s.decorateServiceRecordWithAllocations(ctx, service, allocations)
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Errorf(codes.Internal, "decorate service status: %v", err)
	}
	return s.protoServiceStatus(service, allocations, index), nil
}

func (s *PlatformService) serviceStatusError(ctx context.Context, err error) error {
	if mapped := s.liveOwnerError(ctx, err); mapped != nil {
		return mapped
	}
	return readAccessError("service status", err)
}

func (s *PlatformService) liveOwnerError(ctx context.Context, err error) error {
	if !errors.Is(err, deliverycore.ErrNotLiveOwner) && !errors.Is(err, deliverycore.ErrLeaseLost) {
		return nil
	}
	if ownerErr := s.requireLiveOwner(ctx); ownerErr != nil {
		return ownerErr
	}
	return status.Error(codes.Unavailable, "live owner has not started serving")
}

func (s *PlatformService) ListServiceLogs(ctx context.Context, req *platformv1.ListServiceLogsRequest) (*platformv1.ListServiceLogsResponse, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id is required")
	}
	if s.logStore == nil {
		return nil, status.Error(codes.FailedPrecondition, logs.ErrDisabled.Error())
	}
	if _, err := s.store.ServiceByID(ctx, user, req.GetServiceId()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "load service: %v", err)
	}
	page, err := s.logStore.ListServiceLogs(ctx, req)
	if err != nil {
		if errors.Is(err, logs.ErrDisabled) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Errorf(codes.Internal, "list service logs: %v", err)
	}
	resp := &platformv1.ListServiceLogsResponse{
		Lines:            make([]*platformv1.ServiceLogLine, 0, len(page.Lines)),
		NextPageToken:    page.NextPageToken,
		NextGapPageToken: page.NextGapPageToken,
		Gaps:             make([]*platformv1.ServiceLogGap, 0, len(page.Gaps)),
	}
	for _, line := range page.Lines {
		resp.Lines = append(resp.Lines, toProtoServiceLogLine(line))
	}
	for _, gap := range page.Gaps {
		resp.Gaps = append(resp.Gaps, toProtoServiceLogGap(gap))
	}
	return resp, nil
}

func (s *PlatformService) ListServiceDeployments(ctx context.Context, req *platformv1.ListServiceDeploymentsRequest) (*platformv1.ListServiceDeploymentsResponse, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id is required")
	}
	items, err := s.store.ListServiceDeployments(ctx, user, req.GetServiceId(), req.GetLimit())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service deployments: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "list service deployments: %v", err)
	}
	resp := &platformv1.ListServiceDeploymentsResponse{
		Deployments: make([]*platformv1.DeploymentRecord, 0, len(items)),
	}
	for _, item := range items {
		resp.Deployments = append(resp.Deployments, toProtoDeploymentRecord(item))
	}
	return resp, nil
}

func (s *PlatformService) ListBuildAttempts(ctx context.Context, req *platformv1.ListBuildAttemptsRequest) (*platformv1.ListBuildAttemptsResponse, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" || strings.TrimSpace(req.GetBuildId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id and build_id are required")
	}
	attempts, err := s.delivery.BuildAttempts(ctx, user, req.GetServiceId(), req.GetBuildId())
	if err != nil {
		return nil, readAccessError("list build attempts", err)
	}
	resp := &platformv1.ListBuildAttemptsResponse{Attempts: make([]*platformv1.BuildAttempt, 0, len(attempts))}
	for _, attempt := range attempts {
		resp.Attempts = append(resp.Attempts, deliverycore.ToProtoBuildAttempt(attempt))
	}
	return resp, nil
}

func (s *PlatformService) ListServiceArtifacts(ctx context.Context, req *platformv1.ListServiceArtifactsRequest) (*platformv1.ListServiceArtifactsResponse, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id is required")
	}
	artifacts, err := s.delivery.ListServiceArtifacts(ctx, user, req.GetServiceId(), req.GetLimit())
	if err != nil {
		return nil, readAccessError("list service artifacts", err)
	}
	resp := &platformv1.ListServiceArtifactsResponse{Artifacts: make([]*platformv1.BuildArtifact, 0, len(artifacts))}
	for i := range artifacts {
		resp.Artifacts = append(resp.Artifacts, deliverycore.ToProtoBuildArtifact(&artifacts[i]))
	}
	return resp, nil
}

// ListAgents is operator-only: non-operator callers get PermissionDenied. This
// is a deliberate contract (fleet membership is operator surface); operator
// gating also applies to OpsService fleet RPCs.
func (s *PlatformService) ListAgents(ctx context.Context, _ *emptypb.Empty) (*platformv1.ListAgentsResponse, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.store.ListAgents(ctx, user)
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, authz.ErrDenied) {
			return nil, status.Errorf(codes.PermissionDenied, "list agents: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "list agents: %v", err)
	}
	resp := &platformv1.ListAgentsResponse{Agents: make([]*platformv1.Agent, 0, len(items))}
	for _, item := range items {
		resp.Agents = append(resp.Agents, toProtoAgent(item))
	}
	resp.Live = liveReadMeta(s.delivery)
	return resp, nil
}

func (s *PlatformService) decorateServiceRecord(ctx context.Context, service deliverycore.ServiceRecord) (deliverycore.ServiceRecord, error) {
	return s.decorateServiceRecordWithAllocations(ctx, service, nil)
}

func (s *PlatformService) decorateServiceRecordWithAllocations(ctx context.Context, service deliverycore.ServiceRecord, allocs []deliverycore.AllocationRecord) (deliverycore.ServiceRecord, error) {
	if service.SourceSummary == nil {
		service.SourceSummary = deliverycore.BuildSourceSummary(service.Spec)
	}
	if allocs == nil {
		var err error
		allocs, err = s.store.ListAllocationsByServiceID(ctx, service.ID)
		if err != nil {
			return deliverycore.ServiceRecord{}, err
		}
	}
	var buildRec *deliverycore.BuildRunRecord
	if service.LatestBuild != nil {
		rec := buildRunRecordFromProto(service.LatestBuild)
		buildRec = &rec
	}
	service.ReadyReplicaCount = countReadyAllocations(allocs)
	stages := deploymentStages(service, buildRec)
	if service.LatestBuild == nil && len(stages) > 0 {
		service.LatestBuild = &platformv1.BuildStatus{Stages: stages}
	} else if service.LatestBuild != nil {
		service.LatestBuild.Stages = stages
	}
	return service, nil
}

func buildRunRecordFromProto(status *platformv1.BuildStatus) deliverycore.BuildRunRecord {
	rec := deliverycore.BuildRunRecord{
		ID:            status.GetBuildId(),
		CommitSHA:     status.GetCommitSha(),
		CommitMessage: status.GetCommitMessage(),
		CommitAuthor:  status.GetCommitAuthor(),
		ImageDigest:   status.GetImageDigest(),
		FailureReason: status.GetFailureReason(),
	}
	switch status.GetState() {
	case platformv1.BuildState_BUILD_STATE_QUEUED:
		rec.State = deliverycore.BuildStateQueued
	case platformv1.BuildState_BUILD_STATE_RUNNING:
		rec.State = deliverycore.BuildStateRunning
	case platformv1.BuildState_BUILD_STATE_SUCCEEDED:
		rec.State = deliverycore.BuildStateSucceeded
	case platformv1.BuildState_BUILD_STATE_FAILED:
		rec.State = deliverycore.BuildStateFailed
	case platformv1.BuildState_BUILD_STATE_SUPERSEDED:
		rec.State = deliverycore.BuildStateSuperseded
	case platformv1.BuildState_BUILD_STATE_CANCELLED:
		rec.State = deliverycore.BuildStateCancelled
	}
	if queued := status.GetQueuedAt(); queued != nil && queued.IsValid() {
		rec.QueuedAt = queued.AsTime().UTC()
	}
	if started := status.GetStartedAt(); started != nil && started.IsValid() {
		rec.StartedAt = sql.NullTime{Time: started.AsTime().UTC(), Valid: true}
	}
	if finished := status.GetFinishedAt(); finished != nil && finished.IsValid() {
		rec.FinishedAt = sql.NullTime{Time: finished.AsTime().UTC(), Valid: true}
	}
	return rec
}

func validateServiceSpecRestart(spec *platformv1.ServiceSpec) error {
	runtime := spec.GetRuntime()
	if err := restartpolicy.ValidateRestart(runtime.GetRestart()); err != nil {
		return err
	}
	if check := runtime.GetLivenessCheck(); check != nil {
		if check.GetPort() > 0 {
			if err := deliverycore.ValidatePort(check.GetPort()); err != nil {
				return err
			}
		}
		if check.GetTimeoutSeconds() < 0 {
			return errors.New("liveness check timeout must be non-negative")
		}
		switch check.GetType() {
		case platformv1.HealthCheck_TYPE_UNSPECIFIED:
			if check.GetPath() != "" || check.GetPort() != 0 || check.GetTimeoutSeconds() != 0 {
				return errors.New("only explicit HTTP liveness checks are supported")
			}
		case platformv1.HealthCheck_TYPE_HTTP:
			if !validHealthCheckPath(check.GetPath()) {
				return errors.New("HTTP liveness check path must be an absolute request path beginning with one slash")
			}
		default:
			return errors.New("unsupported liveness check type")
		}
	}
	return nil
}

func validateServiceSpecPorts(spec *platformv1.ServiceSpec) error {
	runtime := spec.GetRuntime()
	for _, port := range runtime.GetPorts() {
		if err := deliverycore.ValidatePort(port.GetPort()); err != nil {
			return err
		}
	}
	check := runtime.GetHealthCheck()
	if check == nil {
		return nil
	}
	if check.GetPort() > 0 {
		if err := deliverycore.ValidatePort(check.GetPort()); err != nil {
			return err
		}
	}
	if check.GetPort() == 0 && len(runtime.GetPorts()) == 0 && check.GetType() != platformv1.HealthCheck_TYPE_UNSPECIFIED {
		return errors.New("health check requires a port or at least one runtime port")
	}
	if check.GetTimeoutSeconds() < 0 {
		return errors.New("health check timeout must be non-negative")
	}
	switch check.GetType() {
	case platformv1.HealthCheck_TYPE_UNSPECIFIED:
		if check.GetPath() != "" || check.GetPort() != 0 || check.GetTimeoutSeconds() != 0 {
			return errors.New("only explicit HTTP health checks are supported")
		}
	case platformv1.HealthCheck_TYPE_HTTP:
		if !validHealthCheckPath(check.GetPath()) {
			return errors.New("HTTP health check path must be an absolute request path beginning with one slash")
		}
	default:
		return errors.New("unsupported health check type")
	}
	return nil
}

func validHealthCheckPath(path string) bool {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\r\n") {
		return false
	}
	parsed, err := url.ParseRequestURI(path)
	return err == nil && !parsed.IsAbs() && parsed.Host == ""
}

func (s *PlatformService) notifyAllAgents(ctx context.Context) {
	ids, err := s.store.AgentIDs(ctx)
	if err != nil {
		slog.Warn("failed to list agents for cluster identity notification", "error", err)
		return
	}
	for _, id := range ids {
		s.notifier.Notify(id)
	}
}
