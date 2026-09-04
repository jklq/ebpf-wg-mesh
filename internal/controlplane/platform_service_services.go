package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func (s *PlatformService) CreateService(ctx context.Context, req *platformv1.CreateServiceRequest) (*platformv1.Service, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetService() == nil {
		return nil, status.Error(codes.InvalidArgument, "service is required")
	}
	spec := canonicalServiceSpec(req.GetService().GetSpec())
	if err := validateServiceSpecResources(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service resources: %v", err)
	}
	if err := validateServiceSpecPorts(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service ports: %v", err)
	}
	if err := validateServiceSpecRestart(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service restart: %v", err)
	}
	if err := validateServicePlacement(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service placement: %v", err)
	}
	if err := validateRollingStrategy(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "rolling strategy: %v", err)
	}
	environment, err := s.environmentForUser(ctx, identity.UserID, req.GetEnvironmentId())
	if err != nil {
		return nil, status.Errorf(codes.PermissionDenied, "environment access: %v", err)
	}
	if err := s.authorizeServiceSource(ctx, environment.ProjectID, spec); err != nil {
		return nil, err
	}
	var service serviceRecord
	service, err = s.store.createScheduledService(ctx, identity.UserID, req.GetEnvironmentId(), req.GetService().GetName(), spec)
	if err != nil {
		if errors.Is(err, errNoPlacementAvailable) || errors.Is(err, errVolumeNotFound) || errors.Is(err, errVolumeAgentMismatch) {
			return nil, status.Errorf(codes.FailedPrecondition, "create service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "create service: %v", err)
	}
	slog.Info("service created", "service_id", service.ID, "environment_id", service.EnvironmentID, "spec_revision", service.SpecRevision, "rollout_generation", service.RolloutGeneration)
	service, err = s.store.serviceByID(ctx, identity.UserID, service.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reload service: %v", err)
	}
	// Emit an initialization-stage line so the right panel immediately shows
	// activity. For image-based services this is the only pre-build step; for
	// source-based services the github/build coordinators will emit follow-up
	// lines as the pipeline progresses.
	s.emitInitialization(ctx, service)
	service, err = s.decorateServiceRecord(ctx, service)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decorate service: %v", err)
	}
	if _, err := s.events.Publish(ctx, service.EnvironmentID); err != nil {
		return nil, status.Errorf(codes.Internal, "publish service event: %v", err)
	}
	return toProtoService(service), nil
}

// emitInitialization writes a single synthetic log line describing the initial
// service scheduling decision. It is a small convenience that keeps the
// platform service methods concise and the phrasing consistent whenever a
// service is first scheduled.
func (s *PlatformService) emitInitialization(ctx context.Context, service serviceRecord) {
	if s.emitter == nil || !s.emitter.Enabled() {
		return
	}
	agentID := service.AllocatedAgentID
	if agentID == "" {
		agentID = "pending placement"
	}
	s.emitter.EmitDeployf(ctx, service, "", "", StageInitialization, "Service scheduled on agent %s (rollout %d)", agentID, service.RolloutGeneration)
}

