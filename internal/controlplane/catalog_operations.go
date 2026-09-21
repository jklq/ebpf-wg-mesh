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

// CatalogOperations serves project and environment deletes and restores,
// fanning deletions out to agent wakeups and ingress syncs.
type CatalogOperations struct {
	store    catalogStore
	notifier deliverycore.PlatformNotifier
	ingress  deliverycore.PlatformIngress
}

// NewCatalogOperations builds the catalog delete/restore operator.
func NewCatalogOperations(store catalogStore, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress) *CatalogOperations {
	return &CatalogOperations{store: store, notifier: notifier, ingress: ingress}
}

func (s *CatalogOperations) DeleteEnvironment(ctx context.Context, req *platformv1.DeleteEnvironmentRequest) (*emptypb.Empty, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	agentIDs, err := s.store.deleteEnvironment(ctx, user, req.GetEnvironmentId(), req.GetConfirmationName())
	if err != nil {
		if errors.Is(err, deliverycore.ErrNotLiveOwner) || errors.Is(err, deliverycore.ErrLeaseLost) {
			return nil, err
		}
		if errors.Is(err, deliverycore.ErrConfirmationMismatch) {
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

func (s *CatalogOperations) RestoreEnvironment(ctx context.Context, environmentID string) (*platformv1.Environment, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.store.restoreEnvironment(ctx, user, environmentID)
	if err != nil {
		if errors.Is(err, deliverycore.ErrNotLiveOwner) || errors.Is(err, deliverycore.ErrLeaseLost) {
			return nil, err
		}
		if errors.Is(err, deliverycore.ErrAncestorDeleted) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, writeAccessError("restore environment", err)
	}
	// Restored rows re-enter the journal; wake everyone so desired state and
	// ingress converge without waiting for the next natural tick.
	if ids, err := s.store.AgentIDs(ctx); err == nil {
		for _, agentID := range ids {
			s.notifier.Notify(agentID)
		}
	}
	s.ingress.RequestSync()
	return toProtoEnvironment(rec), nil
}

func (s *CatalogOperations) DeleteProject(ctx context.Context, req *platformv1.DeleteProjectRequest) (*emptypb.Empty, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	agentIDs, err := s.store.deleteProject(ctx, user, req.GetProjectId(), req.GetConfirmationName())
	if err != nil {
		if errors.Is(err, deliverycore.ErrNotLiveOwner) || errors.Is(err, deliverycore.ErrLeaseLost) {
			return nil, err
		}
		if errors.Is(err, deliverycore.ErrConfirmationMismatch) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, writeAccessError("delete project", err)
	}
	for _, agentID := range agentIDs {
		s.notifier.Notify(agentID)
	}
	s.ingress.RequestSync()
	return &emptypb.Empty{}, nil
}

func (s *CatalogOperations) RestoreProject(ctx context.Context, projectID string) (*platformv1.Project, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.store.restoreProject(ctx, user, projectID)
	if err != nil {
		if errors.Is(err, deliverycore.ErrNotLiveOwner) || errors.Is(err, deliverycore.ErrLeaseLost) {
			return nil, err
		}
		return nil, writeAccessError("restore project", err)
	}
	if ids, err := s.store.AgentIDs(ctx); err == nil {
		for _, agentID := range ids {
			s.notifier.Notify(agentID)
		}
	}
	s.ingress.RequestSync()
	return toProtoProject(rec), nil
}
