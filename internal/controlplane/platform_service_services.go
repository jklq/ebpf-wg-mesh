package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/logs"
	"errors"
	"log/slog"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func (s *PlatformService) CreateService(ctx context.Context, req *platformv1.CreateServiceRequest) (*platformv1.Service, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	identity, err := identity.DelegatedUserFromContext(ctx)
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
	if err := deliverycore.ValidateRollingStrategy(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "rolling strategy: %v", err)
	}
	environment, err := s.environmentForUser(ctx, identity.UserID, req.GetEnvironmentId())
	if err != nil {
		return nil, status.Errorf(codes.PermissionDenied, "environment access: %v", err)
	}
	if err := s.authorizeServiceSource(ctx, environment.ProjectID, spec); err != nil {
		return nil, err
	}
	service, err := s.delivery.CreateScheduledService(ctx, req.GetEnvironmentId(), req.GetService().GetName(), spec)
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, deliverycore.ErrNoPlacementAvailable) || errors.Is(err, deliverycore.ErrVolumeNotFound) || errors.Is(err, deliverycore.ErrVolumeAgentMismatch) {
			return nil, status.Errorf(codes.FailedPrecondition, "create service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "create service: %v", err)
	}
	slog.Info("service created", "service_id", service.ID, "environment_id", service.EnvironmentID, "spec_revision", service.SpecRevision, "rollout_generation", service.RolloutGeneration)
	service, err = s.store.ServiceByID(ctx, identity.UserID, service.ID)
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

func (s *PlatformService) emitInitialization(ctx context.Context, service deliverycore.ServiceRecord) {
	if s.emitter == nil || !s.emitter.Enabled() {
		return
	}
	agentID := service.AllocatedAgentID
	if agentID == "" {
		agentID = "pending placement"
	}
	s.emitter.EmitDeployf(ctx, logs.ServiceScope{EnvironmentID: service.EnvironmentID, ServiceID: service.ID, RolloutGeneration: service.RolloutGeneration, AgentID: service.AllocatedAgentID}, "", "", logs.StageInitialization, "Service scheduled on agent %s (rollout %d)", agentID, service.RolloutGeneration)
}

func (s *PlatformService) UpdateService(ctx context.Context, req *platformv1.UpdateServiceRequest) (*platformv1.Service, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	identity, err := identity.DelegatedUserFromContext(ctx)
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
	if err := deliverycore.ValidateRollingStrategy(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "rolling strategy: %v", err)
	}
	current, err := s.store.ServiceByID(ctx, identity.UserID, req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
	}
	if err := s.requireProjectWriteAccess(ctx, identity.UserID, current.ProjectID); err != nil {
		return nil, err
	}
	if err := s.authorizeServiceSource(ctx, current.ProjectID, spec); err != nil {
		return nil, err
	}
	service, _, err := s.delivery.UpdateService(ctx, req.GetServiceId(), req.GetService().GetName(), spec)
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, deliverycore.ErrConcurrentUpdate) {
			return nil, status.Errorf(codes.Aborted, "update service: %v", err)
		}
		if errors.Is(err, deliverycore.ErrVolumeNotFound) || errors.Is(err, deliverycore.ErrVolumeAgentMismatch) || errors.Is(err, deliverycore.ErrVolumeReplicaUnsupported) {
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

func (s *PlatformService) ApplyDeploymentAction(ctx context.Context, req *platformv1.ApplyDeploymentActionRequest) (*platformv1.ServiceStatus, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" || strings.TrimSpace(req.GetDeploymentId()) == "" ||
		strings.TrimSpace(req.GetIdempotencyKey()) == "" || deliverycore.DeploymentActionName(req.GetAction()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id, deployment_id, action, and idempotency_key are required")
	}
	result, err := s.delivery.ApplyDeploymentAction(ctx, req.GetServiceId(), req.GetDeploymentId(), req.GetAction(), req.GetIdempotencyKey(), req.GetAllocationId())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		switch {
		case errors.Is(err, deliverycore.ErrDeploymentActionDenied):
			return nil, status.Errorf(codes.PermissionDenied, "deployment action: %v", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, status.Errorf(codes.NotFound, "deployment action target: %v", err)
		case errors.Is(err, deliverycore.ErrDeploymentActionConflict), errors.Is(err, deliverycore.ErrConcurrentUpdate):
			return nil, status.Errorf(codes.Aborted, "deployment action: %v", err)
		case errors.Is(err, deliverycore.ErrDeploymentActionInvalid), errors.Is(err, deliverycore.ErrDeploymentStale),
			errors.Is(err, deliverycore.ErrInvalidReplicaCount), errors.Is(err, deliverycore.ErrVolumeReplicaUnsupported),
			errors.Is(err, deliverycore.ErrRolloutInProgress), errors.Is(err, deliverycore.ErrVolumeRollingUnsupported):
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

func (s *PlatformService) ScaleService(ctx context.Context, req *platformv1.ScaleServiceRequest) (*platformv1.ServiceStatus, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	identity, err := identity.DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id is required")
	}
	current, err := s.store.ServiceByID(ctx, identity.UserID, req.GetServiceId())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "load service: %v", err)
	}
	if err := s.requireProjectWriteAccess(ctx, identity.UserID, current.ProjectID); err != nil {
		return nil, err
	}
	service, allocations, index, err := s.delivery.ScaleService(ctx, req.GetServiceId(), req.GetDesiredReplicaCount())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, deliverycore.ErrConcurrentUpdate) {
			return nil, status.Errorf(codes.Aborted, "scale service: %v", err)
		}
		if errors.Is(err, deliverycore.ErrInvalidReplicaCount) || errors.Is(err, deliverycore.ErrVolumeReplicaUnsupported) {
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

func (s *PlatformService) DiscardServiceChanges(ctx context.Context, req *platformv1.DiscardServiceChangesRequest) (*platformv1.Service, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	identity, err := identity.DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	current, err := s.store.ServiceByID(ctx, identity.UserID, req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
	}
	if err := s.requireProjectWriteAccess(ctx, identity.UserID, current.ProjectID); err != nil {
		return nil, err
	}
	service, err := s.delivery.DiscardServiceChanges(ctx, req.GetServiceId(), req.GetChangeIds(), req.GetDiscardAll())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, deliverycore.ErrConcurrentUpdate) {
			return nil, status.Errorf(codes.Aborted, "discard service changes: %v", err)
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

func (s *PlatformService) DeleteService(ctx context.Context, req *platformv1.DeleteServiceRequest) (*emptypb.Empty, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	identity, err := identity.DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id is required")
	}
	service, err := s.store.ServiceByID(ctx, identity.UserID, req.GetServiceId())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
	}
	if err := s.requireProjectWriteAccess(ctx, identity.UserID, service.ProjectID); err != nil {
		return nil, err
	}
	if err := s.delivery.DeleteService(ctx, req.GetServiceId()); err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Errorf(codes.Internal, "delete service: %v", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *PlatformService) GetService(ctx context.Context, req *platformv1.GetServiceRequest) (*platformv1.Service, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	identity, err := identity.DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	service, err := s.store.ServiceByID(ctx, identity.UserID, req.GetServiceId())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
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

func (s *PlatformService) ListServices(ctx context.Context, req *platformv1.ListServicesRequest) (*platformv1.ListServicesResponse, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	identity, err := identity.DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.environmentForUser(ctx, identity.UserID, req.GetEnvironmentId()); err != nil {
		return nil, status.Errorf(codes.PermissionDenied, "environment access: %v", err)
	}
	index, changed, err := s.events.Wait(ctx, req.GetWaitIndex(), platformWaitDuration(req.GetWaitTimeoutSeconds()))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "wait for service event: %v", err)
	}
	if !changed {
		return &platformv1.ListServicesResponse{Index: index, NotModified: true}, nil
	}
	items, err := s.store.ListServices(ctx, identity.UserID, req.GetEnvironmentId())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Errorf(codes.Internal, "list services: %v", err)
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
