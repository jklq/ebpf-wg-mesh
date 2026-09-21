package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/source"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type OpsService struct {
	platformv1.UnimplementedOpsServiceServer
	webhooks  *source.GitHubWebhookHandler
	store     *fleetPersistence
	delivery  *deliverycore.Delivery
	notifier  *Notifier
	authority interface{ RevokeSerials([]string) error }
}

func NewOpsService(webhooks *source.GitHubWebhookHandler, store *fleetPersistence, delivery *deliverycore.Delivery, notifier *Notifier, authority interface{ RevokeSerials([]string) error }) *OpsService {
	return &OpsService{webhooks: webhooks, store: store, delivery: delivery, notifier: notifier, authority: authority}
}

func (s *OpsService) ListFleet(ctx context.Context, _ *emptypb.Empty) (*platformv1.Fleet, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	fleet, err := s.store.fleetView(ctx, user)
	if err != nil {
		return nil, fleetStatusError("list fleet", err)
	}
	return fleet, nil
}

func (s *OpsService) CreateAgent(ctx context.Context, req *platformv1.CreateAgentRequest) (*platformv1.AgentEnrollment, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	rec, token, err := s.delivery.CreateFleetAgent(ctx, user, req)
	if err != nil {
		return nil, fleetStatusError("create agent", err)
	}
	return &platformv1.AgentEnrollment{Agent: toProtoAgent(rec), BootstrapToken: token}, nil
}

func (s *OpsService) UpdateAgent(ctx context.Context, req *platformv1.UpdateAgentRequest) (*platformv1.Agent, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.delivery.UpdateFleetAgent(ctx, user, req)
	if err != nil {
		return nil, fleetStatusError("update agent", err)
	}
	return toProtoAgent(rec), nil
}

func (s *OpsService) SetAgentLifecycle(ctx context.Context, req *platformv1.SetAgentLifecycleRequest) (*platformv1.Agent, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	target := lifecycleStateRecord(req.GetLifecycleState())
	rec, _, err := s.delivery.SetAgentLifecycle(ctx, user, req.GetAgentId(), target)
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
	return toProtoAgent(rec), nil
}

func (s *OpsService) ListBuilders(ctx context.Context, _ *emptypb.Empty) (*platformv1.ListBuildersResponse, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	builders, err := s.delivery.ListBuilders(ctx, user)
	if err != nil {
		return nil, buildSchedulerStatusError("list builders", err)
	}
	resp := &platformv1.ListBuildersResponse{Builders: make([]*platformv1.BuilderWorker, 0, len(builders))}
	for _, builder := range builders {
		resp.Builders = append(resp.Builders, deliverycore.ToProtoBuilderWorker(builder))
	}
	return resp, nil
}

func (s *OpsService) SetBuilderDrain(ctx context.Context, req *platformv1.SetBuilderDrainRequest) (*platformv1.BuilderWorker, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetBuilderId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "builder_id is required")
	}
	rec, err := s.delivery.SetBuilderDrain(ctx, user, req.GetBuilderId(), req.GetDrained())
	if err != nil {
		return nil, buildSchedulerStatusError("set builder drain", err)
	}
	return deliverycore.ToProtoBuilderWorker(rec), nil
}

func (s *OpsService) GetBuildScheduler(ctx context.Context, _ *emptypb.Empty) (*platformv1.BuildSchedulerState, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	state, err := s.delivery.BuildSchedulerState(ctx, user)
	if err != nil {
		return nil, buildSchedulerStatusError("get build scheduler", err)
	}
	return deliverycore.ToProtoBuildSchedulerState(state), nil
}

func (s *OpsService) SetBuildSchedulerPaused(ctx context.Context, req *platformv1.SetBuildSchedulerPausedRequest) (*platformv1.BuildSchedulerState, error) {
	user, err := authorizedUser(ctx)
	if err != nil {
		return nil, err
	}
	state, err := s.delivery.SetBuildSchedulerPaused(ctx, user, req.GetPaused())
	if err != nil {
		return nil, buildSchedulerStatusError("set build scheduler paused", err)
	}
	return deliverycore.ToProtoBuildSchedulerState(state), nil
}

// buildSchedulerStatusError keeps authz denials distinct from missing rows:
// denials wrap both authz.ErrDenied and sql.ErrNoRows, so check ErrDenied
// first and report a genuinely unknown builder as NotFound.
func buildSchedulerStatusError(operation string, err error) error {
	switch {
	case errors.Is(err, authz.ErrDenied):
		return status.Errorf(codes.PermissionDenied, "%s: operator access required", operation)
	case errors.Is(err, sql.ErrNoRows):
		return status.Errorf(codes.NotFound, "%s: %v", operation, err)
	default:
		return status.Errorf(codes.Internal, "%s: %v", operation, err)
	}
}

func fleetStatusError(operation string, err error) error {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return status.Errorf(codes.PermissionDenied, "%s: operator access required or agent not found", operation)
	case errors.Is(err, deliverycore.ErrInvalidFleetAgentInput):
		return status.Errorf(codes.InvalidArgument, "%s: %v", operation, err)
	case errors.Is(err, deliverycore.ErrInvalidAgentTransition), errors.Is(err, deliverycore.ErrAgentHasAllocations):
		return status.Errorf(codes.FailedPrecondition, "%s: %v", operation, err)
	default:
		return status.Errorf(codes.Internal, "%s: %v", operation, err)
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
	case errors.Is(err, source.ErrGitHubWebhookInvalidSignature):
		return nil, status.Error(codes.Unauthenticated, err.Error())
	case errors.Is(err, source.ErrGitHubWebhookMissingHeaders):
		return nil, status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, source.ErrGitHubWebhookPayloadTooLarge):
		return nil, status.Error(codes.ResourceExhausted, err.Error())
	default:
		return nil, status.Errorf(codes.Internal, "ingest github webhook: %v", err)
	}
}
