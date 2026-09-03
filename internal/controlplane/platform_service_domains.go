package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func (s *PlatformService) CreateVolume(ctx context.Context, req *platformv1.CreateVolumeRequest) (*platformv1.Volume, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	volume, err := s.store.createScheduledVolume(ctx, identity.UserID, req.GetEnvironmentId(), req.GetName(), req.GetSizeBytes())
	if err != nil {
		if errors.Is(err, errNoPlacementAvailable) {
			return nil, status.Errorf(codes.FailedPrecondition, "create volume: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "create volume: %v", err)
	}
	slog.Info("volume created", "volume_id", volume.ID, "environment_id", volume.EnvironmentID)
	return toProtoVolume(volume), nil
}

func (s *PlatformService) DeleteVolume(ctx context.Context, req *platformv1.DeleteVolumeRequest) (*emptypb.Empty, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.store.deleteVolume(ctx, identity.UserID, "", req.GetVolumeId()); err != nil {
		if errors.Is(err, errVolumeInUse) {
			return nil, status.Errorf(codes.FailedPrecondition, "delete volume: %v", err)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "volume: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "delete volume: %v", err)
	}
	s.notifyAllAgents(ctx)
	return &emptypb.Empty{}, nil
}

func (s *PlatformService) ListVolumes(ctx context.Context, req *platformv1.ListVolumesRequest) (*platformv1.ListVolumesResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.store.listVolumes(ctx, identity.UserID, req.GetEnvironmentId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list volumes: %v", err)
	}
	resp := &platformv1.ListVolumesResponse{Volumes: make([]*platformv1.Volume, 0, len(items))}
	for _, item := range items {
		resp.Volumes = append(resp.Volumes, toProtoVolume(item))
	}
	return resp, nil
}

func (s *PlatformService) CreateDomainBinding(ctx context.Context, req *platformv1.CreateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetBinding() == nil {
		return nil, status.Error(codes.InvalidArgument, "binding is required")
	}
	service, err := s.store.serviceByID(ctx, identity.UserID, "", req.GetBinding().GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
	}
	hostname, err := canonicalDomainHostname(req.GetBinding().GetHostname())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "hostname: %v", err)
	}
	targetPort := req.GetBinding().GetTargetPort()
	if err := validatePort(targetPort); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
	}
	if isPlatformHostname(hostname, s.platformDomainSuffix) {
		return nil, status.Error(codes.InvalidArgument, "hostname is reserved for generated platform domains")
	}
	if _, err := s.store.platformDomainBindingForService(ctx, identity.UserID, service.ProjectID, req.GetBinding().GetServiceId()); err != nil {
		if errors.Is(err, errPlatformDomainNotGenerated) || errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.FailedPrecondition, "domain ownership: %v", errPlatformDomainNotGenerated)
		}
		return nil, status.Errorf(codes.Internal, "load platform domain: %v", err)
	}
	binding, changed, err := s.store.createDomainBinding(ctx, identity.UserID, service.ProjectID, hostname, req.GetBinding().GetServiceId(), targetPort)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, errDomainAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "create domain binding: %v", err)
		}
		if errors.Is(err, errInvalidPort) {
			return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "create domain binding: %v", err)
	}
	if changed {
		s.notifyServices(ctx, identity.UserID, "", binding.ServiceID)
		s.ingress.RequestSync()
		if _, err := s.events.Publish(ctx, service.EnvironmentID); err != nil {
			return nil, status.Errorf(codes.Internal, "publish domain event: %v", err)
		}
	}
	return s.annotateDomainBinding(ctx, identity.UserID, service.ProjectID, binding), nil
}

func (s *PlatformService) GenerateDomainBinding(ctx context.Context, req *platformv1.GenerateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	targetPort := req.GetTargetPort()
	if err := validatePort(targetPort); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
	}
	service, err := s.store.serviceByID(ctx, identity.UserID, "", req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "service: %v", err)
	}
	hostname, err := generatedPlatformHostname(service.ProjectID, req.GetServiceId(), s.platformDomainSuffix)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "generate platform hostname: %v", err)
	}
	binding, changed, err := s.store.createPlatformDomainBinding(ctx, identity.UserID, service.ProjectID, hostname, req.GetServiceId(), targetPort)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "service: %v", err)
		}
		if errors.Is(err, errDomainAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "generate domain binding: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "generate domain binding: %v", err)
	}
	if changed {
		s.notifyServices(ctx, identity.UserID, "", binding.ServiceID)
		s.ingress.RequestSync()
		if _, err := s.events.Publish(ctx, service.EnvironmentID); err != nil {
			return nil, status.Errorf(codes.Internal, "publish domain event: %v", err)
		}
	}
	return s.annotateDomainBinding(ctx, identity.UserID, service.ProjectID, binding), nil
}

