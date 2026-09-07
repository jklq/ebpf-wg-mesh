package controlplane

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/restartpolicy"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type AgentService struct {
	agentv1.UnimplementedAgentControlServer
	store                   *Store
	delivery                agentDelivery
	logStore                *LogStore
	notifier                *Notifier
	authority               *TLSAuthority
	dashboard               *ManagedDashboardReconciler
	dashboardEnabled        bool
	dashboardTrustedAgentID string
	dashboardCallerID       string
	registry                *RegistryPolicy
}

type AgentServiceOption func(*AgentService)

type agentDelivery interface {
	ObserveAgentStatus(context.Context, string, *agentv1.StatusReport) error
	ReconcileFleetCapacity(context.Context) error
	DesiredStateForAgent(context.Context, string) (*agentv1.DesiredNodeState, error)
	RegisterAgent(context.Context, *agentv1.AgentHello) (bool, error)
}

func WithAgentRegistry(registry *RegistryPolicy) AgentServiceOption {
	return func(service *AgentService) {
		service.registry = registry
	}
}

func NewAgentService(store *Store, delivery agentDelivery, logStore *LogStore, notifier *Notifier, authority *TLSAuthority, dashboard *ManagedDashboardReconciler, dashboardEnabled bool, dashboardTrustedAgentID, dashboardCallerID string, opts ...AgentServiceOption) *AgentService {
	service := &AgentService{
		store: store, delivery: delivery, logStore: logStore, notifier: notifier, authority: authority, dashboard: dashboard,
		dashboardEnabled:        dashboardEnabled,
		dashboardTrustedAgentID: strings.TrimSpace(dashboardTrustedAgentID),
		dashboardCallerID:       strings.TrimSpace(dashboardCallerID),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(service)
		}
	}
	return service
}

func (s *AgentService) Enroll(ctx context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
	if err := checkClientCertificateRevocation(s.authority.revocations, verifiedClientCertificateFromContext(ctx)); err != nil {
		return nil, err
	}
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
	if err := s.store.authorizeAgentCredential(ctx, req.GetAgentId()); err != nil {
		return nil, status.Error(codes.PermissionDenied, "agent is not enrolled or its credentials are revoked")
	}
	if !authenticated {
		if err := s.store.consumeAgentBootstrapToken(ctx, req.GetAgentId(), req.GetBootstrapToken()); err != nil {
			return nil, status.Error(codes.Unauthenticated, "invalid bootstrap token")
		}
	}
	resp, err := s.authority.Enroll(req)
	if err != nil {
		return nil, err
	}
	if serial, err := certificateSerialFromPEM(resp.GetCertPem()); err != nil {
		return nil, status.Errorf(codes.Internal, "record agent certificate: %v", err)
	} else if err := s.store.recordAgentCertificate(ctx, req.GetAgentId(), serial); err != nil {
		return nil, status.Errorf(codes.Internal, "record agent certificate: %v", err)
	}
	slog.Info("agent certificate issued", "agent_id", req.GetAgentId(), "authenticated_renewal", authenticated)
	return resp, nil
}

func (s *AgentService) IssueManagedDashboardCertificate(ctx context.Context, req *agentv1.ManagedDashboardCertificateRequest) (*agentv1.EnrollResponse, error) {
	if err := checkClientCertificateRevocation(s.authority.revocations, verifiedClientCertificateFromContext(ctx)); err != nil {
		return nil, err
	}
	caller, err := ServiceCallerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if caller.Class != serviceCallerAgent || caller.ID != req.GetAgentId() {
		return nil, status.Error(codes.PermissionDenied, "client certificate does not match agent_id")
	}
	if !s.dashboardEnabled || s.dashboardTrustedAgentID == "" || caller.ID != s.dashboardTrustedAgentID {
		return nil, status.Error(codes.PermissionDenied, "managed dashboard certificate requires the trusted agent")
	}
	resp, err := s.authority.IssueManagedDashboardCertificate(s.dashboardCallerID, req.GetCsrPem())
	if err != nil {
		return nil, err
	}
	slog.Info("managed dashboard certificate issued", "agent_id", caller.ID, "dashboard_caller_id", s.dashboardCallerID)
	return resp, nil
}

