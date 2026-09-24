package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/registry"
	"ebof-wg-mesh/internal/reconciliation"
	"ebof-wg-mesh/internal/restartpolicy"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type AgentService struct {
	agentv1.UnimplementedAgentControlServer
	store                   *fleetPersistence
	delivery                agentDelivery
	logStore                *logs.LogStore
	notifier                *Notifier
	authority               *identity.TLSAuthority
	enrollment              *identity.Enrollment
	dashboard               *ManagedDashboardReconciler
	dashboardEnabled        bool
	dashboardTrustedAgentID string
	dashboardCallerID       string
	registry                *registry.Policy
	replicaAddresses        []string
	liveOwner               LiveOwner

	credMu    sync.Mutex
	credCache map[string]cachedPullCredential
	credNow   func() time.Time
	// credReuseHorizon is the interval until the next guaranteed credential
	// refresh: agent sessions rotate at least every client-certificate
	// lifetime and re-deliver credentials there.
	credReuseHorizon time.Duration
}

type cachedPullCredential struct {
	username  string
	password  string
	mintedAt  time.Time
	expiresAt time.Time
	image     string
}

type LiveOwner interface {
	Lookup(context.Context) (held bool, advertiseAddr string, err error)
}

type liveOwnerWatcher interface {
	Watch() (<-chan struct{}, func())
}

type leaseLiveOwner struct {
	leases *LeaseManager
	name   string
}

func (o leaseLiveOwner) Lookup(ctx context.Context) (bool, string, error) {
	if o.leases == nil {
		return false, "", nil
	}
	return o.leases.Lookup(ctx, o.name)
}

func (o leaseLiveOwner) Watch() (<-chan struct{}, func()) {
	if o.leases == nil {
		ch := make(chan struct{})
		return ch, func() { close(ch) }
	}
	return o.leases.Watch(o.name)
}

func watchLiveOwner(owner LiveOwner) (<-chan struct{}, func()) {
	if watcher, ok := owner.(liveOwnerWatcher); ok {
		return watcher.Watch()
	}
	return nil, func() {}
}

type AgentServiceOption func(*AgentService)

type agentDelivery interface {
	ObserveAgentStatus(context.Context, string, *agentv1.StatusReport) error
	ObserveAgentHeartbeat(context.Context, string, string, bool) error
	EndAgentSession(context.Context, string, string) error
	ReconcileFleetCapacity(context.Context) error
	DesiredStateForAgent(context.Context, string) (*agentv1.DesiredNodeState, error)
	AllocationDiffsFrom(string, int64) ([]*agentv1.AllocationDiff, int64, bool)
	RebaseAllocationDiffs(string, *agentv1.DesiredNodeState)
	RegisterAgent(context.Context, *agentv1.AgentHello) (bool, error)
}

func WithAgentRegistry(policy *registry.Policy) AgentServiceOption {
	return func(service *AgentService) {
		service.registry = policy
	}
}

func WithReplicaAddresses(addresses []string) AgentServiceOption {
	return func(service *AgentService) {
		service.replicaAddresses = normalizeReplicaAddresses(addresses)
	}
}

func WithLiveOwner(owner LiveOwner) AgentServiceOption {
	return func(service *AgentService) {
		service.liveOwner = owner
	}
}

func (s *AgentService) requireLiveOwner(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if s.liveOwner != nil {
		held, ownerAddr, err := s.liveOwner.Lookup(ctx)
		if err != nil {
			return status.Errorf(codes.Unavailable, "lookup live owner: %v", err)
		}
		if held {
			return nil
		}
		if strings.TrimSpace(ownerAddr) == "" {
			return status.Error(codes.Unavailable, "live owner is not ready")
		}
		return agentv1.LiveOwnerRedirect(ownerAddr)
	}
	if s.store == nil || s.store.sessions == nil || s.store.sessions.Serving() {
		return nil
	}
	addr, err := s.store.liveOwnerAddr(ctx)
	if err != nil {
		return status.Errorf(codes.Unavailable, "lookup live owner: %v", err)
	}
	if addr != "" {
		return status.Error(codes.FailedPrecondition, deliverycore.LiveOwnerRedirectMessage(addr))
	}
	return status.Error(codes.Unavailable, "live owner is not ready")
}

