package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/health"
	"ebof-wg-mesh/internal/mesh"

	"github.com/google/uuid"
	"google.golang.org/grpc/credentials"
)

type App struct {
	cfg              config.AgentConfig
	runtime          Runtime
	mesh             MeshHandle
	meshFactory      MeshFactory
	meshAssignment   mesh.Assignment
	stateStore       *localStateStore
	supervisor       *workloadSupervisor
	healthStop       func(context.Context) error
	controlPlaneAddr string
	deadOwners       map[string]time.Time
}

const (
	initialReconnectDelay   = time.Second
	maxReconnectDelay       = 30 * time.Second
	reconcileSafetyInterval = time.Minute
	credentialCheckInterval = time.Minute
)

var (
	errRotateSession = errors.New("rotate mTLS session")
	errRedirectOwner = errors.New("redirect to live owner")
)

func New(cfg config.AgentConfig, opts ...Option) (*App, error) {
	options := defaultOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}

	runtime, err := options.runtimeFactory(cfg)
	if err != nil {
		return nil, err
	}
	return &App{
		cfg:         cfg,
		runtime:     runtime,
		meshFactory: options.meshFactory,
	}, nil
}

func (a *App) Close() error {
	if a.healthStop != nil {
		_ = a.healthStop(context.Background())
	}
	if a.runtime != nil {
		_ = a.runtime.Close()
	}
	if a.mesh != nil {
		_ = a.mesh.Close()
	}
	if a.stateStore != nil {
		_ = a.stateStore.Close()
	}
	return nil
}

func (a *App) Run(ctx context.Context) error {
	if listen := strings.TrimSpace(a.cfg.Health.Listen); listen != "" {
		_, shutdown, err := health.ListenAndServe(ctx, listen, a.readyReport)
		if err != nil {
			return fmt.Errorf("listen health: %w", err)
		}
		a.healthStop = shutdown
	}
	clusterID, err := a.persistedClusterIdentity()
	if err != nil {
		return err
	}
	store, err := openLocalStateStore(a.cfg.Runtime.DataDir, a.cfg.Node.ID)
	if err != nil {
		return fmt.Errorf("open local state: %w", err)
	}
	a.stateStore = store
	if err := store.setReplicaSeeds(a.cfg.ControlPlane.Addresses); err != nil {
		_ = store.Close()
		a.stateStore = nil
		return fmt.Errorf("persist control-plane discovery seeds: %w", err)
	}
	a.supervisor = newWorkloadSupervisor(a.cfg.Node.ID, a.runtime, store, a.applyNodeConfig)
	if err := a.supervisor.Start(ctx, clusterID); err != nil {
		_ = store.Close()
		a.stateStore = nil
		return fmt.Errorf("start workload supervision: %w", err)
	}
	return a.runConnections(ctx)
}

func (a *App) runConnections(ctx context.Context) error {
	delay := initialReconnectDelay
	for {
		err := a.runSession(ctx)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, errRotateSession):
			slog.Info("rotating agent mTLS session", "agent_id", a.cfg.Node.ID)
			delay = initialReconnectDelay
			continue
		case errors.Is(err, errRedirectOwner):
			slog.Info("following live owner redirect", "agent_id", a.cfg.Node.ID, "address", a.controlPlaneAddr)
			delay = initialReconnectDelay
			continue
		case ctx.Err() != nil:
			return nil
		default:
			slog.Warn("agent session ended; reconnecting", "agent_id", a.cfg.Node.ID, "error", err, "retry_in", delay)
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}

		if delay < maxReconnectDelay {
			delay *= 2
			if delay > maxReconnectDelay {
				delay = maxReconnectDelay
			}
		}
	}
}

func (a *App) readyReport(context.Context) health.Report {
	if a.supervisor != nil && a.supervisor.Ready() {
		return health.Report{Status: health.StatusReady}
	}
	return health.Report{Status: health.StatusNotReady, Failed: []string{"local_state_recovery"}}
}

