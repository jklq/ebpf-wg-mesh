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

type EnvironmentOperations struct {
	store    environmentStore
	notifier deliverycore.PlatformNotifier
	ingress  deliverycore.PlatformIngress
}

func NewEnvironmentOperations(store environmentStore, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress) *EnvironmentOperations {
	return &EnvironmentOperations{store: store, notifier: notifier, ingress: ingress}
}

func (s *EnvironmentOperations) DeleteEnvironment(ctx context.Context, req *platformv1.DeleteEnvironmentRequest) (*emptypb.Empty, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	agentIDs, err := s.store.deleteEnvironment(ctx, user, req.GetEnvironmentId())
	if err != nil {
		if errors.Is(err, deliverycore.ErrNotLiveOwner) || errors.Is(err, deliverycore.ErrLeaseLost) {
			return nil, err
		}
		if errors.Is(err, errProductionEnvironment) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, writeAccessError("delete environment", err)
	}
	for _, agentID := range agentIDs {
		s.notifier.Notify(agentID)
	}
	s.ingress.RequestSync()
	return &emptypb.Empty{}, nil
}