func NewAgentService(store *fleetPersistence, delivery agentDelivery, logStore *logs.LogStore, notifier *Notifier, authority *identity.TLSAuthority, dashboard *ManagedDashboardReconciler, dashboardEnabled bool, dashboardTrustedAgentID, dashboardCallerID string, opts ...AgentServiceOption) *AgentService {
	service := &AgentService{
		store: store, delivery: delivery, logStore: logStore, notifier: notifier, authority: authority, dashboard: dashboard,
		enrollment:              identity.NewEnrollment(store, authority),
		credReuseHorizon:        authority.ClientCertificateTTL(),
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
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	resp, err := s.enrollment.EnrollAgent(ctx, req)
	if err != nil {
		return nil, err
	}
	return s.withReplicaAddresses(resp), nil
}

func (s *AgentService) IssueManagedDashboardCertificate(ctx context.Context, req *agentv1.ManagedDashboardCertificateRequest) (*agentv1.EnrollResponse, error) {
	if err := s.requireLiveOwner(ctx); err != nil {
		return nil, err
	}
	if err := identity.CheckClientCertificateRevocation(s.authority.Revocations(), identity.VerifiedClientCertificateFromContext(ctx)); err != nil {
		return nil, err
	}
	caller, err := identity.ServiceCallerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if caller.Class != identity.CallerAgent || caller.ID != req.GetAgentId() {
		return nil, status.Error(codes.PermissionDenied, "client certificate does not match agent_id")
	}
	if !s.dashboardEnabled || s.dashboardTrustedAgentID == "" || caller.ID != s.dashboardTrustedAgentID {
		return nil, status.Error(codes.PermissionDenied, "managed dashboard certificate requires the trusted agent")
	}
	resp, err := s.authority.IssueManagedDashboardCertificate(ctx, s.dashboardCallerID, req.GetCsrPem())
	if err != nil {
		return nil, err
	}
	slog.Info("managed dashboard certificate issued", "agent_id", caller.ID, "dashboard_caller_id", s.dashboardCallerID)
	return s.withReplicaAddresses(resp), nil
}

func (s *AgentService) Sync(stream agentv1.AgentControl_SyncServer) error {
	ctx := stream.Context()
	ownerChanged, stopOwnerWatch := watchLiveOwner(s.liveOwner)
	defer stopOwnerWatch()
	if err := s.requireLiveOwner(ctx); err != nil {
		return err
	}
	if err := identity.CheckClientCertificateRevocation(s.authority.Revocations(), identity.VerifiedClientCertificateFromContext(ctx)); err != nil {
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
	caller, authenticated, err := identity.AuthenticatedServiceCallerFromContext(ctx)
	if err != nil {
		return status.Errorf(codes.Unauthenticated, "peer identity: %v", err)
	}
	if !authenticated {
		return status.Error(codes.Unauthenticated, "client certificate is required")
	}
	if caller.Class != identity.CallerAgent {
		return status.Error(codes.PermissionDenied, "agent client certificate required")
	}
	if caller.ID != hello.GetAgentId() {
		return status.Error(codes.PermissionDenied, "client certificate does not match hello.agent_id")
	}
	epoch, err := s.store.agentAuthorityEpoch(ctx)
	if err != nil {
		return status.Errorf(codes.Internal, "read agent authority: %v", err)
	}
	// The active cluster identity flips at CA rotate-start; the retiring
	// identity stays accepted so pre-renewal agents stay connected
	// through the overlap.
	trustedCluster, err := s.authority.VerifyClusterID(ctx, hello.GetClusterId())
	if err != nil {
		return status.Errorf(codes.Unavailable, "verify cluster identity: %v", err)
	}
	if !trustedCluster {
		return status.Error(codes.FailedPrecondition, "identity recovery required: cluster differs from authenticated authority")
	}
	switch hello.GetInitializationState() {
	case "uninitialized", "ready", "recovery":
	default:
		return status.Error(codes.InvalidArgument, "local-store initialization_state is required")
	}
	if hello.GetAcceptedAuthorityEpoch() > epoch {
		return status.Errorf(codes.FailedPrecondition, "agent authority epoch %d is newer than control-plane epoch %d", hello.GetAcceptedAuthorityEpoch(), epoch)
	}
	if err := s.store.AuthorizeAgentCredential(ctx, hello.GetAgentId()); err != nil {
		return status.Error(codes.PermissionDenied, "agent is not enrolled or its credentials are revoked")
	}
	if hello.GetWireguardListenPort() < 1 || hello.GetWireguardListenPort() > 65535 {
		return status.Error(codes.InvalidArgument, "wireguard_listen_port must be between 1 and 65535")
	}
	if err := validateAgentEndpointAgainstPeer(ctx, hello); err != nil {
		return err
	}
	changed, err := s.delivery.RegisterAgent(ctx, hello)
	if errors.Is(err, deliverycore.ErrStaleAgentSession) {
		return status.Error(codes.FailedPrecondition, "stale session incarnation")
	}
	if errors.Is(err, reconciliation.ErrIdentityRecovery) {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	if err != nil {
		return status.Errorf(codes.Internal, "register agent: %v", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.delivery.EndAgentSession(closeCtx, hello.GetAgentId(), hello.GetSessionId()); err != nil {
			slog.Warn("end agent session", "agent_id", hello.GetAgentId(), "error", err)
		}
	}()
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
		sendErr <- s.sendLoop(ctx, stream, hello.AgentId, hello.GetSessionId(), hello.GetClusterId(), epoch, hello, notifyCh, ownerChanged)
	}()
	s.notifier.Notify(hello.AgentId)

	type receivedMessage struct {
		message *agentv1.AgentClientMessage
		err     error
	}
	received := make(chan receivedMessage, 1)
	go func() {
		for {
			message, err := stream.Recv()
			select {
			case received <- receivedMessage{message, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		var msg *agentv1.AgentClientMessage
		var err error
		select {
		case result := <-received:
			msg, err = result.message, result.err
		case err := <-sendErr:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := identity.CheckClientCertificateRevocation(s.authority.Revocations(), identity.VerifiedClientCertificateFromContext(ctx)); err != nil {
			return err
		}
		if err := s.store.AuthorizeAgentCredential(ctx, hello.GetAgentId()); err != nil {
			return status.Error(codes.PermissionDenied, "agent credentials were revoked")
		}
		switch payload := msg.Payload.(type) {
		case *agentv1.AgentClientMessage_Acknowledgement:
			ack := payload.Acknowledgement
			if ack.GetAgentId() != hello.GetAgentId() || ack.GetSessionId() != hello.GetSessionId() || ack.GetAuthorityEpoch() != epoch {
				return status.Error(codes.FailedPrecondition, "acknowledgement does not match session authority")
			}
			if err := s.store.acknowledgeAgentDesired(ctx, ack); err != nil {
				return status.Errorf(codes.FailedPrecondition, "acknowledgement: %v", err)
			}
		case *agentv1.AgentClientMessage_Heartbeat:
			if payload.Heartbeat.GetAgentId() != hello.GetAgentId() {
				return status.Error(codes.PermissionDenied, "heartbeat agent_id does not match session")
			}
			if payload.Heartbeat.GetSessionId() != hello.GetSessionId() {
				return status.Error(codes.FailedPrecondition, "heartbeat session_id is stale")
			}
			if err := s.delivery.ObserveAgentHeartbeat(ctx, hello.GetAgentId(), payload.Heartbeat.GetSessionId(), payload.Heartbeat.GetRecoveryMode()); err != nil {
				return status.Errorf(codes.FailedPrecondition, "heartbeat: %v", err)
			}
		case *agentv1.AgentClientMessage_StatusReport:
			if payload.StatusReport.GetAgentId() != hello.GetAgentId() {
				return status.Error(codes.PermissionDenied, "status report agent_id does not match session")
			}
			if payload.StatusReport.GetSessionId() != hello.GetSessionId() {
				return status.Error(codes.FailedPrecondition, "status report session_id is stale")
			}
			if payload.StatusReport.GetAuthorityEpoch() != epoch {
				return status.Errorf(codes.FailedPrecondition, "status report authority epoch %d does not match %d", payload.StatusReport.GetAuthorityEpoch(), epoch)
			}
			slog.Info("agent status report", "agent_id", payload.StatusReport.GetAgentId(), "services", len(payload.StatusReport.GetServices()), "volumes", len(payload.StatusReport.GetVolumes()), "recovery_mode", payload.StatusReport.GetRecoveryMode())
			if err := s.delivery.ObserveAgentStatus(ctx, hello.GetAgentId(), payload.StatusReport); err != nil {
				return status.Errorf(codes.FailedPrecondition, "status report: %v", err)
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
	}
}

// syncSent is the position delivered on the session's sync stream: the
// monotonic allocation cursor plus content versions for the independently
// delivered streams and the observation overlay. The overlay versions the
// observation-derived fields of accepted services (internal hosts, restart
// observations); it drifts at a fixed cursor, so it is tracked alongside the
// streams and drives same-cursor repair checkpoints whenever it moves past
// the last delivered position.
type syncSent struct {
	alloc       int64
	nodeConfig  string
	credentials string
	replicas    string
	overlay     string
}

func (s *AgentService) sendLoop(ctx context.Context, stream agentv1.AgentControl_SyncServer, agentID, sessionID, clusterID string, epoch uint64, hello *agentv1.AgentHello, notifyCh, ownerChanged <-chan struct{}) error {
	// 2.10: initialize from hello so an unchanged reconnect sends nothing.
	sent := syncSent{
		alloc:       hello.GetReconciliationCursor(),
		nodeConfig:  hello.GetAcceptedNodeConfigVersion(),
		credentials: hello.GetAcceptedCredentialsVersion(),
		replicas:    hello.GetAcceptedReplicasVersion(),
		overlay:     hello.GetAcceptedObservationOverlayVersion(),
	}
	helloInventory := hello.GetAllocations()
	helloInit := hello.GetInitializationState()
	helloEpoch := hello.GetAcceptedAuthorityEpoch()
	first := true
	for {
		slog.Info("checking desired state", "agent_id", agentID)
		next, err := s.sendSyncBatch(ctx, stream, agentID, sessionID, clusterID, epoch, sent, helloInventory, helloInit, helloEpoch, first)
		if err != nil {
			return err
		}
		sent = next
		first = false
		helloInventory = nil
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-notifyCh:
			if !ok {
				return nil
			}
		case _, ok := <-ownerChanged:
			if !ok {
				return nil
			}
			if err := s.requireLiveOwner(ctx); err != nil {
				return err
			}
			return status.Error(codes.Unavailable, "live owner changed; reconnect required")
		}
	}
}

// sendSyncBatch loads current state, reconciles inventory on first send, and
// emits a single fenced batch — node config, credentials, allocations
// (checkpoint or ordered diffs), then replicas — terminated by a batch-end
// marker. It returns the new sent position (unchanged when nothing was sent).
func (s *AgentService) sendSyncBatch(ctx context.Context, stream agentv1.AgentControl_SyncServer, agentID, sessionID, clusterID string, epoch uint64, sent syncSent, helloInventory []*agentv1.ServiceCondition, helloInit string, helloEpoch uint64, first bool) (syncSent, error) {
	state, err := s.delivery.DesiredStateForAgent(ctx, agentID)
	if err != nil {
		return sent, status.Errorf(codes.Internal, "desired state: %v", err)
	}
	if state.GetAuthorityEpoch() != epoch {
		return sent, status.Error(codes.Internal, "snapshot authority changed; reconnect required")
	}
	creds, err := s.pullCredentialsForAgent(ctx, agentID, state)
	if err != nil {
		return sent, status.Errorf(codes.Internal, "pull credentials: %v", err)
	}
	current := deliverycore.SyncVersions{
		Cursor:      state.GetReconciliationCursor(),
		NodeConfig:  state.GetNodeConfigVersion(),
		Credentials: creds.GetCredentialsVersion(),
		Replicas:    deliverycore.HashReplicas(s.replicaAddresses),
	}
	currentOverlay := reconciliation.HashObservationOverlay(state.GetServices())
	if current.Cursor < sent.alloc {
		return sent, status.Error(codes.FailedPrecondition, "agent cursor is ahead of control plane; recovery required")
	}
	needCheckpoint := false
	var diffs []*agentv1.AllocationDiff
	if first && helloInit != "ready" {
		// Initialization or recovery establishes the desired set with a
		// checkpoint, even when the cursor matches. Diffs require a prior
		// checkpoint baseline.
		slog.Info("establishing desired set with checkpoint", "agent_id", agentID, "init", helloInit)
		needCheckpoint = true
	} else if first && helloEpoch != epoch {
		// Takeover advances the epoch; the checkpoint carries the new
		// authority even when allocation content is unchanged.
		slog.Info("authority epoch changed; sending checkpoint", "agent_id", agentID, "hello_epoch", helloEpoch, "epoch", epoch)
		needCheckpoint = true
	} else if first && sent.overlay != currentOverlay {
		// Observation-derived fields (internal hosts, restart observations)
		// drift without a desired_revision bump. A ready reconnect echoes the
		// accepted observation overlay version and gets a repair checkpoint
		// when it trails; the checkpoint covers the overlay in full even when
		// the cursor also advanced and retained diffs would reach only their
		// changed services.
		slog.Info("observation overlay drift; repairing with checkpoint", "agent_id", agentID)
		needCheckpoint = true
	} else if current.Cursor == sent.alloc {
		if first && !deliverycore.InventoriesMatch(helloInventory, state.GetServices()) {
			slog.Info("allocation inventory mismatch; repairing with checkpoint", "agent_id", agentID, "cursor", current.Cursor)
			needCheckpoint = true
		} else if sent.overlay != currentOverlay {
			// Observation-derived fields drift at a fixed cursor: a live
			// health change adds or removes internal host entries without a
			// desired_revision bump, so no diff covers it. The sent position
			// tracks the overlay delivered on this session, and a connected
			// agent gets a same-cursor repair checkpoint whenever live
			// observations move the overlay past it — not only on reconnect.
			slog.Info("observation overlay drift; repairing with checkpoint", "agent_id", agentID, "cursor", current.Cursor)
			needCheckpoint = true
		}
	} else {
		// Cursor advanced; the batch must carry allocation coverage from
		// last delivered cursor to current.Cursor. A revision that changes no allocation
		// content (e.g., a peer-only bump still moves desired_revision) is
		// retained and sent as an empty no-op diff that advances the agent's
		// accepted cursor in lockstep. Returning the advanced cursor without
		// delivering a covering allocation message would leave the agent on
		// its old accepted cursor and the next diff would be rejected on its
		// base revision. Anything the retained chain cannot cover falls back
		// to a checkpoint.
		if stored, target, ok := s.delivery.AllocationDiffsFrom(agentID, sent.alloc); ok && target == current.Cursor && len(stored) > 0 {
			diffs = stored
		} else {
			slog.Info("diff history unavailable; sending checkpoint", "agent_id", agentID, "base", sent.alloc, "target", current.Cursor)
			needCheckpoint = true
		}
	}
	needNode := current.NodeConfig != sent.nodeConfig
	needCreds := current.Credentials != sent.credentials
	needReplicas := current.Replicas != sent.replicas
	needAlloc := needCheckpoint || len(diffs) > 0
	if !needAlloc && !needNode && !needCreds && !needReplicas {
		slog.Info("desired state unchanged", "agent_id", agentID, "cursor", current.Cursor)
		return sent, nil
	}
	// Single grant covers the whole batch with one expiry.
	deadline, err := s.store.grantAgentCommand(ctx, agentID, sessionID, epoch, current)
	if err != nil {
		return sent, status.Errorf(codes.Internal, "desired state: %v", err)
	}
	// Order: policy/peers first (fail closed), then credentials, then
	// allocations, then replica discovery.
	if needNode && !needCheckpoint {
		update := &agentv1.NodeConfigUpdate{
			AgentId: agentID, NodeConfigVersion: current.NodeConfig,
			NodeConfig: state.GetNodeConfig(), ClusterId: clusterID,
		}
		stampNodeConfigUpdate(update, sessionID, epoch, deadline)
		if err := stream.Send(&agentv1.AgentServerMessage{
			Payload: &agentv1.AgentServerMessage_NodeConfigUpdate{NodeConfigUpdate: update},
		}); err != nil {
			return sent, err
		}
	}
	if needCreds {
		creds.ClusterId = clusterID
		stampPullCredentials(creds, sessionID, epoch, deadline)
		if err := stream.Send(&agentv1.AgentServerMessage{
			Payload: &agentv1.AgentServerMessage_PullCredentials{PullCredentials: creds},
		}); err != nil {
			return sent, err
		}
	}
	if needCheckpoint {
		stampAgentCommand(state, sessionID, epoch, deadline)
		state.ClusterId = clusterID
		slog.Info("sending checkpoint", "agent_id", agentID, "cursor", state.GetReconciliationCursor(), "services", len(state.GetServices()), "volumes", len(state.GetVolumes()))
		if err := stream.Send(&agentv1.AgentServerMessage{
			Payload: &agentv1.AgentServerMessage_DesiredState{DesiredState: state},
		}); err != nil {
			return sent, err
		}
		// The checkpoint traveled outside the retained diff chain; move the
		// chain's baseline to the delivered content so a later diff can never
		// silently skip fields the checkpoint changed.
		s.delivery.RebaseAllocationDiffs(agentID, state)
	} else {
		for _, diff := range diffs {
			diff.ClusterId = clusterID
			stampAllocationDiff(diff, sessionID, epoch, deadline)
			slog.Info("sending diff", "agent_id", agentID, "base", diff.GetBaseRevision(), "target", diff.GetTargetRevision(), "starts", len(diff.GetStarts()), "updates", len(diff.GetUpdates()), "stops", len(diff.GetStops()))
			if err := stream.Send(&agentv1.AgentServerMessage{
				Payload: &agentv1.AgentServerMessage_AllocationDiff{AllocationDiff: diff},
			}); err != nil {
				return sent, err
			}
		}
	}
	if needReplicas {
		replicas := &agentv1.ReplicaEndpoints{
			AgentId: agentID, ReplicasVersion: current.Replicas,
			ReplicaAddresses: append([]string(nil), s.replicaAddresses...),
			ClusterId:        clusterID,
		}
		stampReplicaEndpoints(replicas, sessionID, epoch, deadline)
		if err := stream.Send(&agentv1.AgentServerMessage{
			Payload: &agentv1.AgentServerMessage_ReplicaEndpoints{ReplicaEndpoints: replicas},
		}); err != nil {
			return sent, err
		}
	}
	// Close the batch: the agent withholds status publication until this
	// marker, so a batch that pauses mid-stream can never surface an
	// intermediate inventory between its messages.
	if err := stream.Send(&agentv1.AgentServerMessage{
		Payload: &agentv1.AgentServerMessage_BatchEnd{BatchEnd: &agentv1.SyncBatchEnd{SessionId: sessionID}},
	}); err != nil {
		return sent, err
	}
	// Every stream that differed was delivered, and overlay drift is covered
	// by the checkpoint same-cursor repair forces or by the sent diffs —
	// whose entries carry the observed fields of every service changed since
	// the previous recorded snapshot — so the whole current position is now
	// accepted by the agent.
	return syncSent{
		alloc:       current.Cursor,
		nodeConfig:  current.NodeConfig,
		credentials: current.Credentials,
		replicas:    current.Replicas,
		overlay:     currentOverlay,
	}, nil
}

func (s *AgentService) withReplicaAddresses(resp *agentv1.EnrollResponse) *agentv1.EnrollResponse {
	if resp == nil {
		return nil
	}
	resp.ReplicaAddresses = append([]string(nil), s.replicaAddresses...)
	return resp
}

// validateAgentEndpointAgainstPeer validates the endpoint syntax and requires
// same-family endpoints to match the authenticated connection's observed
// source. A dual-stack host may dial over one family while advertising the
// other; the agent's separately self-reported advertise address is not trusted
// as proof for a conflicting address in the same family.
func validateAgentEndpointAgainstPeer(ctx context.Context, hello *agentv1.AgentHello) error {
	endpoint, err := deliverycore.CanonicalAgentWireGuardEndpoint(hello.GetWireguardEndpoint())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	advertise, err := deliverycore.CanonicalAgentAdvertiseAddr(hello.GetAdvertiseAddr())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	hello.WireguardEndpoint = endpoint
	hello.AdvertiseAddr = advertise
	endpointAddr := netip.MustParseAddrPort(endpoint).Addr().Unmap()
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return status.Error(codes.FailedPrecondition, "wireguard endpoint ownership cannot be verified without the peer address")
	}
	peerHost, _, err := net.SplitHostPort(p.Addr.String())
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "parse observed peer address: %v", err)
	}
	peerAddr, err := netip.ParseAddr(peerHost)
	if err != nil {
		return status.Error(codes.FailedPrecondition, "wireguard endpoint ownership could not be verified")
	}
	peerAddr = peerAddr.Unmap()
	if peerAddr.IsLoopback() || peerAddr.IsUnspecified() || endpointAddr == peerAddr || endpointAddr.Is4() != peerAddr.Is4() {
		return nil
	}
	return status.Errorf(codes.FailedPrecondition, "wireguard endpoint address %q does not match the observed peer %q", endpointAddr, peerAddr)
}

func normalizeReplicaAddresses(addresses []string) []string {
	result := make([]string, 0, len(addresses))
	seen := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		if _, ok := seen[address]; ok {
			continue
		}
		seen[address] = struct{}{}
		result = append(result, address)
	}
	return result
}

func (s *AgentService) emitCrashLoopEvents(ctx context.Context, agentID string, report *agentv1.StatusReport) {
	if s == nil || report == nil || s.logStore == nil || !s.logStore.Enabled() {
		return
	}
	var lines []logs.LogLineInput
	for _, cond := range report.GetServices() {
		if cond.GetPhase() != restartpolicy.PhaseCrashLoop && !cond.GetRestart().GetCrashLoop() {
			continue
		}
		alloc, err := s.store.reads.allocationByServiceID(ctx, cond.GetServiceId())
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
		lines = append(lines, logs.LogLineInput{
			ObservedAt:        time.Now().UTC(),
			EnvironmentID:     alloc.EnvironmentID,
			ServiceID:         cond.GetServiceId(),
			AllocationID:      cond.GetAllocationId(),
			AgentID:           agentID,
			Stream:            "combined",
			LogType:           logs.LogTypeDeploy,
			Stage:             "restart",
			RolloutGeneration: cond.GetDesiredRolloutGeneration(),
			Sequence:          logs.NextSequence(),
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

// pullCredentialCacheTTL reuses minted pull credentials across syncs so the
// credentials version stays stable. It is well within the 48h pull TTL.
const pullCredentialCacheTTL = time.Hour

func (s *AgentService) credTime() time.Time {
	if s != nil && s.credNow != nil {
		return s.credNow().UTC()
	}
	return time.Now().UTC()
}

// pullCredentialsForAgent mints (with cache) the independently versioned pull
// credentials for this agent's current desired services. Only platform images
// receive entries; external images need no credentials.
func (s *AgentService) pullCredentialsForAgent(ctx context.Context, agentID string, state *agentv1.DesiredNodeState) (*agentv1.PullCredentialSet, error) {
	out := &agentv1.PullCredentialSet{AgentId: agentID}
	if s == nil || s.registry == nil || !s.registry.Enabled() || state == nil {
		out.CredentialsVersion = deliverycore.HashCredentials(nil)
		return out, nil
	}
	now := s.credTime()
	current := make(map[string]string, len(state.GetServices()))
	for _, svc := range state.GetServices() {
		current[svc.GetAllocationId()] = svc.GetSpec().GetImage()
	}
	s.credMu.Lock()
	if s.credCache == nil {
		s.credCache = make(map[string]cachedPullCredential)
	}
	// Prune removed allocations for this agent and bound total size.
	for key, entry := range s.credCache {
		agent, alloc, _, ok := splitCredCacheKey(key)
		if !ok {
			delete(s.credCache, key)
			continue
		}
		if agent != agentID {
			continue
		}
		if _, ok := current[alloc]; !ok {
			delete(s.credCache, key)
			continue
		}
		_ = entry
	}
	if len(s.credCache) > 10000 {
		s.credCache = make(map[string]cachedPullCredential)
	}
	s.credMu.Unlock()

	for _, svc := range state.GetServices() {
		image := svc.GetSpec().GetImage()
		if image == "" {
			continue
		}
		key := credCacheKey(agentID, svc.GetAllocationId(), image)
		s.credMu.Lock()
		cached, ok := s.credCache[key]
		s.credMu.Unlock()
		// Reuse a minted credential only while it stays valid through the
		// next session. Sessions rotate at least every client-certificate
		// lifetime and refresh credentials there; a token that would expire
		// before then must be re-minted now, or a private-image restart
		// during the renewed session fails to pull with the stale token.
		if ok && cached.image == image && now.Sub(cached.mintedAt) < pullCredentialCacheTTL && cached.expiresAt.After(now.Add(s.credReuseHorizon)) {
			if cached.username != "" || cached.password != "" {
				out.Credentials = append(out.Credentials, &agentv1.AllocationCredential{
					AllocationId: svc.GetAllocationId(), Username: cached.username, Password: cached.password,
				})
			}
			continue
		}
		username, password, err := s.registry.CredentialsForPull(ctx,
			agentID+"-"+svc.GetAllocationId(), svc.GetEnvironmentId(), svc.GetServiceId(), image)
		if err != nil {
			return nil, fmt.Errorf("mint pull credential for service %s: %w", svc.GetServiceId(), err)
		}
		s.credMu.Lock()
		s.credCache[key] = cachedPullCredential{username: username, password: password, mintedAt: now, expiresAt: now.Add(s.registry.PullCredentialLifetime()), image: image}
		s.credMu.Unlock()
		if username != "" || password != "" {
			out.Credentials = append(out.Credentials, &agentv1.AllocationCredential{
				AllocationId: svc.GetAllocationId(), Username: username, Password: password,
			})
		}
	}
	out.CredentialsVersion = deliverycore.HashCredentials(out.GetCredentials())
	return out, nil
}

func credCacheKey(agentID, allocationID, image string) string {
	return agentID + "\x00" + allocationID + "\x00" + image
}

func splitCredCacheKey(key string) (agentID, allocationID, image string, ok bool) {
	parts := strings.SplitN(key, "\x00", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}
