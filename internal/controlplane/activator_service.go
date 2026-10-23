package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// activatorService is the internal wake endpoint for ingress activators.
// The activator caller class is enforced by the internal auth interceptor;
// the handler itself only ever sees a hostname, resolved server-side.
type activatorService struct {
	platformv1.UnimplementedActivatorServiceServer
	delivery     *deliverycore.Delivery
	wakeDeadline time.Duration
}

func (s *activatorService) WakeService(ctx context.Context, req *platformv1.WakeServiceRequest) (*platformv1.WakeServiceResponse, error) {
	if s.wakeDeadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.wakeDeadline)
		defer cancel()
	}
	result, err := s.delivery.WakeServiceByHostname(ctx, req.GetHostname())
	if errors.Is(err, deliverycore.ErrWakeHostnameNotFound) {
		return nil, status.Error(codes.NotFound, "no live domain binding for hostname")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "wake service: %v", err)
	}
	response := &platformv1.WakeServiceResponse{ServiceId: result.ServiceID, Ready: result.Ready}
	for _, endpoint := range result.Endpoints {
		response.Endpoints = append(response.Endpoints, &platformv1.WakeEndpoint{Address: endpoint.Address, Port: endpoint.Port})
	}
	return response, nil
}

// serverlessLoop runs the idle evaluator on the live owner.
func (s *Server) serverlessLoop(ctx context.Context) {
	idleTimeout := time.Duration(s.cfg.Serverless.IdleTimeoutSeconds) * time.Second
	interval := time.Duration(s.cfg.Serverless.EvalIntervalSeconds) * time.Second
	evaluate := func() {
		if err := s.delivery.EvaluateServerlessIdle(ctx, idleTimeout); err != nil && ctx.Err() == nil {
			slog.Warn("serverless idle evaluation failed", "error", err)
		}
	}
	evaluate()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			evaluate()
		}
	}
}
