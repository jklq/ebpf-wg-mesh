package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/url"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/restartpolicy"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func (s *platformService) CreateService(ctx context.Context, req *platformv1.CreateServiceRequest) (*platformv1.Service, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetService() == nil {
		return nil, status.Error(codes.InvalidArgument, "service is required")
	}
	spec := deliverycore.CanonicalServiceSpec(req.GetService().GetSpec())
	if err := validateServiceSpecResources(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service resources: %v", err)
	}
	if err := validateServiceSpecPorts(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service ports: %v", err)
	}
	if err := validateServiceSpecRestart(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service restart: %v", err)
	}
	if err := deliverycore.ValidateServicePlacement(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service placement: %v", err)
	}
	if err := deliverycore.ValidateBuildRecipe(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service build recipe: %v", err)
	}
	if err := deliverycore.ValidateRollingStrategy(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "rolling strategy: %v", err)
	}
	environment, err := s.environmentForUser(ctx, user, req.GetEnvironmentId())
	if err != nil {
		return nil, readAccessError("environment access", err)
	}
	if err := s.authorizeServiceSource(ctx, environment.ProjectID, spec); err != nil {
		return nil, err
	}
	service, err := s.delivery.CreateScheduledService(ctx, user, req.GetEnvironmentId(), req.GetService().GetName(), spec)
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, deliverycore.ErrServiceAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "create service: %v", err)
		}
		if errors.Is(err, deliverycore.ErrNoPlacementAvailable) || errors.Is(err, deliverycore.ErrVolumeNotFound) || errors.Is(err, deliverycore.ErrVolumeAgentMismatch) || errors.Is(err, deliverycore.ErrEnvironmentDeleted) {
			return nil, status.Errorf(codes.FailedPrecondition, "create service: %v", err)
		}
		return nil, writeAccessError("create service", err)
	}
	slog.Info("service created", "service_id", service.ID, "environment_id", service.EnvironmentID, "spec_revision", service.SpecRevision, "rollout_generation", service.RolloutGeneration)
	service, err = s.store.ServiceByID(ctx, user, service.ID)
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Errorf(codes.Internal, "reload service: %v", err)
	}
	s.emitInitialization(ctx, service)
	service, err = s.decorateServiceRecord(ctx, service)
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Errorf(codes.Internal, "decorate service: %v", err)
	}
	return toProtoService(service), nil
}

func (s *platformService) emitInitialization(ctx context.Context, service deliverycore.ServiceRecord) {
	if s.emitter == nil || !s.emitter.Enabled() {
		return
	}
	agentID := service.AllocatedAgentID
	if agentID == "" {
		agentID = "pending placement"
	}
	s.emitter.EmitDeployf(ctx, logs.ServiceScope{EnvironmentID: service.EnvironmentID, ServiceID: service.ID, RolloutGeneration: service.RolloutGeneration, AgentID: service.AllocatedAgentID}, "", "", logs.StageInitialization, "Service scheduled on agent %s (rollout %d)", agentID, service.RolloutGeneration)
}

func (s *platformService) UpdateService(ctx context.Context, req *platformv1.UpdateServiceRequest) (*platformv1.Service, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetService() == nil {
		return nil, status.Error(codes.InvalidArgument, "service is required")
	}
	spec := deliverycore.CanonicalServiceSpec(req.GetService().GetSpec())
	if err := validateServiceSpecResources(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service resources: %v", err)
	}
	if err := validateServiceSpecPorts(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service ports: %v", err)
	}
	if err := validateServiceSpecRestart(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service restart: %v", err)
	}
	if err := deliverycore.ValidateServicePlacement(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service placement: %v", err)
	}
	if err := deliverycore.ValidateBuildRecipe(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service build recipe: %v", err)
	}
	if err := deliverycore.ValidateRollingStrategy(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "rolling strategy: %v", err)
	}
	current, err := s.store.ServiceByID(ctx, user, req.GetServiceId())
	if err != nil {
		return nil, readAccessError("service", err)
	}
	if err := s.authorizeServiceSource(ctx, current.ProjectID, spec); err != nil {
		return nil, err
	}
	service, _, err := s.delivery.UpdateService(ctx, user, req.GetServiceId(), req.GetService().GetName(), spec)
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, authz.ErrDenied) {
			return nil, status.Errorf(codes.PermissionDenied, "project write access: %v", err)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, deliverycore.ErrConcurrentUpdate) {
			return nil, status.Errorf(codes.Aborted, "update service: %v", err)
		}
		if errors.Is(err, deliverycore.ErrVolumeNotFound) || errors.Is(err, deliverycore.ErrVolumeAgentMismatch) || errors.Is(err, deliverycore.ErrVolumeReplicaUnsupported) || errors.Is(err, deliverycore.ErrServiceDeleted) {
			return nil, status.Errorf(codes.FailedPrecondition, "update service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "update service: %v", err)
	}
	service, err = s.decorateServiceRecord(ctx, service)
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Errorf(codes.Internal, "decorate service: %v", err)
	}
	return toProtoService(service), nil
}

