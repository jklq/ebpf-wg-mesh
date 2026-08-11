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
	logStore  *LogStore
	notifier  *Notifier
	ingress   *IngressSyncer
	authority *TLSAuthority
	dashboard *ManagedDashboardReconciler
}

func NewAgentService(store *Store, logStore *LogStore, notifier *Notifier, ingress *IngressSyncer, authority *TLSAuthority, dashboard *ManagedDashboardReconciler) *AgentService {
	return &AgentService{store: store, logStore: logStore, notifier: notifier, ingress: ingress, authority: authority, dashboard: dashboard}
}

func (s *AgentService) Enroll(ctx context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
	caller, authenticated, err := authenticatedServiceCallerFromContext(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "peer identity: %v", err)
	}
	if authenticated {
		if caller.Class != serviceCallerAgent {
			return nil, status.Error(codes.PermissionDenied, "agent client certificate required")
		}
		if caller.ID != req.GetAgentId() {
			return nil, status.Error(codes.PermissionDenied, "client certificate does not match agent_id")
		}
	}
	resp, err := s.authority.Enroll(req)
	if err != nil {
		return nil, err
	}
	if !authenticated {
		if err := s.store.consumeAgentBootstrapToken(ctx, req.GetAgentId(), req.GetBootstrapToken()); err != nil {
			return nil, status.Error(codes.Unauthenticated, "invalid bootstrap token")
		}
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
	if caller.Class != serviceCallerAgent {
		return status.Error(codes.PermissionDenied, "agent client certificate required")
	}
	if caller.ID != hello.GetAgentId() {
		return status.Error(codes.PermissionDenied, "client certificate does not match hello.agent_id")
	}
	changed, err := s.store.upsertAgent(ctx, hello)
	if err != nil {
		return status.Errorf(codes.Internal, "register agent: %v", err)
	}
	if changed {
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
			if payload.Heartbeat.GetAgentId() != hello.GetAgentId() {
				return status.Error(codes.PermissionDenied, "heartbeat agent_id does not match session")
			}
			if err := s.store.heartbeatAgent(ctx, hello.GetAgentId()); err != nil {
				return status.Errorf(codes.Internal, "heartbeat: %v", err)
			}
		case *agentv1.AgentClientMessage_StatusReport:
			if payload.StatusReport.GetAgentId() != hello.GetAgentId() {
				return status.Error(codes.PermissionDenied, "status report agent_id does not match session")
			}
			slog.Info("agent status report", "agent_id", payload.StatusReport.GetAgentId(), "services", len(payload.StatusReport.GetServices()), "volumes", len(payload.StatusReport.GetVolumes()))
			ingressChanged, err := s.store.recordStatusReport(ctx, hello.GetAgentId(), payload.StatusReport)
			if err != nil {
				return status.Errorf(codes.Internal, "status report: %v", err)
			}
			if ingressChanged {
				s.ingress.RequestSync()
			}
		case *agentv1.AgentClientMessage_LogBatch:
			batch := payload.LogBatch
			if batch.GetAgentId() != hello.GetAgentId() {
				return status.Error(codes.PermissionDenied, "log batch agent_id does not match session")
			}
			if err := s.store.validateAgentLogBatch(ctx, hello.GetAgentId(), batch); err != nil {
				return status.Errorf(codes.PermissionDenied, "log batch ownership: %v", err)
			}
			if s.logStore != nil {
				if err := s.logStore.WriteAgentBatch(ctx, hello.GetAgentId(), batch); err != nil {
					return status.Errorf(codes.Internal, "log batch: %v", err)
				}
			}
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
			nextRevision, err := sendLatestDesiredState(ctx, agentID, lastRevision, func() (*agentv1.DesiredNodeState, error) {
				return s.store.desiredStateForAgent(ctx, agentID)
			}, func(state *agentv1.DesiredNodeState) error {
				return stream.Send(&agentv1.AgentServerMessage{
					Payload: &agentv1.AgentServerMessage_DesiredState{DesiredState: state},
				})
			})
			if err != nil {
				return status.Errorf(codes.Internal, "desired state: %v", err)
			}
			lastRevision = nextRevision
		}
	}
}

func sendLatestDesiredState(
	ctx context.Context,
	agentID string,
	lastRevision int64,
	load func() (*agentv1.DesiredNodeState, error),
	send func(*agentv1.DesiredNodeState) error,
) (int64, error) {
	_ = ctx
	for {
		state, err := load()
		if err != nil {
			return lastRevision, err
		}
		if state.Revision == lastRevision {
			slog.Info("desired state unchanged", "agent_id", agentID, "revision", state.Revision)
			return lastRevision, nil
		}
		slog.Info("sending desired state", "agent_id", agentID, "revision", state.Revision, "services", len(state.Services), "volumes", len(state.Volumes))
		if err := send(state); err != nil {
			return lastRevision, err
		}
		slog.Info("desired state sent", "agent_id", agentID, "revision", state.Revision)
		lastRevision = state.Revision
	}
}
