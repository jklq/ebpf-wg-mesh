package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
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
		if errors.Is(err, deliverycore.ErrNoPlacementAvailable) {
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
	if err := s.store.deleteVolume(ctx, identity.UserID, req.GetVolumeId()); err != nil {
		if errors.Is(err, deliverycore.ErrVolumeInUse) {
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
	if _, err := DelegatedUserFromContext(ctx); err != nil {
		return nil, err
	}
	return s.domains.CreateDomainBinding(ctx, req)
}

func (s *PlatformService) GenerateDomainBinding(ctx context.Context, req *platformv1.GenerateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	if _, err := DelegatedUserFromContext(ctx); err != nil {
		return nil, err
	}
	return s.domains.GenerateDomainBinding(ctx, req)
}

func (s *PlatformService) GetDomainBinding(ctx context.Context, req *platformv1.GetDomainBindingRequest) (*platformv1.DomainBinding, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	binding, err := s.store.domainBindingByHostname(ctx, identity.UserID, req.GetHostname())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "domain binding: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "get domain binding: %v", err)
	}
	return s.domains.annotateDomainBinding(ctx, identity.UserID, binding), nil
}

func (s *PlatformService) ListDomainBindings(ctx context.Context, req *platformv1.ListDomainBindingsRequest) (*platformv1.ListDomainBindingsResponse, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.store.listDomainBindings(ctx, identity.UserID, req.GetServiceId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list domain bindings: %v", err)
	}
	resp := &platformv1.ListDomainBindingsResponse{Bindings: make([]*platformv1.DomainBinding, 0, len(items))}
	for _, item := range items {
		resp.Bindings = append(resp.Bindings, s.domains.annotateDomainBinding(ctx, identity.UserID, item))
	}
	return resp, nil
}

func (s *PlatformService) UpdateDomainBinding(ctx context.Context, req *platformv1.UpdateDomainBindingRequest) (*platformv1.DomainBinding, error) {
	if _, err := DelegatedUserFromContext(ctx); err != nil {
		return nil, err
	}
	return s.domains.UpdateDomainBinding(ctx, req)
}

func (s *PlatformService) DeleteDomainBinding(ctx context.Context, req *platformv1.DeleteDomainBindingRequest) (*emptypb.Empty, error) {
	if _, err := DelegatedUserFromContext(ctx); err != nil {
		return nil, err
	}
	return s.domains.DeleteDomainBinding(ctx, req)
}
