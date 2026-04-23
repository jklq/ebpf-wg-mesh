package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type App struct {
	cfg            config.AgentConfig
	runtime        Runtime
	mesh           MeshHandle
	meshFactory    MeshFactory
	meshAssignment config.AgentMeshAssignment
}

const (
	initialReconnectDelay = time.Second
	maxReconnectDelay     = 30 * time.Second
	reconcilePollInterval = 2 * time.Second
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
	if a.runtime != nil {
		_ = a.runtime.Close()
	}
	if a.mesh != nil {
		_ = a.mesh.Close()
	}
	return nil
}

func (a *App) Run(ctx context.Context) error {
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

func (a *App) runSession(ctx context.Context) error {
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
		return fmt.Errorf("open sync stream: %w", err)
	}
	slog.Info("opened sync stream", "agent_id", a.cfg.Node.ID)
	publicKey, err := a.wireGuardPublicKey()
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
			CpuMillisCapacity:       a.cfg.Node.Resources.CPUMillis,
			MemoryMebibytesCapacity: a.cfg.Node.Resources.MemoryMebibytes,
			WireguardPublicKey:      publicKey,
			WireguardListenPort:     int32(a.cfg.Mesh.WireGuard.ListenPort),
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
	)
	reconcileAndReport := func(state *agentv1.DesiredNodeState) error {
		reconcileStateMu.Lock()
		defer reconcileStateMu.Unlock()
		report, err := a.runtime.Reconcile(sessionCtx, state)
		if err != nil {
			slog.Error("reconcile failed", "agent_id", a.cfg.Node.ID, "revision", state.GetRevision(), "error", err)
			report = &agentv1.StatusReport{
				AgentId: a.cfg.Node.ID,
				Services: []*agentv1.ServiceCondition{{
					Phase:   "Error",
					Message: err.Error(),
				}},
			}
		}
		if err := send(&agentv1.AgentClientMessage{
			Payload: &agentv1.AgentClientMessage_StatusReport{StatusReport: report},
		}); err != nil {
			return err
		}
		return nil
	}
	go a.heartbeatLoop(sessionCtx, send)
	go periodicReconcileLoop(sessionCtx, reconcilePollInterval, func() *agentv1.DesiredNodeState {
		desiredStateMu.RLock()
		defer desiredStateMu.RUnlock()
		if latestDesired == nil {
			return nil
		}
		return proto.Clone(latestDesired).(*agentv1.DesiredNodeState)
	}, func(state *agentv1.DesiredNodeState) {
		if err := reconcileAndReport(state); err != nil && sessionCtx.Err() == nil {
			slog.Warn("periodic status report failed", "agent_id", a.cfg.Node.ID, "revision", state.GetRevision(), "error", err)
		}
	})

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
		latestDesired = proto.Clone(state).(*agentv1.DesiredNodeState)
		desiredStateMu.Unlock()
		slog.Info("received desired state", "agent_id", a.cfg.Node.ID, "revision", state.GetRevision(), "services", len(state.GetServices()), "volumes", len(state.GetVolumes()))
		if err := reconcileAndReport(state); err != nil {
			return err
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

func (a *App) wireGuardPublicKey() (string, error) {
	privateKey, err := wgtypes.ParseKey(a.cfg.Mesh.WireGuard.PrivateKey)
	if err != nil {
		return "", err
	}
	return privateKey.PublicKey().String(), nil
}

func (a *App) applyNodeConfig(ctx context.Context, assigned *agentv1.AssignedNodeConfig) error {
	if assigned == nil {
		return errors.New("assigned node config missing")
	}
	next := config.AgentMeshAssignment{
		WorkloadIPv6Subnet: assigned.GetWorkloadIpv6Subnet(),
		WireGuardAddresses: append([]string(nil), assigned.GetWireguardAddresses()...),
	}
	for _, peer := range assigned.GetPeers() {
		next.Peers = append(next.Peers, config.PeerConfig{
			Name:                 peer.GetName(),
			PublicKey:            peer.GetPublicKey(),
			Endpoint:             peer.GetEndpoint(),
			AllowedIPs:           append([]string(nil), peer.GetAllowedIps()...),
			PersistentKeepaliveS: int(peer.GetPersistentKeepaliveSeconds()),
		})
	}
	if reflect.DeepEqual(a.meshAssignment, next) {
		return nil
	}
	if a.mesh != nil {
		_ = a.mesh.Close()
		a.mesh = nil
	}
	meshRuntime, err := a.meshFactory(ctx, a.cfg.MeshRuntimeConfig(next))
	if err != nil {
		return err
	}
	a.mesh = meshRuntime
	a.meshAssignment = next
	return nil
}