func (s *PlatformService) GetDomainBinding(ctx context.Context, req *platformv1.GetDomainBindingRequest) (*platformv1.DomainBinding, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	binding, err := s.store.domainBindingByHostname(ctx, identity.UserID, "", req.GetHostname())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain binding: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "get domain binding: %v", err)
	}
	return s.annotateDomainBinding(ctx, identity.UserID, binding.ProjectID, binding), nil
}

func (s *PlatformService) ListDomainBindings(ctx context.Context, req *platformv1.ListDomainBindingsRequest) (*platformv1.ListDomainBindingsResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.store.listDomainBindings(ctx, identity.UserID, "", req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list domain bindings: %v", err)
	}
	resp := &platformv1.ListDomainBindingsResponse{Bindings: make([]*platformv1.DomainBinding, 0, len(items))}
	for _, item := range items {
		resp.Bindings = append(resp.Bindings, s.annotateDomainBinding(ctx, identity.UserID, item.ProjectID, item))
	}
	return resp, nil
}

func (s *PlatformService) UpdateDomainBinding(ctx context.Context, req *platformv1.UpdateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetBinding() == nil {
		return nil, status.Error(codes.InvalidArgument, "binding is required")
	}
	targetPort := req.GetBinding().GetTargetPort()
	if err := validatePort(targetPort); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
	}
	var previousServiceID string
	if previous, err := s.store.domainBindingByHostname(ctx, identity.UserID, "", req.GetHostname()); err == nil {
		previousServiceID = previous.ServiceID
	}
	binding, changed, err := s.store.updateDomainBinding(ctx, identity.UserID, "", req.GetHostname(), req.GetBinding().GetServiceId(), targetPort)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain binding: %v", err)
		}
		if errors.Is(err, errInvalidPort) {
			return nil, status.Errorf(codes.InvalidArgument, "target port: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "update domain binding: %v", err)
	}
	if changed {
		s.notifyServices(ctx, identity.UserID, "", previousServiceID, binding.ServiceID)
		s.ingress.RequestSync()
		updated, err := s.store.serviceByID(ctx, identity.UserID, "", binding.ServiceID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "load updated domain service: %v", err)
		}
		if _, err := s.events.Publish(ctx, updated.EnvironmentID); err != nil {
			return nil, status.Errorf(codes.Internal, "publish domain event: %v", err)
		}
	}
	return s.annotateDomainBinding(ctx, identity.UserID, binding.ProjectID, binding), nil
}

func (s *PlatformService) DeleteDomainBinding(ctx context.Context, req *platformv1.DeleteDomainBindingRequest) (*emptypb.Empty, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	binding, err := s.store.domainBindingByHostname(ctx, identity.UserID, "", req.GetHostname())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain binding: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "get domain binding: %v", err)
	}
	if binding.PlatformGenerated {
		items, listErr := s.store.listDomainBindings(ctx, identity.UserID, "", binding.ServiceID)
		if listErr != nil {
			return nil, status.Errorf(codes.Internal, "list domain bindings: %v", listErr)
		}
		if hasCustomDomainBinding(items) {
			return nil, status.Errorf(codes.FailedPrecondition, "delete domain binding: %v", errPlatformDomainInUse)
		}
	}
	changed, err := s.store.deleteDomainBinding(ctx, identity.UserID, "", req.GetHostname())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain binding: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "delete domain binding: %v", err)
	}
	if !binding.PlatformGenerated {
		items, listErr := s.store.listDomainBindings(ctx, identity.UserID, "", binding.ServiceID)
		if listErr != nil {
			return nil, status.Errorf(codes.Internal, "list domain bindings: %v", listErr)
		}
		if leftover := leftoverPlatformHostname(items); leftover != "" {
			extra, deleteErr := s.store.deleteDomainBinding(ctx, identity.UserID, "", leftover)
			if deleteErr != nil && !errors.Is(deleteErr, sql.ErrNoRows) {
				return nil, status.Errorf(codes.Internal, "delete generated domain binding: %v", deleteErr)
			}
			changed = changed || extra
		}
	}
	if changed {
		s.notifyServices(ctx, identity.UserID, "", binding.ServiceID)
		s.ingress.RequestSync()
		updated, err := s.store.serviceByID(ctx, identity.UserID, "", binding.ServiceID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "load updated domain service: %v", err)
		}
		if _, err := s.events.Publish(ctx, updated.EnvironmentID); err != nil {
			return nil, status.Errorf(codes.Internal, "publish domain event: %v", err)
		}
	}
	return &emptypb.Empty{}, nil
}