func (a *App) runSession(ctx context.Context) error {
	slog.Info("starting agent session", "agent_id", a.cfg.Node.ID)
	creds, certNotAfter, clusterID, err := a.clientCredentials(ctx)
	if err != nil {
		return fmt.Errorf("build control plane credentials: %w", err)
	}
	addresses, err := a.controlPlaneCandidates()
	if err != nil {
		return fmt.Errorf("load control-plane discovery set: %w", err)
	}
	if len(addresses) == 0 {
		return errors.New("control-plane discovery set is empty")
	}
	var failures []error
	for _, address := range addresses {
		err := a.runSessionAt(ctx, creds, certNotAfter, clusterID, address)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		previousPin := a.controlPlaneAddr
		err = a.replicaAttemptError(address, err)
		if errors.Is(err, errRedirectOwner) || errors.Is(err, errRotateSession) {
			return err
		}
		if !shouldWalkNextReplica(err) {
			return err
		}
		if a.controlPlaneAddr == "" && strings.TrimSpace(previousPin) != "" {
			slog.Info("forgetting unavailable control-plane pin", "agent_id", a.cfg.Node.ID, "address", address)
		}
		failures = append(failures, err)
	}
	return fmt.Errorf("no reachable control-plane replica: %w", errors.Join(failures...))
}