func (s *PlatformService) UpdateService(ctx context.Context, req *platformv1.UpdateServiceRequest) (*platformv1.Service, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetService() == nil {
		return nil, status.Error(codes.InvalidArgument, "service is required")
	}
	spec := canonicalServiceSpec(req.GetService().GetSpec())
	if err := validateServiceSpecResources(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service resources: %v", err)
	}
	if err := validateServiceSpecPorts(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service ports: %v", err)
	}
	if err := validateServiceSpecRestart(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service restart: %v", err)
	}
	if err := validateServicePlacement(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "service placement: %v", err)
	}
	if err := validateRollingStrategy(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "rolling strategy: %v", err)
	}
	current, err := s.store.serviceByID(ctx, identity.UserID, req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
	}
	if err := s.requireProjectWriteAccess(ctx, identity.UserID, current.ProjectID); err != nil {
		return nil, err
	}
	if err := s.authorizeServiceSource(ctx, current.ProjectID, spec); err != nil {
		return nil, err
	}
	service, _, err := s.store.updateService(ctx, identity.UserID, req.GetServiceId(), req.GetService().GetName(), spec)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, errConcurrentUpdate) {
			return nil, status.Errorf(codes.Aborted, "update service: %v", err)
		}
		if errors.Is(err, errVolumeNotFound) || errors.Is(err, errVolumeAgentMismatch) || errors.Is(err, errVolumeReplicaUnsupported) {
			return nil, status.Errorf(codes.FailedPrecondition, "update service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "update service: %v", err)
	}
	service, err = s.decorateServiceRecord(ctx, service)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decorate service: %v", err)
	}
	if _, err := s.events.Publish(ctx, service.EnvironmentID); err != nil {
		return nil, status.Errorf(codes.Internal, "publish service event: %v", err)
	}
	return toProtoService(service), nil
}

func (s *PlatformService) ApplyDeploymentAction(ctx context.Context, req *platformv1.ApplyDeploymentActionRequest) (*platformv1.ServiceStatus, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" || strings.TrimSpace(req.GetDeploymentId()) == "" ||
		strings.TrimSpace(req.GetIdempotencyKey()) == "" || deploymentActionName(req.GetAction()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id, deployment_id, action, and idempotency_key are required")
	}
	currentService, err := s.store.serviceByID(ctx, identity.UserID, req.GetServiceId())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "load service: %v", err)
	}
	if err := s.requireProjectWriteAccess(ctx, identity.UserID, currentService.ProjectID); err != nil {
		return nil, err
	}
	_, _, err = s.store.applyDeploymentAction(ctx, identity.UserID, req.GetServiceId(), req.GetDeploymentId(), req.GetAction(), req.GetIdempotencyKey(), req.GetAllocationId())
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, status.Errorf(codes.NotFound, "deployment action target: %v", err)
		case errors.Is(err, errDeploymentActionConflict), errors.Is(err, errConcurrentUpdate):
			return nil, status.Errorf(codes.Aborted, "deployment action: %v", err)
		case errors.Is(err, errDeploymentActionInvalid), errors.Is(err, errDeploymentStale),
			errors.Is(err, errInvalidReplicaCount), errors.Is(err, errVolumeReplicaUnsupported),
			errors.Is(err, errRolloutInProgress), errors.Is(err, errVolumeRollingUnsupported):
			return nil, status.Errorf(codes.FailedPrecondition, "deployment action: %v", err)
		default:
			return nil, status.Errorf(codes.Internal, "deployment action: %v", err)
		}
	}
	s.notifyAllAgents(ctx)
	s.ingress.RequestSync()
	currentService, allocations, err := s.store.serviceStatus(ctx, identity.UserID, req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "deployment action status: %v", err)
	}
	currentService, err = s.decorateServiceRecordWithAllocations(ctx, currentService, allocations)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decorate deployment action status: %v", err)
	}
	index, err := s.events.Publish(ctx, currentService.EnvironmentID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "publish service event: %v", err)
	}
	return toProtoServiceStatus(currentService, allocations, index), nil
}

func (s *PlatformService) ScaleService(ctx context.Context, req *platformv1.ScaleServiceRequest) (*platformv1.ServiceStatus, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id is required")
	}
	current, err := s.store.serviceByID(ctx, identity.UserID, req.GetServiceId())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "load service: %v", err)
	}
	if err := s.requireProjectWriteAccess(ctx, identity.UserID, current.ProjectID); err != nil {
		return nil, err
	}
	service, allocations, err := s.store.scaleService(ctx, identity.UserID, req.GetServiceId(), req.GetDesiredReplicaCount())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, errConcurrentUpdate) {
			return nil, status.Errorf(codes.Aborted, "scale service: %v", err)
		}
		if errors.Is(err, errInvalidReplicaCount) || errors.Is(err, errVolumeReplicaUnsupported) {
			return nil, status.Errorf(codes.FailedPrecondition, "scale service: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "scale service: %v", err)
	}
	service, err = s.decorateServiceRecordWithAllocations(ctx, service, allocations)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decorate scaled service: %v", err)
	}
	index, err := s.events.Publish(ctx, service.EnvironmentID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "publish service event: %v", err)
	}
	return toProtoServiceStatus(service, allocations, index), nil
}

