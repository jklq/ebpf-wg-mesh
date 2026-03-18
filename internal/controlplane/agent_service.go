package controlplane

import (
	"context"
	"errors"
	"io"
	"log/slog"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type AgentService struct {
	agentv1.UnimplementedAgentControlServer
	store     *Store
	notifier  *Notifier
	ingress   *IngressSyncer
	authority *TLSAuthority
	dashboard *ManagedDashboardReconciler
}

func NewAgentService(store *Store, notifier *Notifier, ingress *IngressSyncer, authority *TLSAuthority, dashboard *ManagedDashboardReconciler) *AgentService {
	return &AgentService{store: store, notifier: notifier, ingress: ingress, authority: authority, dashboard: dashboard}
}

func (s *AgentService) Enroll(ctx context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
	caller, authenticated, err := authenticatedServiceCallerFromContext(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "peer identity: %v", err)
	}
	allowBootstrap := true
	if authenticated {
		if caller.ID != req.GetAgentId() {
			return nil, status.Error(codes.PermissionDenied, "client certificate does not match agent_id")
		}
		allowBootstrap = false
	}
	resp, err := s.authority.Enroll(req, allowBootstrap)
	if err != nil {
		return nil, err
	}
	slog.Info("agent certificate issued", "agent_id", req.GetAgentId(), "authenticated_renewal", authenticated)
	return resp, nil
}

func (s *AgentService) Sync(stream agentv1.AgentControl_SyncServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first message must be hello")
	}
	caller, authenticated, err := authenticatedServiceCallerFromContext(ctx)
	if err != nil {
		return status.Errorf(codes.Unauthenticated, "peer identity: %v", err)
	}
	if !authenticated {
		return status.Error(codes.Unauthenticated, "client certificate is required")
	}
	if caller.ID != hello.GetAgentId() {
		return status.Error(codes.PermissionDenied, "client certificate does not match hello.agent_id")
	}
	changed, err := s.store.upsertAgent(ctx, hello)
	if err != nil {
		return status.Errorf(codes.Internal, "register agent: %v", err)
	}
	if changed {
		if _, err := s.store.nextDesiredRevision(ctx); err != nil {
			return status.Errorf(codes.Internal, "advance desired revision: %v", err)
		}
		if s.dashboard != nil {
			if err := s.dashboard.Reconcile(ctx); err != nil && !errors.Is(err, errNoPlacementAvailable) {
				slog.Warn("dashboard reconcile failed after agent change", "agent_id", hello.AgentId, "error", err)
			}
		}
	}
	slog.Info("agent connected", "agent_id", hello.AgentId, "name", hello.Name)
	notifyCh, stop := s.notifier.Watch(hello.AgentId)
	defer stop()

	sendErr := make(chan error, 1)
	go func() {
		sendErr <- s.sendLoop(ctx, stream, hello.AgentId, notifyCh)
	}()
	if changed {
		ids, err := s.store.agentIDs(ctx)
		if err != nil {
			return status.Errorf(codes.Internal, "list agents for notify: %v", err)
		}
		s.notifier.NotifyAll(ids)
	} else {
		s.notifier.Notify(hello.AgentId)
	}

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch payload := msg.Payload.(type) {
		case *agentv1.AgentClientMessage_Heartbeat:
			if err := s.store.heartbeatAgent(ctx, payload.Heartbeat.GetAgentId()); err != nil {
				return status.Errorf(codes.Internal, "heartbeat: %v", err)
			}
		case *agentv1.AgentClientMessage_StatusReport:
			slog.Info("agent status report", "agent_id", payload.StatusReport.GetAgentId(), "services", len(payload.StatusReport.GetServices()), "volumes", len(payload.StatusReport.GetVolumes()))
			if err := s.store.recordStatusReport(ctx, payload.StatusReport); err != nil {
				return status.Errorf(codes.Internal, "status report: %v", err)
			}
			_ = s.ingress.Sync(ctx)
		}
		select {
		case err := <-sendErr:
			return err
		default:
		}
	}
}

func (s *AgentService) sendLoop(ctx context.Context, stream agentv1.AgentControl_SyncServer, agentID string, notifyCh <-chan struct{}) error {
	var lastRevision int64 = -1
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-notifyCh:
			if !ok {
				return nil
			}
			slog.Info("notify received", "agent_id", agentID)
			state, err := s.store.desiredStateForAgent(ctx, agentID)
			if err != nil {
				return status.Errorf(codes.Internal, "desired state: %v", err)
			}
			if state.Revision == lastRevision {
				slog.Info("desired state unchanged", "agent_id", agentID, "revision", state.Revision)
				continue
			}
			slog.Info("sending desired state", "agent_id", agentID, "revision", state.Revision, "services", len(state.Services), "volumes", len(state.Volumes))
			if err := stream.Send(&agentv1.AgentServerMessage{
				Payload: &agentv1.AgentServerMessage_DesiredState{DesiredState: state},
			}); err != nil {
				return err
			}
			slog.Info("desired state sent", "agent_id", agentID, "revision", state.Revision)
			lastRevision = state.Revision
		}
	}
}
