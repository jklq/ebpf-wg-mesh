package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/health"
	"ebof-wg-mesh/internal/mesh"

	"github.com/google/uuid"
	"google.golang.org/grpc/credentials"
)

type App struct {
	cfg                  config.AgentConfig
	runtime              Runtime
	mesh                 MeshHandle
	meshFactory          MeshFactory
	meshAssignment       mesh.Assignment
	stateStore           *localStateStore
	supervisor           *workloadSupervisor
	logShipper           *logShipper
	healthStop           func(context.Context) error
	controlPlaneAddr     string
	deadOwners           map[string]time.Time
	sessionEstablishedAt time.Time
}

const (
	// logBatchAckTimeout bounds how long one batch waits for the
	// server's acceptance ack before the send fails and the batch
	// retries (deduplicated server-side).
	logBatchAckTimeout    = 30 * time.Second
	initialReconnectDelay = time.Second
	maxReconnectDelay     = 30 * time.Second
	// stableSessionDuration is how long a session must stay connected for a
	// subsequent disconnect to reset the reconnect backoff. Without it a single
	// transient outage leaves the agent waiting the accumulated maximum long
	// after the control plane recovered, because a long-lived session never
	// resets the delay.
	stableSessionDuration   = time.Minute
	reconcileSafetyInterval = time.Minute
	credentialCheckInterval = time.Minute
	diskEnforcementInterval = 15 * time.Second
	// reportRepublishQuietPeriod defers report publication to stream-quiet
	// points; eager publication can emit a report predating the batch.
	reportRepublishQuietPeriod = 250 * time.Millisecond
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
	if a.logShipper != nil {
		_ = a.logShipper.Close()
		a.logShipper = nil
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
	// The pinned identity adopted at enrollment, empty before the first
	// enrollment. It survives CA rotations; the bundle on disk does not.
	a.supervisor = newWorkloadSupervisor(a.cfg.Node.ID, a.runtime, store, a.applyNodeConfig)
	// Install the log sink before supervision restores workloads:
	// containers started from stored desired state stream output
	// immediately, and a nil sink would discard their boot logs.
	shipper, err := newLogShipper(a.cfg.Node.ID, logShipConfigFromAgent(a.cfg))
	if err != nil {
		_ = store.Close()
		a.stateStore = nil
		return fmt.Errorf("open log spool: %w", err)
	}
	a.logShipper = shipper
	if runtimeWithLogs, ok := a.runtime.(logSinkRuntime); ok {
		runtimeWithLogs.SetLogSink(shipper)
	}
	if err := a.supervisor.Start(ctx, store.clusterIdentity()); err != nil {
		_ = store.Close()
		a.stateStore = nil
		return fmt.Errorf("start workload supervision: %w", err)
	}
	go func() {
		_ = shipper.Run(ctx)
	}()
	return a.runConnections(ctx)
}

func logShipConfigFromAgent(cfg config.AgentConfig) logShipConfig {
	ship := cfg.Logs
	burst := ship.Burst
	if burst <= 0 {
		burst = 1000
	}
	return logShipConfig{
		SpoolDir:      filepath.Join(cfg.Runtime.DataDir, "log-spool"),
		SpoolMaxBytes: ship.SpoolMaxBytes,
		// A non-positive rate disables producer limiting; zero is a
		// deliberate operator choice, not an unset default.
		RatePerSec:    float64(ship.RatePerSec),
		Burst:         burst,
		BatchSize:     ship.FlushBatchSize,
		FlushInterval: time.Duration(ship.FlushIntervalSeconds) * time.Second,
		ReplayWindow:  time.Duration(ship.ReplayWindowSeconds) * time.Second,
	}
}

func (a *App) runConnections(ctx context.Context) error {
	delay := initialReconnectDelay
	for {
		err := a.runSession(ctx)
		stable := !a.sessionEstablishedAt.IsZero() && time.Since(a.sessionEstablishedAt) >= stableSessionDuration
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
			if stable {
				delay = initialReconnectDelay
			}
			slog.Warn("agent session ended; reconnecting", "agent_id", a.cfg.Node.ID, "error", err, "retry_in", delay)
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}

		delay = nextReconnectDelay(delay)
	}
}