func (s *AgentService) Sync(stream agentv1.AgentControl_SyncServer) error {
	ctx := stream.Context()
	if err := checkClientCertificateRevocation(s.authority.revocations, verifiedClientCertificateFromContext(ctx)); err != nil {
		return err
	}
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
	if err := s.store.authorizeAgentCredential(ctx, hello.GetAgentId()); err != nil {
		return status.Error(codes.PermissionDenied, "agent is not enrolled or its credentials are revoked")
	}
	changed, err := s.delivery.RegisterAgent(ctx, hello)
	if err != nil {
		return status.Errorf(codes.Internal, "register agent: %v", err)
	}
	if changed {
		if err := s.delivery.ReconcileFleetCapacity(ctx); err != nil {
			return status.Errorf(codes.Internal, "reconcile fleet capacity: %v", err)
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
		ids, err := s.store.deliveryQueries().AgentIDs(ctx)
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
		if err := checkClientCertificateRevocation(s.authority.revocations, verifiedClientCertificateFromContext(ctx)); err != nil {
			return err
		}
		if err := s.store.authorizeAgentCredential(ctx, hello.GetAgentId()); err != nil {
			return status.Error(codes.PermissionDenied, "agent credentials were revoked")
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
			if err := s.delivery.ObserveAgentStatus(ctx, hello.GetAgentId(), payload.StatusReport); err != nil {
				return status.Errorf(codes.Internal, "status report: %v", err)
			}
			s.emitCrashLoopEvents(ctx, hello.GetAgentId(), payload.StatusReport)
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
				state, err := s.delivery.DesiredStateForAgent(ctx, agentID)
				if err != nil {
					return nil, err
				}
				if err := s.attachRegistryPullCredentials(agentID, state); err != nil {
					return nil, err
				}
				return state, nil
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

func (s *AgentService) emitCrashLoopEvents(ctx context.Context, agentID string, report *agentv1.StatusReport) {
	if s == nil || report == nil || s.logStore == nil || !s.logStore.Enabled() {
		return
	}
	var lines []LogLineInput
	for _, cond := range report.GetServices() {
		if cond.GetPhase() != restartpolicy.PhaseCrashLoop && !cond.GetRestart().GetCrashLoop() {
			continue
		}
		alloc, err := s.store.allocationByServiceID(ctx, cond.GetServiceId())
		if err != nil {
			continue
		}
		message := cond.GetMessage()
		if message == "" {
			message = cond.GetRestart().GetMessage()
		}
		if message == "" {
			message = "allocation entered crash loop; authorized restart or new rollout required"
		}
		slog.Warn("allocation entered crash loop",
			"agent_id", agentID,
			"service_id", cond.GetServiceId(),
			"allocation_id", cond.GetAllocationId(),
			"message", message,
		)
		lines = append(lines, LogLineInput{
			ObservedAt:        time.Now().UTC(),
			EnvironmentID:     alloc.EnvironmentID,
			ServiceID:         cond.GetServiceId(),
			AllocationID:      cond.GetAllocationId(),
			AgentID:           agentID,
			Stream:            "combined",
			LogType:           LogTypeDeploy,
			Stage:             "restart",
			RolloutGeneration: cond.GetDesiredRolloutGeneration(),
			Sequence:          nextSynthSequence(),
			Line:              message,
		})
	}
	if len(lines) == 0 {
		return
	}
	if err := s.logStore.WriteLogLines(ctx, lines); err != nil {
		slog.Warn("write crash-loop event", "error", err, "agent_id", agentID)
	}
}

func (s *AgentService) attachRegistryPullCredentials(agentID string, state *agentv1.DesiredNodeState) error {
	if s == nil || s.registry == nil || !s.registry.Enabled() || state == nil {
		return nil
	}
	for _, service := range state.GetServices() {
		username, password, err := s.registry.CredentialsForPull(
			agentID+"-"+service.GetAllocationId(), service.GetEnvironmentId(), service.GetServiceId(), service.GetSpec().GetImage(),
		)
		if err != nil {
			return fmt.Errorf("mint pull credential for service %s: %w", service.GetServiceId(), err)
		}
		service.RegistryUsername = username
		service.RegistryPassword = password
	}
	return nil
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
