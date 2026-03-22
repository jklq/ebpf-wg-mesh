package controlplane

import (
	"context"
	"errors"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type OpsService struct {
	platformv1.UnimplementedOpsServiceServer
	webhooks *GitHubWebhookHandler
}

func NewOpsService(webhooks *GitHubWebhookHandler) *OpsService {
	return &OpsService{webhooks: webhooks}
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
	default:
		return nil, status.Errorf(codes.Internal, "ingest github webhook: %v", err)
	}
}