// nextReconnectDelay advances the reconnect backoff after a failed session.
// A session that stayed connected for at least stableSessionDuration is
// treated as a healthy connection and resets the backoff; otherwise the delay
// doubles up to maxReconnectDelay so a persistently unavailable control plane
// is not hammered.
func nextReconnectDelay(current time.Duration) time.Duration {
	next := current * 2
	if next > maxReconnectDelay {
		return maxReconnectDelay
	}
	return next
}

func (a *App) readyReport(context.Context) health.Report {
	if a.supervisor != nil && a.supervisor.Ready() {
		return health.Report{Status: health.StatusReady}
	}
	return health.Report{Status: health.StatusNotReady, Failed: []string{"local_state_recovery"}}
}

func (a *App) runSession(ctx context.Context) error {
	a.sessionEstablishedAt = time.Time{}
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

// cumulativeAck acknowledges the whole accepted position. The authority epoch
// is the session's confirmed epoch, not the store's: independent streams ack
// before the first checkpoint or diff advances the accepted epoch.
func cumulativeAck(agentID, sessionID string, summary localStateSummary, confirmedEpoch uint64) *agentv1.DesiredStateAcknowledgement {
	return &agentv1.DesiredStateAcknowledgement{
		AgentId: agentID, SessionId: sessionID,
		AuthorityEpoch: confirmedEpoch, ReconciliationCursor: summary.ReconciliationCursor,
		NodeConfigVersion: summary.NodeConfigVersion, CredentialsVersion: summary.CredentialsVersion, ReplicasVersion: summary.ReplicasVersion,
	}
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
	// Log batches are acknowledged per batch: sendLogs returns only
	// once the server accepted the batch into its durable ingest path,
	// so the shipper's spool commit can never outrun acceptance.
	var batchSeq atomic.Uint64
	var ackMu sync.Mutex
	ackWaiters := make(map[uint64]chan struct{})
	completeAck := func(id uint64) {
		ackMu.Lock()
		done, ok := ackWaiters[id]
		delete(ackWaiters, id)
		ackMu.Unlock()
		if ok {
			close(done)
		}
	}
	sendLogs := func(msg *agentv1.AgentClientMessage) error {
		batch := msg.GetLogBatch()
		if batch == nil {
			return send(msg)
		}
		id := batchSeq.Add(1)
		batch.BatchId = id
		done := make(chan struct{})
		ackMu.Lock()
		ackWaiters[id] = done
		ackMu.Unlock()
		defer func() {
			ackMu.Lock()
			delete(ackWaiters, id)
			ackMu.Unlock()
		}()
		if err := send(msg); err != nil {
			return err
		}
		select {
		case <-done:
			return nil
		case <-time.After(logBatchAckTimeout):
			return errors.New("log batch acceptance timed out")
		case <-sessionCtx.Done():
			return sessionCtx.Err()
		}
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
	// An unchanged reconnect sends no server messages: after the handshake
	// window, confirm authority optimistically and publish the observation.
	handshakeTimer := time.NewTimer(replicaRPCTimeout)
	defer handshakeTimer.Stop()
	if err := send(&agentv1.AgentClientMessage{
		Payload: &agentv1.AgentClientMessage_Hello{Hello: &agentv1.AgentHello{
			AgentId:                           a.cfg.Node.ID,
			Name:                              a.cfg.Node.Name,
			AdvertiseAddr:                     a.cfg.Node.AdvertiseAddr,
			CpuMillisCapacity:                 a.cfg.Node.Resources.AdvertisedCPUMillis(),
			MemoryMebibytesCapacity:           a.cfg.Node.Resources.AdvertisedMemoryMebibytes(),
			WireguardPublicKey:                publicKey,
			WireguardListenPort:               int32(a.cfg.Mesh.WireGuard.ListenPort),
			WireguardEndpoint:                 a.cfg.Mesh.WireGuard.AdvertiseEndpoint,
			RuntimeCapabilities:               []string{"containerd", "wireguard", "ebpf-policy"},
			SoftwareVersion:                   Version,
			SessionId:                         sessionID,
			SessionIncarnation:                incarnation,
			ClusterId:                         clusterID,
			LocalStoreId:                      summary.LocalStoreID,
			InitializationState:               string(summary.Initialization),
			Allocations:                       summary.Allocations,
			AcceptedAuthorityEpoch:            summary.AuthorityEpoch,
			ReconciliationCursor:              summary.ReconciliationCursor,
			AcceptedNodeConfigVersion:         summary.NodeConfigVersion,
			AcceptedCredentialsVersion:        summary.CredentialsVersion,
			AcceptedReplicasVersion:           summary.ReplicasVersion,
			AcceptedObservationOverlayVersion: summary.ObservationOverlayVersion,
			RecoveryMode:                      summary.Initialization == initializationRecovery,
			RuntimeResources:                  runtimeResources,
		}},
	}); err != nil {
		if cause := handshakeCause(sessionCtx); cause != nil {
			return cause
		}
		return err
	}
	slog.Info("sent agent hello", "agent_id", a.cfg.Node.ID)
	if a.logShipper != nil {
		a.logShipper.Attach(sendLogs)
		defer a.logShipper.Detach()
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
	// Newest authority epoch whose stamped payloads this session confirmed.
	confirmedEpoch := summary.AuthorityEpoch
	handshake := true
	// batchOpen marks a server batch whose batch-end marker has not arrived.
	batchOpen := false
	sendCurrentReport := func() error {
		report, err := a.supervisor.CurrentReport()
		if err != nil || report == nil {
			return err
		}
		summary, err := a.supervisor.Summary()
		if err != nil {
			return err
		}
		// Publish only reports at the accepted allocation position: mid-batch
		// the persisted report trails the accepts and would be rejected
		// against the control plane's final assignments.
		if report.GetAuthorityEpoch() != summary.AuthorityEpoch || report.GetReconciliationCursor() != summary.ReconciliationCursor {
			return nil
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
	sendAck := func() error {
		summary, err := a.supervisor.Summary()
		if err != nil {
			return err
		}
		return send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_Acknowledgement{
			Acknowledgement: cumulativeAck(a.cfg.Node.ID, sessionID, summary, confirmedEpoch),
		}})
	}
	confirmAuthority := func(epoch uint64) {
		if epoch > confirmedEpoch {
			confirmedEpoch = epoch
		}
		if !authorityConfirmed {
			a.sessionEstablishedAt = time.Now()
		}
		authorityConfirmed = true
	}
	// The managed dashboard identity must be in place before reconciliation
	// lets the runtime start the dashboard, for diffs as for checkpoints.
	reconcileAcceptedAllocations := func() error {
		desired, err := a.stateStore.desiredState()
		if err != nil {
			return fmt.Errorf("load desired state for dashboard identity: %w", err)
		}
		if err := a.refreshManagedDashboardIdentity(sessionCtx, client, desired); err != nil {
			a.supervisor.ReconcileAcceptedDesired()
			return err
		}
		a.supervisor.ReconcileAcceptedDesired()
		return nil
	}
	// Reports publish at stream-quiet points, never inside an open batch.
	reportRepublish := make(chan struct{}, 1)
	var republishTimer *time.Timer
	scheduleReportRepublish := func() {
		if republishTimer != nil {
			republishTimer.Stop()
		}
		republishTimer = time.AfterFunc(reportRepublishQuietPeriod, func() {
			select {
			case reportRepublish <- struct{}{}:
			default:
			}
		})
	}
	for {
		select {
		case <-sessionCtx.Done():
			if cause := handshakeCause(sessionCtx); cause != nil {
				return cause
			}
			return nil
		case <-handshakeTimer.C:
			if handshake {
				handshake = false
				confirmAuthority(summary.AuthorityEpoch)
				slog.Info("no sync batch; assuming unchanged reconnect", "agent_id", a.cfg.Node.ID)
				if err := sendCurrentReport(); err != nil {
					return err
				}
			}
		case <-a.supervisor.ReportNotifications():
			if handshake || !authorityConfirmed {
				continue
			}
			scheduleReportRepublish()
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
		case <-reportRepublish:
			if handshake || !authorityConfirmed {
				continue
			}
			if batchOpen {
				continue
			}
			if err := sendCurrentReport(); err != nil {
				return err
			}
		case result := <-received:
			if handshake {
				handshake = false
				if !handshakeTimer.Stop() {
					select {
					case <-handshakeTimer.C:
					default:
					}
				}
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
			if ack := result.message.GetLogBatchAck(); ack != nil {
				// Log acknowledgements have no state BatchEnd marker.
				completeAck(ack.GetBatchId())
				continue
			}
			if end := result.message.GetBatchEnd(); end != nil {
				if end.GetSessionId() != sessionID {
					return fmt.Errorf("batch end for foreign session %q", end.GetSessionId())
				}
				batchOpen = false
				scheduleReportRepublish()
				continue
			}
			// Every state message opens or extends a batch; publication waits
			// for the batch-end marker.
			batchOpen = true
			switch payload := result.message.Payload.(type) {
			case *agentv1.AgentServerMessage_DesiredState:
				state := payload.DesiredState
				if state == nil {
					continue
				}
				if _, err := a.supervisor.AcceptDesired(clusterID, sessionID, state); err != nil {
					return fmt.Errorf("accept desired state: %w", err)
				}
				confirmAuthority(state.GetAuthorityEpoch())
				if err := sendAck(); err != nil {
					return err
				}
				if err := reconcileAcceptedAllocations(); err != nil {
					return err
				}
				slog.Info("accepted checkpoint", "agent_id", a.cfg.Node.ID, "authority_epoch", state.GetAuthorityEpoch(), "cursor", state.GetReconciliationCursor(), "services", len(state.GetServices()), "volumes", len(state.GetVolumes()))
				scheduleReportRepublish()
			case *agentv1.AgentServerMessage_AllocationDiff:
				diff := payload.AllocationDiff
				if diff == nil {
					continue
				}
				if _, err := a.supervisor.AcceptDiff(clusterID, sessionID, diff); err != nil {
					return fmt.Errorf("accept allocation diff: %w", err)
				}
				confirmAuthority(diff.GetAuthorityEpoch())
				if err := sendAck(); err != nil {
					return err
				}
				if err := reconcileAcceptedAllocations(); err != nil {
					return err
				}
				slog.Info("accepted diff", "agent_id", a.cfg.Node.ID, "base", diff.GetBaseRevision(), "target", diff.GetTargetRevision(), "starts", len(diff.GetStarts()), "updates", len(diff.GetUpdates()), "stops", len(diff.GetStops()))
				scheduleReportRepublish()
			case *agentv1.AgentServerMessage_NodeConfigUpdate:
				update := payload.NodeConfigUpdate
				if update == nil {
					continue
				}
				if _, err := a.supervisor.AcceptNodeConfig(clusterID, sessionID, update); err != nil {
					return fmt.Errorf("accept node config: %w", err)
				}
				confirmAuthority(update.GetAuthorityEpoch())
				if err := sendAck(); err != nil {
					return err
				}
				a.supervisor.ReconcileAcceptedDesired()
				scheduleReportRepublish()
			case *agentv1.AgentServerMessage_PullCredentials:
				creds := payload.PullCredentials
				if creds == nil {
					continue
				}
				if _, err := a.supervisor.AcceptCredentials(clusterID, sessionID, creds); err != nil {
					return fmt.Errorf("accept pull credentials: %w", err)
				}
				confirmAuthority(creds.GetAuthorityEpoch())
				if err := sendAck(); err != nil {
					return err
				}
				// Credentials may unblock image pulls; reconcile to retry.
				a.supervisor.ReconcileAcceptedDesired()
				scheduleReportRepublish()
			case *agentv1.AgentServerMessage_ReplicaEndpoints:
				replicas := payload.ReplicaEndpoints
				if replicas == nil {
					continue
				}
				if _, err := a.supervisor.AcceptReplicas(clusterID, sessionID, replicas); err != nil {
					return fmt.Errorf("accept replica endpoints: %w", err)
				}
				confirmAuthority(replicas.GetAuthorityEpoch())
				if err := sendAck(); err != nil {
					return err
				}
				a.supervisor.ReconcileAcceptedDesired()
				scheduleReportRepublish()
			default:
				continue
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