func (a *App) runSessionAt(ctx context.Context, creds credentials.TransportCredentials, certNotAfter time.Time, clusterID, addr string) error {
	sessionCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if renewAt := certNotAfter.Add(-time.Duration(a.cfg.ControlPlane.TLS.RenewBeforeMinutes) * time.Minute); !renewAt.IsZero() {
		go a.rotateSessionAt(sessionCtx, cancel, renewAt)
	}
	conn, err := dialControlPlane(sessionCtx, addr, creds)
	if err != nil {
		if cause := handshakeCause(sessionCtx); cause != nil {
			return cause
		}
		return fmt.Errorf("dial control plane: %w", err)
	}
	defer conn.Close()
	a.controlPlaneAddr = addr
	slog.Info("dialed control plane", "agent_id", a.cfg.Node.ID, "address", addr)

	client := agentv1.NewAgentControlClient(conn)
	sessionID := uuid.NewString()
	incarnation, err := a.stateStore.nextSessionIncarnation()
	if err != nil {
		return err
	}
	stream, err := client.Sync(sessionCtx)
	if err != nil {
		if cause := handshakeCause(sessionCtx); cause != nil {
			return cause
		}
		return fmt.Errorf("open sync stream: %w", err)
	}
	slog.Info("opened sync stream", "agent_id", a.cfg.Node.ID)
	publicKey, err := mesh.PublicKey(a.cfg.Mesh.WireGuard.PrivateKey)
	if err != nil {
		return fmt.Errorf("derive wireguard public key: %w", err)
	}
	var sendMu sync.Mutex
	send := func(msg *agentv1.AgentClientMessage) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(msg)
	}
	summary, err := a.supervisor.Summary()
	if err != nil {
		return fmt.Errorf("read local inventory: %w", err)
	}
	if err := a.stateStore.requireClusterIdentity(clusterID); err != nil {
		return err
	}
	runtimeResources := make([]*agentv1.RuntimeResource, 0, len(summary.RuntimeResources))
	for _, resource := range summary.RuntimeResources {
		runtimeResources = append(runtimeResources, &agentv1.RuntimeResource{AllocationId: resource.AllocationID, VolumeId: resource.VolumeID, RuntimeId: resource.RuntimeID})
	}
	handshakeExpire := time.AfterFunc(replicaRPCTimeout, func() {
		cancel(errHandshakeTimeout)
	})
	defer handshakeExpire.Stop()
	if err := send(&agentv1.AgentClientMessage{
		Payload: &agentv1.AgentClientMessage_Hello{Hello: &agentv1.AgentHello{
			AgentId:                 a.cfg.Node.ID,
			Name:                    a.cfg.Node.Name,
			AdvertiseAddr:           a.cfg.Node.AdvertiseAddr,
			CpuMillisCapacity:       a.cfg.Node.Resources.CPUMillis,
			MemoryMebibytesCapacity: a.cfg.Node.Resources.MemoryMebibytes,
			WireguardPublicKey:      publicKey,
			WireguardListenPort:     int32(a.cfg.Mesh.WireGuard.ListenPort),
			RuntimeCapabilities:     []string{"containerd", "wireguard", "ebpf-policy"},
			SoftwareVersion:         Version,
			SessionId:               sessionID,
			SessionIncarnation:      incarnation,
			ClusterId:               clusterID,
			LocalStoreId:            summary.LocalStoreID,
			InitializationState:     string(summary.Initialization),
			Allocations:             summary.Allocations,
			AcceptedAuthorityEpoch:  summary.AuthorityEpoch,
			ReconciliationCursor:    summary.ReconciliationCursor,
			RecoveryMode:            summary.Initialization == initializationRecovery,
			RuntimeResources:        runtimeResources,
		}},
	}); err != nil {
		if cause := handshakeCause(sessionCtx); cause != nil {
			return cause
		}
		return err
	}
	slog.Info("sent agent hello", "agent_id", a.cfg.Node.ID)
	if runtimeWithLogs, ok := a.runtime.(logSinkRuntime); ok {
		logSink := newStreamLogSink(sessionCtx, a.cfg.Node.ID, send)
		runtimeWithLogs.SetLogSink(logSink)
		defer func() {
			runtimeWithLogs.SetLogSink(nil)
			logSink.Close()
		}()
	}
	go a.heartbeatLoop(sessionCtx, sessionID, send)
	credentialTicker := time.NewTicker(credentialCheckInterval)
	defer credentialTicker.Stop()
	type receiveResult struct {
		message *agentv1.AgentServerMessage
		err     error
	}
	received := make(chan receiveResult, 1)
	go func() {
		for {
			message, err := stream.Recv()
			select {
			case received <- receiveResult{message: message, err: err}:
			case <-sessionCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var lastSentSequence uint64
	authorityConfirmed := false
	handshake := true
	sendCurrentReport := func() error {
		report, err := a.supervisor.CurrentReport()
		if err != nil || report == nil {
			return err
		}
		if report.GetObservationSequence() <= lastSentSequence {
			return nil
		}
		report.SessionId = sessionID
		if err := send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_StatusReport{StatusReport: report}}); err != nil {
			return err
		}
		lastSentSequence = report.GetObservationSequence()
		return nil
	}
	for {
		select {
		case <-sessionCtx.Done():
			if cause := handshakeCause(sessionCtx); cause != nil {
				return cause
			}
			return nil
		case <-a.supervisor.ReportNotifications():
			if handshake || !authorityConfirmed {
				continue
			}
			if err := sendCurrentReport(); err != nil {
				return err
			}
		case <-credentialTicker.C:
			if handshake {
				continue
			}
			state, err := a.stateStore.desiredState()
			if err != nil {
				return fmt.Errorf("load desired state for credential renewal: %w", err)
			}
			if state == nil {
				continue
			}
			if err := a.refreshManagedDashboardIdentity(sessionCtx, client, state); err != nil {
				if _, ok := liveOwnerAddr(err); ok {
					return a.followLiveOwner(err)
				}
				slog.Warn("refresh managed dashboard identity", "agent_id", a.cfg.Node.ID, "error", err)
			}
		case result := <-received:
			if handshake {
				handshake = false
				handshakeExpire.Stop()
			}
			if result.err != nil {
				if cause := handshakeCause(sessionCtx); cause != nil {
					return cause
				}
				if errors.Is(result.err, context.Canceled) || ctx.Err() != nil {
					return nil
				}
				return result.err
			}
			if result.message == nil {
				continue
			}
			state := result.message.GetDesiredState()
			if state == nil {
				continue
			}
			if err := a.stateStore.setReplicaAddresses(state.GetReplicaAddresses()); err != nil {
				return fmt.Errorf("persist control-plane replica addresses: %w", err)
			}
			changed, err := a.supervisor.AcceptDesired(clusterID, sessionID, state)
			if err != nil {
				return fmt.Errorf("accept desired state: %w", err)
			}
			if err := send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_Acknowledgement{
				Acknowledgement: &agentv1.DesiredStateAcknowledgement{AgentId: a.cfg.Node.ID, SessionId: sessionID,
					AuthorityEpoch: state.GetAuthorityEpoch(), ReconciliationCursor: state.GetReconciliationCursor()},
			}}); err != nil {
				return err
			}
			if err := a.refreshManagedDashboardIdentity(sessionCtx, client, state); err != nil {
				a.supervisor.ReconcileAcceptedDesired()
				return err
			}
			a.supervisor.ReconcileAcceptedDesired()
			authorityConfirmed = true
			slog.Info("accepted desired state", "agent_id", a.cfg.Node.ID, "authority_epoch", state.GetAuthorityEpoch(), "cursor", state.GetReconciliationCursor(), "services", len(state.GetServices()), "volumes", len(state.GetVolumes()))
			if !changed {
				if err := sendCurrentReport(); err != nil {
					return err
				}
			}
		}
	}
}