func (s *platformService) ApplyDeploymentAction(ctx context.Context, req *platformv1.ApplyDeploymentActionRequest) (*platformv1.ServiceStatus, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" || strings.TrimSpace(req.GetDeploymentId()) == "" ||
		strings.TrimSpace(req.GetIdempotencyKey()) == "" || deliverycore.DeploymentActionName(req.GetAction()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id, deployment_id, action, and idempotency_key are required")
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	result, err := s.delivery.ApplyDeploymentAction(ctx, user, req.GetServiceId(), req.GetDeploymentId(), req.GetAction(), req.GetIdempotencyKey(), req.GetAllocationId())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		switch {
		case errors.Is(err, authz.ErrDenied):
			return nil, status.Errorf(codes.PermissionDenied, "deployment action: %v", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, status.Errorf(codes.NotFound, "deployment action target: %v", err)
		case errors.Is(err, deliverycore.ErrDeploymentActionConflict), errors.Is(err, deliverycore.ErrConcurrentUpdate):
			return nil, status.Errorf(codes.Aborted, "deployment action: %v", err)
		case errors.Is(err, deliverycore.ErrDeploymentActionInvalid), errors.Is(err, deliverycore.ErrDeploymentStale),
			errors.Is(err, deliverycore.ErrInvalidReplicaCount), errors.Is(err, deliverycore.ErrVolumeReplicaUnsupported),
			errors.Is(err, deliverycore.ErrRolloutInProgress), errors.Is(err, deliverycore.ErrVolumeRollingUnsupported),
			errors.Is(err, deliverycore.ErrSealedNameConflict),
			errors.Is(err, deliverycore.ErrServiceDeleted):
			return nil, status.Errorf(codes.FailedPrecondition, "deployment action: %v", err)
		default:
			return nil, status.Errorf(codes.Internal, "deployment action: %v", err)
		}
	}
	result.Service, err = s.decorateServiceRecordWithAllocations(ctx, result.Service, result.Allocations)
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Errorf(codes.Internal, "decorate deployment action status: %v", err)
	}
	return s.protoServiceStatus(result.Service, result.Allocations, result.EventIndex), nil
}

func (s *platformService) ScaleService(ctx context.Context, req *platformv1.ScaleServiceRequest) (*platformv1.ServiceStatus, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id is required")
	}
	service, allocations, index, err := s.delivery.ScaleService(ctx, user, req.GetServiceId(), req.GetDesiredReplicaCount())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, authz.ErrDenied) {
			return nil, status.Errorf(codes.PermissionDenied, "project write access: %v", err)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, deliverycore.ErrConcurrentUpdate) {
			return nil, status.Errorf(codes.Aborted, "scale service: %v", err)
		}
		if errors.Is(err, deliverycore.ErrInvalidReplicaCount) || errors.Is(err, deliverycore.ErrVolumeReplicaUnsupported) || errors.Is(err, deliverycore.ErrServiceDeleted) {
			return nil, status.Errorf(codes.FailedPrecondition, "scale service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "scale service: %v", err)
	}
	service, err = s.decorateServiceRecordWithAllocations(ctx, service, allocations)
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Errorf(codes.Internal, "decorate scaled service: %v", err)
	}
	return s.protoServiceStatus(service, allocations, index), nil
}