func (s *PlatformService) DiscardServiceChanges(ctx context.Context, req *platformv1.DiscardServiceChangesRequest) (*platformv1.Service, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	current, err := s.store.serviceByID(ctx, identity.UserID, req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
	}
	if err := s.requireProjectWriteAccess(ctx, identity.UserID, current.ProjectID); err != nil {
		return nil, err
	}
	service, err := s.store.discardServiceChanges(ctx, identity.UserID, req.GetServiceId(), req.GetChangeIds(), req.GetDiscardAll())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, errConcurrentUpdate) {
			return nil, status.Errorf(codes.Aborted, "discard service changes: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "discard service changes: %v", err)
	}
	service, err = s.decorateServiceRecord(ctx, service)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decorate service: %v", err)
	}
	if _, err := s.events.Publish(ctx, service.EnvironmentID); err != nil {
		return nil, status.Errorf(codes.Internal, "publish service event: %v", err)
	}
	return toProtoService(service), nil
}

func (s *PlatformService) DeleteService(ctx context.Context, req *platformv1.DeleteServiceRequest) (*emptypb.Empty, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	service, err := s.store.serviceByID(ctx, identity.UserID, req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
	}
	if err := s.requireProjectWriteAccess(ctx, identity.UserID, service.ProjectID); err != nil {
		return nil, err
	}
	bindings, err := s.store.listDomainBindings(ctx, identity.UserID, req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list domain bindings: %v", err)
	}
	if err := s.store.deleteService(ctx, identity.UserID, req.GetServiceId()); err != nil {
		return nil, status.Errorf(codes.Internal, "delete service: %v", err)
	}
	s.notifyAllAgents(ctx)
	if len(bindings) > 0 {
		s.ingress.RequestSync()
	}
	if _, err := s.events.Publish(ctx, service.EnvironmentID); err != nil {
		return nil, status.Errorf(codes.Internal, "publish service event: %v", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *PlatformService) GetService(ctx context.Context, req *platformv1.GetServiceRequest) (*platformv1.Service, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	service, err := s.store.serviceByID(ctx, identity.UserID, req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
	}
	service, err = s.decorateServiceRecord(ctx, service)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decorate service: %v", err)
	}
	return toProtoService(service), nil
}

func (s *PlatformService) ListServices(ctx context.Context, req *platformv1.ListServicesRequest) (*platformv1.ListServicesResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	environment, err := s.environmentForUser(ctx, identity.UserID, req.GetEnvironmentId())
	if err != nil {
		return nil, status.Errorf(codes.PermissionDenied, "environment access: %v", err)
	}
	index, changed, err := s.events.Wait(ctx, environment.ID, req.GetWaitIndex(), platformWaitDuration(req.GetWaitTimeoutSeconds()))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "wait for service event: %v", err)
	}
	if !changed {
		return &platformv1.ListServicesResponse{Index: index, NotModified: true}, nil
	}
	items, err := s.store.listServices(ctx, identity.UserID, req.GetEnvironmentId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list services: %v", err)
	}
	resp := &platformv1.ListServicesResponse{Services: make([]*platformv1.Service, 0, len(items)), Index: index}
	for _, item := range items {
		item, err = s.decorateServiceRecord(ctx, item)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "decorate service: %v", err)
		}
		resp.Services = append(resp.Services, toProtoService(item))
	}
	return resp, nil
}
