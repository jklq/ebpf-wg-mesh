package controlplane

import (
	"context"
	"errors"
	"strings"

	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/secretkeys"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SealServiceSecret writes one sealed secret value. The value is write-only:
// it is sealed into ciphertext here and never returned by any read. Only the
// control-plane path assembling desired state decrypts it, for exactly the
// assigned allocations.
func (s *PlatformService) SealServiceSecret(ctx context.Context, req *platformv1.SealServiceSecretRequest) (*platformv1.SealServiceSecretResponse, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service id is required")
	}
	// Values are never logged, never echoed, and never included in errors.
	version, err := s.delivery.SealServiceSecret(ctx, user, req.GetServiceId(), req.GetName(), []byte(req.GetValue()))
	if err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		switch {
		case errors.Is(err, deliverycore.ErrInvalidSealedName):
			return nil, status.Errorf(codes.InvalidArgument, "seal secret: %v", err)
		case errors.Is(err, deliverycore.ErrSealedNameConflict):
			return nil, status.Errorf(codes.FailedPrecondition, "seal secret: %v", err)
		case errors.Is(err, deliverycore.ErrSealedSecretsUnavailable):
			return nil, status.Errorf(codes.FailedPrecondition, "seal secret: %v", err)
		case errors.Is(err, secretkeys.ErrSealedValueTooLarge):
			return nil, status.Errorf(codes.InvalidArgument, "seal secret: %v", err)
		}
		return nil, writeAccessError("seal secret", err)
	}
	return &platformv1.SealServiceSecretResponse{
		ServiceId: req.GetServiceId(),
		Name:      req.GetName(),
		Version:   version,
	}, nil
}

// DeleteServiceSecret tombstones a sealed secret. Pinned deployment reads
// keep resolving captured versions so rollback still restores them.
func (s *PlatformService) DeleteServiceSecret(ctx context.Context, req *platformv1.DeleteServiceSecretRequest) (*emptypb.Empty, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service id is required")
	}
	if err := s.delivery.DeleteServiceSecret(ctx, user, req.GetServiceId(), req.GetName()); err != nil {
		if mapped := s.liveOwnerError(ctx, err); mapped != nil {
			return nil, mapped
		}
		switch {
		case errors.Is(err, secretkeys.ErrNoSuchSecret):
			return nil, status.Errorf(codes.NotFound, "delete secret: %v", err)
		case errors.Is(err, deliverycore.ErrSealedSecretsUnavailable):
			return nil, status.Errorf(codes.FailedPrecondition, "delete secret: %v", err)
		}
		return nil, writeAccessError("delete secret", err)
	}
	return &emptypb.Empty{}, nil
}

// ListServiceSecrets returns masked existence records (names and versions,
// never values) for a service's live secrets.
func (s *PlatformService) ListServiceSecrets(ctx context.Context, req *platformv1.ListServiceSecretsRequest) (*platformv1.ListServiceSecretsResponse, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetServiceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service id is required")
	}
	metas, err := s.delivery.ListServiceSecrets(ctx, user, req.GetServiceId())
	if err != nil {
		if errors.Is(err, deliverycore.ErrSealedSecretsUnavailable) {
			return nil, status.Errorf(codes.FailedPrecondition, "list secrets: %v", err)
		}
		return nil, readAccessError("list secrets", err)
	}
	out := &platformv1.ListServiceSecretsResponse{}
	for _, meta := range metas {
		out.Secrets = append(out.Secrets, &platformv1.ServiceSecretMetadata{
			Name:      meta.Name,
			Version:   meta.Version,
			UpdatedAt: timestamppb.New(meta.UpdatedAt),
		})
	}
	return out, nil
}