func (s *platformService) DiscardServiceChanges(ctx context.Context, req *platformv1.DiscardServiceChangesRequest) (*platformv1.Service, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	service, err := s.delivery.DiscardServiceChanges(ctx, user, req.GetServiceId(), req.GetChangeIds(), req.GetDiscardAll())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, authz.ErrDenied) {
			return nil, status.Errorf(codes.PermissionDenied, "project write access: %v", err)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, deliverycore.ErrConcurrentUpdate) {
			return nil, status.Errorf(codes.Aborted, "discard service changes: %v", err)
		}
		if errors.Is(err, deliverycore.ErrServiceDeleted) {
			return nil, status.Errorf(codes.FailedPrecondition, "discard service changes: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "discard service changes: %v", err)
	}
	service, err = s.decorateServiceRecord(ctx, service)
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Errorf(codes.Internal, "decorate service: %v", err)
	}
	return toProtoService(service), nil
}

func (s *platformService) DeleteService(ctx context.Context, req *platformv1.DeleteServiceRequest) (*emptypb.Empty, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id is required")
	}
	if err := s.delivery.DeleteService(ctx, user, req.GetServiceId()); err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, writeAccessError("delete service", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *platformService) RestoreService(ctx context.Context, req *platformv1.RestoreServiceRequest) (*platformv1.Service, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id is required")
	}
	service, err := s.delivery.RestoreService(ctx, user, req.GetServiceId())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, deliverycore.ErrAncestorDeleted) || errors.Is(err, deliverycore.ErrDeletionExpired) {
			return nil, status.Errorf(codes.FailedPrecondition, "restore service: %v", err)
		}
		return nil, writeAccessError("restore service", err)
	}
	service, err = s.decorateServiceRecord(ctx, service)
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Errorf(codes.Internal, "decorate service: %v", err)
	}
	return toProtoService(service), nil
}

func (s *platformService) GetService(ctx context.Context, req *platformv1.GetServiceRequest) (*platformv1.Service, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	service, err := s.store.ServiceByID(ctx, user, req.GetServiceId())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, readAccessError("service", err)
	}
	service, err = s.decorateServiceRecord(ctx, service)
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Errorf(codes.Internal, "decorate service: %v", err)
	}
	return toProtoService(service), nil
}

func (s *platformService) ListServices(ctx context.Context, req *platformv1.ListServicesRequest) (*platformv1.ListServicesResponse, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	// Authorize before waiting so denied callers never hold waiter slots.
	if _, err := s.store.EnvironmentByID(ctx, user, req.GetEnvironmentId()); err != nil {
		return nil, writeAccessError("environment access", err)
	}
	index, changed, err := s.events.Wait(ctx, req.GetWaitIndex(), platformWaitDuration(req.GetWaitTimeoutSeconds()))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "wait for service event: %v", err)
	}
	if !changed {
		return &platformv1.ListServicesResponse{Index: index, NotModified: true}, nil
	}
	items, err := s.store.ListServices(ctx, user, req.GetEnvironmentId(), req.GetIncludeDeleted())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, writeAccessError("list services", err)
	}
	allocations, err := s.delivery.LiveAllocationsByEnvironment(req.GetEnvironmentId())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Errorf(codes.Internal, "list live service allocations: %v", err)
	}
	resp := &platformv1.ListServicesResponse{Services: make([]*platformv1.Service, 0, len(items)), Index: index}
	for _, item := range items {
		serviceAllocations, ok := allocations[item.ID]
		if !ok {
			serviceAllocations = []deliverycore.AllocationRecord{}
		}
		item, err = s.decorateServiceRecordWithAllocations(ctx, item, serviceAllocations)
		if err != nil {
			if mapped := s.liveOwnerError(ctx, err); mapped != nil {
				return nil, mapped
			}
			return nil, status.Errorf(codes.Internal, "decorate service: %v", err)
		}
		resp.Services = append(resp.Services, toProtoService(item))
	}
	return resp, nil
}

