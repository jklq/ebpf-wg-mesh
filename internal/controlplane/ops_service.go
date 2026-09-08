package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"errors"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type OpsService struct {
	platformv1.UnimplementedOpsServiceServer
	webhooks  *GitHubWebhookHandler
	store     *fleetPersistence
	delivery  *deliverycore.Delivery
	notifier  *Notifier
	authority *TLSAuthority
}

func NewOpsService(webhooks *GitHubWebhookHandler, store *fleetPersistence, delivery *deliverycore.Delivery, notifier *Notifier, authority *TLSAuthority) *OpsService {
	return &OpsService{webhooks: webhooks, store: store, delivery: delivery, notifier: notifier, authority: authority}
}

func (s *OpsService) ListFleet(ctx context.Context, _ *emptypb.Empty) (*platformv1.Fleet, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	fleet, err := s.store.fleetView(ctx, identity.UserID)
	if err != nil {
		return nil, fleetStatusError("list fleet", err)
	}
	return fleet, nil
}

func (s *OpsService) CreateAgent(ctx context.Context, req *platformv1.CreateAgentRequest) (*platformv1.AgentEnrollment, error) {
	rec, token, err := s.delivery.CreateFleetAgent(ctx, req)
	if err != nil {
		return nil, fleetStatusError("create agent", err)
	}
	return &platformv1.AgentEnrollment{Agent: toProtoAgent(rec), BootstrapToken: token}, nil
}

func (s *OpsService) UpdateAgent(ctx context.Context, req *platformv1.UpdateAgentRequest) (*platformv1.Agent, error) {
	rec, err := s.delivery.UpdateFleetAgent(ctx, req)
	if err != nil {
		return nil, fleetStatusError("update agent", err)
	}
	return toProtoAgent(rec), nil
}

func (s *OpsService) SetAgentLifecycle(ctx context.Context, req *platformv1.SetAgentLifecycleRequest) (*platformv1.Agent, error) {
	identity, err := DelegatedUserFromContext(ctx)
	if err != nil {
		return nil, err
	}
	target := lifecycleStateRecord(req.GetLifecycleState())
	rec, notify, err := s.delivery.SetAgentLifecycle(ctx, identity.UserID, req.GetAgentId(), target)
	if err != nil {
		return nil, fleetStatusError("set agent lifecycle", err)
	}
	if rec.LifecycleState == deliverycore.AgentStateRetired && s.authority != nil {
		serials, serialErr := s.store.listAgentCertificateSerials(ctx, rec.ID)
		if serialErr != nil {
			return nil, status.Errorf(codes.Internal, "set agent lifecycle: load revoked serials: %v", serialErr)
		}
		if err := s.authority.RevokeSerials(serials); err != nil {
			return nil, status.Errorf(codes.Internal, "set agent lifecycle: revoke credentials: %v", err)
		}
	}
	if s.notifier != nil {
		s.notifier.NotifyAll(notify)
	}
	return toProtoAgent(rec), nil
}

func fleetStatusError(operation string, err error) error {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return status.Errorf(codes.PermissionDenied, "%s: operator access required or agent not found", operation)
	case errors.Is(err, deliverycore.ErrInvalidAgentTransition), errors.Is(err, deliverycore.ErrAgentHasAllocations):
		return status.Errorf(codes.FailedPrecondition, "%s: %v", operation, err)
	default:
		return status.Errorf(codes.InvalidArgument, "%s: %v", operation, err)
	}
}

func (s *OpsService) IngestGitHubWebhook(ctx context.Context, req *platformv1.IngestGitHubWebhookRequest) (*emptypb.Empty, error) {
	if s.webhooks == nil {
		return nil, status.Error(codes.FailedPrecondition, "github webhooks are not configured")
	}
	err := s.webhooks.HandleDelivery(
		ctx,
		req.GetSignature_256(),
		req.GetDeliveryId(),
		req.GetEventType(),
		req.GetPayload(),
	)
	switch {
	case err == nil:
		return &emptypb.Empty{}, nil
	case errors.Is(err, errGitHubWebhookInvalidSignature):
		return nil, status.Error(codes.Unauthenticated, err.Error())
	case errors.Is(err, errGitHubWebhookMissingHeaders):
		return nil, status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, errGitHubWebhookPayloadTooLarge):
		return nil, status.Error(codes.ResourceExhausted, err.Error())
	default:
		return nil, status.Errorf(codes.Internal, "ingest github webhook: %v", err)
	}
}
