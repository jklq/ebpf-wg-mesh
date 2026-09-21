package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func (s *PlatformService) CreateVolume(ctx context.Context, req *platformv1.CreateVolumeRequest) (*platformv1.Volume, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	if req.GetSizeBytes() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "volume size_bytes must be greater than 0")
	}
	environmentID := strings.TrimSpace(req.GetEnvironmentId())
	if environmentID == "" {
		return nil, status.Error(codes.InvalidArgument, "environment_id is required")
	}
	volume, err := s.store.createScheduledVolume(ctx, user, environmentID, name, req.GetSizeBytes())
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, deliverycore.ErrNoPlacementAvailable) || errors.Is(err, deliverycore.ErrEnvironmentDeleted) {
			return nil, status.Errorf(codes.FailedPrecondition, "create volume: %v", err)
		}
		if errors.Is(err, deliverycore.ErrVolumeAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "create volume: %v", err)
		}
		if errors.Is(err, deliverycore.ErrInvalidVolume) {
			return nil, status.Errorf(codes.InvalidArgument, "create volume: %v", err)
		}
		return nil, writeAccessError("create volume", err)
	}
	slog.Info("volume created", "volume_id", volume.ID, "environment_id", volume.EnvironmentID)
	return toProtoVolume(volume), nil
}

func (s *PlatformService) DeleteVolume(ctx context.Context, req *platformv1.DeleteVolumeRequest) (*emptypb.Empty, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetVolumeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if err := s.store.deleteVolume(ctx, user, req.GetVolumeId(), req.GetConfirmationName()); err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		if errors.Is(err, deliverycore.ErrVolumeInUse) || errors.Is(err, deliverycore.ErrVolumeNotEmpty) || errors.Is(err, deliverycore.ErrConfirmationMismatch) {
			return nil, status.Errorf(codes.FailedPrecondition, "delete volume: %v", err)
		}
		return nil, writeAccessError("delete volume", err)
	}
	s.notifyAllAgents(ctx)
	return &emptypb.Empty{}, nil
}

func (s *PlatformService) ListVolumes(ctx context.Context, req *platformv1.ListVolumesRequest) (*platformv1.ListVolumesResponse, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.store.listVolumes(ctx, user, req.GetEnvironmentId(), req.GetIncludeDeleted())
	if err != nil {
		return nil, writeAccessError("list volumes", err)
	}
	resp := &platformv1.ListVolumesResponse{Volumes: make([]*platformv1.Volume, 0, len(items))}
	for _, item := range items {
		resp.Volumes = append(resp.Volumes, toProtoVolume(item))
	}
	return resp, nil
}

func (s *PlatformService) CreateDomainBinding(ctx context.Context, req *platformv1.CreateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	result, err := s.domains.CreateDomainBinding(ctx, user, req)
	if mapped := s.liveOwnerError(ctx, err); mapped != nil {
		return nil, mapped
	}
	return result, err
}

func (s *PlatformService) GenerateDomainBinding(ctx context.Context, req *platformv1.GenerateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	result, err := s.domains.GenerateDomainBinding(ctx, user, req)
	if mapped := s.liveOwnerError(ctx, err); mapped != nil {
		return nil, mapped
	}
	return result, err
}

func (s *PlatformService) GetDomainBinding(ctx context.Context, req *platformv1.GetDomainBindingRequest) (*platformv1.DomainBinding, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	binding, err := s.domains.GetDomainBinding(ctx, user, req.GetHostname())
	if err != nil {
		return nil, readAccessError("domain binding", err)
	}
	return binding, nil
}

func (s *PlatformService) ListDomainBindings(ctx context.Context, req *platformv1.ListDomainBindingsRequest) (*platformv1.ListDomainBindingsResponse, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.domains.ListDomainBindings(ctx, user, req.GetServiceId(), req.GetIncludeDeleted())
	if err != nil {
		return nil, writeAccessError("list domain bindings", err)
	}
	resp := &platformv1.ListDomainBindingsResponse{Bindings: make([]*platformv1.DomainBinding, 0, len(items))}
	resp.Bindings = append(resp.Bindings, items...)
	return resp, nil
}

func (s *PlatformService) UpdateDomainBinding(ctx context.Context, req *platformv1.UpdateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	result, err := s.domains.UpdateDomainBinding(ctx, user, req)
	if mapped := s.liveOwnerError(ctx, err); mapped != nil {
		return nil, mapped
	}
	return result, err
}

func (s *PlatformService) DeleteDomainBinding(ctx context.Context, req *platformv1.DeleteDomainBindingRequest) (*emptypb.Empty, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	result, err := s.domains.DeleteDomainBinding(ctx, user, req)
	if mapped := s.liveOwnerError(ctx, err); mapped != nil {
		return nil, mapped
	}
	return result, err
}

func (s *PlatformService) RestoreDomainBinding(ctx context.Context, req *platformv1.RestoreDomainBindingRequest) (*platformv1.DomainBinding, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetHostname()) == "" {
		return nil, status.Error(codes.InvalidArgument, "hostname is required")
	}
	result, err := s.domains.RestoreDomainBinding(ctx, user, req.GetHostname())
	if mapped := s.liveOwnerError(ctx, err); mapped != nil {
		return nil, mapped
	}
	return result, err
}