func (a *App) refreshManagedDashboardIdentity(ctx context.Context, issuer dashboardCertificateIssuer, state *agentv1.DesiredNodeState) error {
	changed, err := a.ensureManagedDashboardIdentity(ctx, issuer, state)
	if err != nil {
		return fmt.Errorf("ensure managed dashboard identity: %w", err)
	}
	if changed {
		if err := a.supervisor.RestartManagedDashboard(ctx, state); err != nil {
			return err
		}
	}
	return nil
}

func runtimeEventReconcileLoop(
	ctx context.Context,
	events <-chan struct{},
	errs <-chan error,
	reconcile func(),
	reportError func(error),
) {
	for events != nil || errs != nil {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if reconcile != nil {
				reconcile()
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil && reportError != nil {
				reportError(err)
			}
		}
	}
}

func (a *App) rotateSessionAt(ctx context.Context, cancel context.CancelCauseFunc, renewAt time.Time) {
	delay := time.Until(renewAt)
	if delay <= 0 {
		cancel(errRotateSession)
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
		cancel(errRotateSession)
	}
}

func (a *App) heartbeatLoop(ctx context.Context, sessionID string, send func(*agentv1.AgentClientMessage) error) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = send(&agentv1.AgentClientMessage{
				Payload: &agentv1.AgentClientMessage_Heartbeat{Heartbeat: &agentv1.AgentHeartbeat{
					AgentId: a.cfg.Node.ID, SessionId: sessionID, RecoveryMode: a.supervisor == nil || !a.supervisor.Ready(),
				}},
			})
		}
	}
}

func (a *App) applyNodeConfig(ctx context.Context, assigned *agentv1.AssignedNodeConfig) error {
	if assigned == nil {
		return errors.New("assigned node config missing")
	}
	next := mesh.AssignmentFrom(assigned)
	if reflect.DeepEqual(a.meshAssignment, next) {
		return nil
	}
	if a.mesh != nil {
		if err := a.mesh.Update(mesh.RuntimeConfig(a.cfg, next)); err != nil {
			return err
		}
		a.meshAssignment = next
		return nil
	}
	meshRuntime, err := a.meshFactory(ctx, mesh.RuntimeConfig(a.cfg, next))
	if err != nil {
		return err
	}
	a.mesh = meshRuntime
	a.meshAssignment = next
	return nil
}