func (s *platformService) GetServiceStatus(ctx context.Context, req *platformv1.GetServiceStatusRequest) (*platformv1.ServiceStatus, error) {
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

func (s *platformService) serviceStatusError(ctx context.Context, err error) error {
	if mapped := s.liveOwnerError(ctx, err); mapped != nil {
		return mapped
	}
	return readAccessError("service status", err)
}

func (s *platformService) liveOwnerError(ctx context.Context, err error) error {
	if !errors.Is(err, deliverycore.ErrNotLiveOwner) && !errors.Is(err, deliverycore.ErrLeaseLost) {
		return nil
	}
	if ownerErr := s.requireLiveOwner(ctx); ownerErr != nil {
		return ownerErr
	}
	return status.Error(codes.Unavailable, "live owner has not started serving")
}

func (s *platformService) ListServiceLogs(ctx context.Context, req *platformv1.ListServiceLogsRequest) (*platformv1.ListServiceLogsResponse, error) {
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

func (s *platformService) ListServiceDeployments(ctx context.Context, req *platformv1.ListServiceDeploymentsRequest) (*platformv1.ListServiceDeploymentsResponse, error) {
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

func (s *platformService) ListBuildAttempts(ctx context.Context, req *platformv1.ListBuildAttemptsRequest) (*platformv1.ListBuildAttemptsResponse, error) {
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

func (s *platformService) ListServiceArtifacts(ctx context.Context, req *platformv1.ListServiceArtifactsRequest) (*platformv1.ListServiceArtifactsResponse, error) {
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
func (s *platformService) ListAgents(ctx context.Context, _ *emptypb.Empty) (*platformv1.ListAgentsResponse, error) {
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

func (s *platformService) decorateServiceRecord(ctx context.Context, service deliverycore.ServiceRecord) (deliverycore.ServiceRecord, error) {
	return s.decorateServiceRecordWithAllocations(ctx, service, nil)
}

func (s *platformService) decorateServiceRecordWithAllocations(ctx context.Context, service deliverycore.ServiceRecord, allocs []deliverycore.AllocationRecord) (deliverycore.ServiceRecord, error) {
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

func (s *platformService) notifyAllAgents(ctx context.Context) {
	ids, err := s.store.AgentIDs(ctx)
	if err != nil {
		slog.Warn("failed to list agents for cluster identity notification", "error", err)
		return
	}
	for _, id := range ids {
		s.notifier.Notify(id)
	}
}

const (
	defaultServiceCPUMillis       int64 = 250
	defaultServiceMemoryMebibytes int64 = 256
)

func runtimePortsFromInts(ports []int32) []*platformv1.ServiceRuntimePort {
	out := make([]*platformv1.ServiceRuntimePort, 0, len(ports))
	seen := make(map[int32]struct{}, len(ports))
	for _, port := range ports {
		if deliverycore.ValidatePort(port) != nil {
			continue
		}
		if _, ok := seen[port]; ok {
			continue
		}
		seen[port] = struct{}{}
		out = append(out, &platformv1.ServiceRuntimePort{
			Port:    port,
			Primary: len(out) == 0,
		})
	}
	return out
}

func directImageServiceSpec(image string, runtime *platformv1.ServiceRuntime) *platformv1.ServiceSpec {
	if runtime == nil {
		runtime = defaultServiceRuntime()
	}
	return &platformv1.ServiceSpec{
		Runtime: runtime,
		Source: &platformv1.ServiceSource{
			Source: &platformv1.ServiceSource_Image{
				Image: &platformv1.DirectImageSource{Image: image},
			},
		},
	}
}

func repositoryServiceSpec(runtime *platformv1.ServiceRuntime, source *platformv1.ServiceSourceSpec) *platformv1.ServiceSpec {
	if runtime == nil {
		runtime = defaultServiceRuntime()
	}
	if source == nil {
		source = &platformv1.ServiceSourceSpec{}
	}
	return &platformv1.ServiceSpec{
		Runtime: runtime,
		Source: &platformv1.ServiceSource{
			Source: &platformv1.ServiceSource_SourceSpec{
				SourceSpec: source,
			},
		},
	}
}

func defaultServiceRuntime() *platformv1.ServiceRuntime {
	return &platformv1.ServiceRuntime{
		CpuMillis:       defaultServiceCPUMillis,
		MemoryMebibytes: defaultServiceMemoryMebibytes,
	}
}

func validateServiceSpecResources(spec *platformv1.ServiceSpec) error {
	if spec == nil || spec.GetRuntime() == nil {
		return errors.New("runtime resources are required")
	}
	runtime := spec.GetRuntime()
	if runtime.GetCpuMillis() < defaultServiceCPUMillis {
		return errors.New("cpu_millis must be at least 250")
	}
	if runtime.GetMemoryMebibytes() < defaultServiceMemoryMebibytes {
		return errors.New("memory_mebibytes must be at least 256")
	}
	return nil
}
