package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"errors"
	"log/slog"
	"net/url"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/restartpolicy"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func (s *PlatformService) GetServiceStatus(ctx context.Context, req *platformv1.GetServiceStatusRequest) (*platformv1.ServiceStatus, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	service, allocations, err := s.store.serviceStatus(ctx, identity.UserID, req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service status: %v", err)
	}
	index, changed, err := s.events.Wait(ctx, service.EnvironmentID, req.GetWaitIndex(), platformWaitDuration(req.GetWaitTimeoutSeconds()))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "wait for service event: %v", err)
	}
	if !changed {
		return &platformv1.ServiceStatus{Index: index, NotModified: true}, nil
	}
	// Re-read after the wait: the first read only resolves the environment to
	// watch. Returning it here would report the state from *before* the change
	// that woke us, leaving every watcher one event behind — the final "healthy"
	// status of a rollout would then never reach the client.
	service, allocations, err = s.store.serviceStatus(ctx, identity.UserID, req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service status: %v", err)
	}
	service, err = s.decorateServiceRecordWithAllocations(ctx, service, allocations)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decorate service status: %v", err)
	}
	return toProtoServiceStatus(service, allocations, index), nil
}

func (s *PlatformService) ListServiceLogs(ctx context.Context, req *platformv1.ListServiceLogsRequest) (*platformv1.ListServiceLogsResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id is required")
	}
	if s.logStore == nil {
		return nil, status.Error(codes.FailedPrecondition, errLogStoreDisabled.Error())
	}
	if _, err := s.store.serviceByID(ctx, identity.UserID, req.GetServiceId()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "load service: %v", err)
	}
	lines, err := s.logStore.ListServiceLogs(ctx, req)
	if err != nil {
		if errors.Is(err, errLogStoreDisabled) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Errorf(codes.Internal, "list service logs: %v", err)
	}
	resp := &platformv1.ListServiceLogsResponse{Lines: make([]*platformv1.ServiceLogLine, 0, len(lines))}
	for _, line := range lines {
		resp.Lines = append(resp.Lines, toProtoServiceLogLine(line))
	}
	return resp, nil
}

func (s *PlatformService) ListServiceDeployments(ctx context.Context, req *platformv1.ListServiceDeploymentsRequest) (*platformv1.ListServiceDeploymentsResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id is required")
	}
	items, err := s.store.listServiceDeployments(ctx, identity.UserID, req.GetServiceId(), req.GetLimit())
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

func (s *PlatformService) ListAgents(ctx context.Context, _ *emptypb.Empty) (*platformv1.ListAgentsResponse, error) {
	if _, err := DelegatedUserFromContext(ctx); err != nil {
		return nil, err
	}
	items, err := s.store.listAgents(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list agents: %v", err)
	}
	resp := &platformv1.ListAgentsResponse{Agents: make([]*platformv1.Agent, 0, len(items))}
	for _, item := range items {
		resp.Agents = append(resp.Agents, toProtoAgent(item))
	}
	return resp, nil
}

func (s *PlatformService) decorateServiceRecord(ctx context.Context, service deliverycore.ServiceRecord) (deliverycore.ServiceRecord, error) {
	return s.decorateServiceRecordWithAllocations(ctx, service, nil)
}

func (s *PlatformService) decorateServiceRecordWithAllocations(ctx context.Context, service deliverycore.ServiceRecord, allocs []deliverycore.AllocationRecord) (deliverycore.ServiceRecord, error) {
	if service.SourceSummary == nil {
		service.SourceSummary = deliverycore.BuildSourceSummary(service.Spec)
	}
	// Stages are projected from the allocations + latest build; if we cannot
	// load allocations we still return the stages derived from just the
	// service+build so the UI gets something to render (showing a "waiting"
	// deploy stage rather than a hard error).
	if allocs == nil {
		var err error
		allocs, err = s.store.listAllocationsByServiceID(ctx, service.ID)
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

func (s *PlatformService) notifyServiceAgents(ctx context.Context, serviceID string, identityCatalogChanged bool) {
	if identityCatalogChanged || s.notifier == nil {
		s.notifyAllAgents(ctx)
		return
	}
	ids, err := s.store.listAllocationsByServiceID(ctx, serviceID)
	if err != nil {
		s.notifyAllAgents(ctx)
		return
	}
	seen := map[string]struct{}{}
	for _, alloc := range ids {
		if alloc.AgentID == "" {
			continue
		}
		if _, ok := seen[alloc.AgentID]; ok {
			continue
		}
		seen[alloc.AgentID] = struct{}{}
		s.notifier.Notify(alloc.AgentID)
	}
	if len(seen) == 0 {
		s.notifyAllAgents(ctx)
	}
}

// buildRunRecordFromProto rebuilds the (minimal) in-memory buildRunRecord we
// need for stage projection starting from a proto BuildStatus. We don't
// round-trip every field — the projector only reads state and timestamps, so
// we only reconstruct those. Keeping this narrow avoids accidentally widening
// the implicit contract between decorator and projector.
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
	if queued := status.GetQueuedAt(); queued != nil {
		rec.QueuedAt = queued.AsTime()
	}
	if started := status.GetStartedAt(); started != nil {
		rec.StartedAt = sql.NullTime{Time: started.AsTime(), Valid: true}
	}
	if finished := status.GetFinishedAt(); finished != nil {
		rec.FinishedAt = sql.NullTime{Time: finished.AsTime(), Valid: true}
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
	agents, err := s.store.listAgents(ctx)
	if err != nil {
		slog.Warn("failed to list agents for cluster identity notification", "error", err)
		return
	}
	for _, agent := range agents {
		s.notifier.Notify(agent.ID)
	}
}

func (s *PlatformService) requireProjectWriteAccess(ctx context.Context, userID, projectID string) error {
	if err := s.store.authorizeProjectWrite(ctx, userID, projectID); err != nil {
		return status.Errorf(codes.PermissionDenied, "project write access: %v", err)
	}
	return nil
}
