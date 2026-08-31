package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/health"
	"ebof-wg-mesh/internal/mesh"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type App struct {
	cfg            config.AgentConfig
	runtime        Runtime
	mesh           MeshHandle
	meshFactory    MeshFactory
	meshAssignment mesh.Assignment
	ready          atomic.Bool
	healthStop     func(context.Context) error
}

const (
	initialReconnectDelay   = time.Second
	maxReconnectDelay       = 30 * time.Second
	reconcileSafetyInterval = time.Minute
)

var errRotateSession = errors.New("rotate mTLS session")

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
	if a.ready.Load() {
		return health.Report{Status: health.StatusReady}
	}
	return health.Report{Status: health.StatusNotReady, Failed: []string{"control_plane"}}
}

func (a *App) runSession(ctx context.Context) error {
	defer a.ready.Store(false)
	slog.Info("starting agent session", "agent_id", a.cfg.Node.ID)
	creds, certNotAfter, err := a.clientCredentials(ctx)
	if err != nil {
		return fmt.Errorf("build control plane credentials: %w", err)
	}

	sessionCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if renewAt := certNotAfter.Add(-time.Duration(a.cfg.ControlPlane.TLS.RenewBeforeMinutes) * time.Minute); !renewAt.IsZero() {
		go a.rotateSessionAt(sessionCtx, cancel, renewAt)
	}
	conn, err := grpc.NewClient(a.cfg.ControlPlane.Address, grpc.WithTransportCredentials(creds))
	if err != nil {
		return fmt.Errorf("dial control plane: %w", err)
	}
	defer conn.Close()
	slog.Info("dialed control plane", "agent_id", a.cfg.Node.ID, "address", a.cfg.ControlPlane.Address)

	client := agentv1.NewAgentControlClient(conn)
	stream, err := client.Sync(sessionCtx)
	if err != nil {
		a.ready.Store(false)
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
	if err := send(&agentv1.AgentClientMessage{
		Payload: &agentv1.AgentClientMessage_Hello{Hello: &agentv1.AgentHello{
			AgentId:                 a.cfg.Node.ID,
			Name:                    a.cfg.Node.Name,
			AdvertiseAddr:           a.cfg.Node.AdvertiseAddr,
			CpuMillisCapacity:       a.cfg.Node.Resources.AdvertisedCPUMillis(),
			MemoryMebibytesCapacity: a.cfg.Node.Resources.AdvertisedMemoryMebibytes(),
			WireguardPublicKey:      publicKey,
			WireguardListenPort:     int32(a.cfg.Mesh.WireGuard.ListenPort),
			RuntimeCapabilities:     []string{"containerd", "wireguard", "ebpf-policy"},
			SoftwareVersion:         Version,
		}},
	}); err != nil {
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
	var (
		desiredStateMu   sync.RWMutex
		latestDesired    *agentv1.DesiredNodeState
		reconcileStateMu sync.Mutex
		lastReport       *agentv1.StatusReport
	)
	reconcileAndReport := func(state *agentv1.DesiredNodeState) error {
		reconcileStateMu.Lock()
		defer reconcileStateMu.Unlock()
		identityChanged, reconcileErr := a.ensureManagedDashboardIdentity(sessionCtx, client, state)
		if reconcileErr != nil {
			reconcileErr = fmt.Errorf("ensure managed dashboard identity: %w", reconcileErr)
		}
		if reconcileErr == nil && identityChanged {
			restarter, ok := a.runtime.(managedDashboardRestartRuntime)
			if !ok {
				reconcileErr = errors.New("runtime cannot restart the managed dashboard after certificate renewal")
			} else if err := restarter.RestartManagedDashboard(sessionCtx, state); err != nil {
				reconcileErr = err
			}
		}
		var report *agentv1.StatusReport
		if reconcileErr == nil {
			report, reconcileErr = a.runtime.Reconcile(sessionCtx, state)
		}
		if reconcileErr != nil {
			slog.Error("reconcile failed", "agent_id", a.cfg.Node.ID, "revision", state.GetRevision(), "error", reconcileErr)
			report = &agentv1.StatusReport{
				AgentId: a.cfg.Node.ID,
				Services: []*agentv1.ServiceCondition{{
					Phase:   "Error",
					Message: reconcileErr.Error(),
				}},
			}
		}
		if !statusReportChanged(lastReport, report) {
			return nil
		}
		if err := send(&agentv1.AgentClientMessage{
			Payload: &agentv1.AgentClientMessage_StatusReport{StatusReport: report},
		}); err != nil {
			return err
		}
		lastReport = proto.Clone(report).(*agentv1.StatusReport)
		return nil
	}
	go a.heartbeatLoop(sessionCtx, send)
	latestDesiredState := func() *agentv1.DesiredNodeState {
		desiredStateMu.RLock()
		defer desiredStateMu.RUnlock()
		if latestDesired == nil {
			return nil
		}
		return proto.Clone(latestDesired).(*agentv1.DesiredNodeState)
	}
	reconcileLatest := func(source string) {
		state := latestDesiredState()
		if state == nil {
			return
		}
		if err := reconcileAndReport(state); err != nil && sessionCtx.Err() == nil {
			slog.Warn("runtime reconciliation failed", "agent_id", a.cfg.Node.ID, "revision", state.GetRevision(), "source", source, "error", err)
		}
	}
	go periodicReconcileLoop(sessionCtx, reconcileSafetyInterval, latestDesiredState, func(*agentv1.DesiredNodeState) {
		reconcileLatest("safety-resync")
	})
	if source, ok := a.runtime.(RuntimeEventSource); ok {
		events, eventErrors := source.ReconcileEvents(sessionCtx)
		go runtimeEventReconcileLoop(sessionCtx, events, eventErrors, func() {
			reconcileLatest("runtime-event")
		}, func(err error) {
			slog.Warn("runtime event watch ended", "agent_id", a.cfg.Node.ID, "error", err)
		})
	}

	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(context.Cause(sessionCtx), errRotateSession) {
				return errRotateSession
			}
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		state := msg.GetDesiredState()
		if state == nil {
			continue
		}
		if err := a.applyNodeConfig(sessionCtx, state.GetNodeConfig()); err != nil {
			return fmt.Errorf("apply assigned node config: %w", err)
		}
		desiredStateMu.Lock()
		workloadsChanged := !desiredWorkloadsEqual(latestDesired, state)
		latestDesired = proto.Clone(state).(*agentv1.DesiredNodeState)
		desiredStateMu.Unlock()
		a.ready.Store(true)
		slog.Info("received desired state", "agent_id", a.cfg.Node.ID, "revision", state.GetRevision(), "services", len(state.GetServices()), "volumes", len(state.GetVolumes()))
		if !workloadsChanged {
			continue
		}
		if err := reconcileAndReport(state); err != nil {
			return err
		}
	}
}

func desiredWorkloadsEqual(previous, next *agentv1.DesiredNodeState) bool {
	if previous == nil || next == nil {
		return previous == next
	}
	return proto.Equal(
		&agentv1.DesiredNodeState{Services: previous.GetServices(), Volumes: previous.GetVolumes()},
		&agentv1.DesiredNodeState{Services: next.GetServices(), Volumes: next.GetVolumes()},
	)
}

func statusReportChanged(previous, next *agentv1.StatusReport) bool {
	return !proto.Equal(previous, next)
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

func periodicReconcileLoop(
	ctx context.Context,
	interval time.Duration,
	desiredState func() *agentv1.DesiredNodeState,
	reconcile func(*agentv1.DesiredNodeState),
) {
	if interval <= 0 || desiredState == nil || reconcile == nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			state := desiredState()
			if state == nil {
				continue
			}
			reconcile(state)
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

func (a *App) heartbeatLoop(ctx context.Context, send func(*agentv1.AgentClientMessage) error) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = send(&agentv1.AgentClientMessage{
				Payload: &agentv1.AgentClientMessage_Heartbeat{Heartbeat: &agentv1.AgentHeartbeat{AgentId: a.cfg.Node.ID}},
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
