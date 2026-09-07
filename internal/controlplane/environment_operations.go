package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"errors"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// EnvironmentOperations owns environment removal, including cluster and ingress wakes.
type EnvironmentOperations struct {
	store    environmentStore
	notifier deliverycore.PlatformNotifier
	ingress  deliverycore.PlatformIngress
}

func NewEnvironmentOperations(store environmentStore, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress) *EnvironmentOperations {
	return &EnvironmentOperations{store: store, notifier: notifier, ingress: ingress}
}

func (s *EnvironmentOperations) DeleteEnvironment(ctx context.Context, req *platformv1.DeleteEnvironmentRequest) (*emptypb.Empty, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	agentIDs, err := s.store.deleteEnvironment(ctx, identity.UserID, req.GetEnvironmentId())
	if err != nil {
		if errors.Is(err, errProductionEnvironment) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Errorf(codes.Internal, "delete environment: %v", err)
	}
	for _, agentID := range agentIDs {
		s.notifier.Notify(agentID)
	}
	s.ingress.RequestSync()
	return &emptypb.Empty{}, nil
}
