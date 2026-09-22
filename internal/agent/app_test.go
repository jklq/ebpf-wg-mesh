package agent

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/signkeys/signkeystest"
	"ebof-wg-mesh/internal/mesh"
	"ebof-wg-mesh/internal/reconciliation"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type recordingMesh struct {
	updates []config.MeshRuntimeConfig
	closed  int
	err     error
}

func (m *recordingMesh) Update(cfg config.MeshRuntimeConfig) error {
	m.updates = append(m.updates, cfg)
	return m.err
}

func (m *recordingMesh) Close() error {
	m.closed++
	return nil
}

func TestApplyNodeConfigUpdatesRunningMeshInPlace(t *testing.T) {
	t.Parallel()

	handle := &recordingMesh{}
	factoryCalls := 0
	app := &App{
		cfg: config.AgentConfig{Mesh: config.MeshConfig{WireGuard: config.WireGuard{
			InterfaceName: "wg0",
			PrivateKey:    "private-key",
		}}},
		meshFactory: func(context.Context, config.MeshRuntimeConfig) (MeshHandle, error) {
			factoryCalls++
			return handle, nil
		},
	}
	initial := &agentv1.AssignedNodeConfig{
		WorkloadIpv6Subnet: "fd00:44:1::/80",
		WireguardAddresses: []string{"fd00:44::1/128"},
	}
	if err := app.applyNodeConfig(context.Background(), initial); err != nil {
		t.Fatalf("initial applyNodeConfig: %v", err)
	}
	updated := &agentv1.AssignedNodeConfig{
		WorkloadIpv6Subnet: "fd00:44:1::/80",
		WireguardAddresses: []string{"fd00:44::1/128"},
		Peers: []*agentv1.WireGuardPeer{{
			Name:       "node-b",
			PublicKey:  "peer-public-key",
			Endpoint:   "[2001:db8::2]:51820",
			AllowedIps: []string{"fd00:44:2::/80"},
		}},
	}
	if err := app.applyNodeConfig(context.Background(), updated); err != nil {
		t.Fatalf("updated applyNodeConfig: %v", err)
	}

	if factoryCalls != 1 {
		t.Fatalf("expected one mesh creation, got %d", factoryCalls)
	}
	if handle.closed != 0 {
		t.Fatalf("mesh was closed during update %d times", handle.closed)
	}
	if len(handle.updates) != 1 || len(handle.updates[0].WireGuard.Peers) != 1 {
		t.Fatalf("expected updated peer assignment, got %+v", handle.updates)
	}
	if err := app.applyNodeConfig(context.Background(), updated); err != nil {
		t.Fatalf("unchanged applyNodeConfig: %v", err)
	}
	if len(handle.updates) != 1 {
		t.Fatalf("unchanged assignment triggered update: %d calls", len(handle.updates))
	}
}

func TestApplyNodeConfigKeepsPreviousAssignmentWhenUpdateFails(t *testing.T) {
	t.Parallel()

	updateErr := errors.New("update failed")
	handle := &recordingMesh{err: updateErr}
	previous := &agentv1.AssignedNodeConfig{WireguardAddresses: []string{"fd00:44::1/128"}}
	app := &App{
		cfg:            config.AgentConfig{},
		mesh:           handle,
		meshAssignment: mesh.AssignmentFrom(previous),
	}
	next := &agentv1.AssignedNodeConfig{WireguardAddresses: []string{"fd00:44::2/128"}}
	if err := app.applyNodeConfig(context.Background(), next); !errors.Is(err, updateErr) {
		t.Fatalf("applyNodeConfig error = %v, want %v", err, updateErr)
	}
	if got := app.meshAssignment.WireGuardAddresses; len(got) != 1 || got[0] != "fd00:44::1/128" {
		t.Fatalf("failed update changed assignment: %+v", app.meshAssignment)
	}
	if handle.closed != 0 {
		t.Fatalf("failed update closed mesh %d times", handle.closed)
	}
}

func TestCumulativeAckFencesToConfirmedSessionEpoch(t *testing.T) {
	t.Parallel()

	// Fresh agent: pull credentials arrive before the first checkpoint, so
	// the store's accepted allocation epoch is still zero while the session
	// runs under the confirmed authority epoch. The cumulative ack must echo
	// the confirmed epoch or the control plane rejects it as outside the
	// session authority and initial sync cannot complete.
	summary := localStateSummary{ReconciliationCursor: 0, CredentialsVersion: "creds-v1"}
	ack := cumulativeAck("node-1", "session-1", summary, 7)
	if ack.GetAuthorityEpoch() != 7 {
		t.Fatalf("ack authority epoch = %d, want confirmed session epoch 7", ack.GetAuthorityEpoch())
	}
	if ack.GetReconciliationCursor() != 0 || ack.GetCredentialsVersion() != "creds-v1" {
		t.Fatalf("ack lost cumulative position: %+v", ack)
	}
	if ack.GetAgentId() != "node-1" || ack.GetSessionId() != "session-1" {
		t.Fatalf("ack lost session identity: %+v", ack)
	}
}

func TestNextReconnectDelayBoundsReconnectStorms(t *testing.T) {
	t.Parallel()

	// A short-lived session grows the backoff to bound reconnect storms.
	if got := nextReconnectDelay(initialReconnectDelay); got != 2*time.Second {
		t.Fatalf("first unstable delay = %s, want 2s", got)
	}
	if got := nextReconnectDelay(maxReconnectDelay); got != maxReconnectDelay {
		t.Fatalf("unstable delay at maximum = %s, want %s", got, maxReconnectDelay)
	}
}

func TestRuntimeEventReconcileLoop(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan struct{}, 1)
	errs := make(chan error, 1)
	reconciled := make(chan struct{}, 1)
	reportedErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		runtimeEventReconcileLoop(ctx, events, errs, func() {
			reconciled <- struct{}{}
		}, func(err error) {
			reportedErr <- err
		})
		close(done)
	}()

	events <- struct{}{}
	select {
	case <-reconciled:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("runtime event did not trigger reconciliation")
	}
	watchErr := errors.New("watch failed")
	errs <- watchErr
	select {
	case got := <-reportedErr:
		if !errors.Is(got, watchErr) {
			t.Fatalf("reported error = %v, want %v", got, watchErr)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("runtime watch error was not reported")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("runtime event loop did not stop")
	}
}

type syncFuncServer struct {
	agentv1.UnimplementedAgentControlServer
	sync func(agentv1.AgentControl_SyncServer) error
}

func (s *syncFuncServer) Sync(stream agentv1.AgentControl_SyncServer) error { return s.sync(stream) }

func newTestTLSAuthority(t *testing.T) *identity.TLSAuthority {
	t.Helper()
	authority, err := identity.NewTLSAuthority(context.Background(), config.ControlPlaneConfig{
		StateDir: t.TempDir(),
		InternalGRPC: config.ListenerConfig{TLS: config.ServerTLSConfig{
			ServerNames:             []string{"localhost"},
			ServerCertValidityHours: 24,
			ClientCertValidityHours: 6,
		}},
	}, signkeystest.New(t))
	if err != nil {
		t.Fatalf("NewTLSAuthority: %v", err)
	}
	return authority
}

func startSyncServer(t *testing.T, authority *identity.TLSAuthority, handler func(agentv1.AgentControl_SyncServer) error) string {
	t.Helper()
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(authority.HTTPConfig())))
	agentv1.RegisterAgentControlServer(server, &syncFuncServer{sync: handler})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() {
		server.Stop()
		_ = ln.Close()
	})
	return ln.Addr().String()
}

// newSyncSessionApp builds an App enrolled against the test authority whose
// store already accepted a checkpoint at epoch 1, cursor 5 with one running
// allocation. A session under test receives independent and allocation
// updates against that baseline.
func newSyncSessionApp(t *testing.T, authority *identity.TLSAuthority) (*App, credentials.TransportCredentials, time.Time, string) {
	t.Helper()
	enrollAddr := startEnrollServer(t, authority, func(ctx context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
		return authority.Enroll(ctx, req)
	})
	privateKey, err := mesh.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey: %v", err)
	}
	app := &App{
		cfg: config.AgentConfig{
			Node: config.NodeConfig{ID: "node-1"},
			ControlPlane: config.ControlPlaneClientConfig{
				Addresses: []string{enrollAddr},
				TLS: config.ClientTLSConfig{
					CAFile:         writeTrustBundleFile(t, authority),
					ServerName:     "localhost",
					BootstrapToken: "bootstrap-token",
				},
			},
			Mesh:    config.MeshConfig{WireGuard: config.WireGuard{InterfaceName: "wg0", PrivateKey: privateKey}},
			Runtime: config.RuntimeConfig{DataDir: t.TempDir()},
		},
	}
	creds, certNotAfter, clusterIdentity, err := app.clientCredentials(context.Background())
	if err != nil {
		t.Fatalf("clientCredentials: %v", err)
	}
	store := openTestLocalState(t)
	if err := store.prepareStartup(clusterIdentity, nil); err != nil {
		t.Fatal(err)
	}
	desired := testDesiredState(1, 5, "alloc-1")
	desired.ClusterId = clusterIdentity
	if _, err := store.acceptDesired(clusterIdentity, "test-session", desired); err != nil {
		t.Fatal(err)
	}
	runtime := &supervisorTestRuntime{inventory: []RuntimeResource{{AllocationID: "alloc-1", RuntimeID: "runtime-alloc-1"}}}
	supervisor := newWorkloadSupervisor("node-1", runtime, store, func(context.Context, *agentv1.AssignedNodeConfig) error { return nil })
	supervisorCtx, supervisorCancel := context.WithCancel(context.Background())
	t.Cleanup(supervisorCancel)
	if err := supervisor.Start(supervisorCtx, clusterIdentity); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}
	app.stateStore = store
	app.supervisor = supervisor
	return app, creds, certNotAfter, clusterIdentity
}

func TestSyncSessionRepublishesReportAfterIndependentUpdate(t *testing.T) {
	t.Parallel()

	// A reconnect whose batch carries only an independent-stream update
	// (node config, credentials, or replicas) must still republish the
	// agent's runtime report: the live view drops the previous session's
	// observations when the session is replaced, and heartbeats alone leave
	// allocations pending until runtime happens to emit another report.
	authority := newTestTLSAuthority(t)
	messages := make(chan *agentv1.AgentClientMessage, 16)
	var clusterIdentity string
	syncAddr := startSyncServer(t, authority, func(stream agentv1.AgentControl_SyncServer) error {
		hello, err := stream.Recv()
		if err != nil {
			return err
		}
		if hello.GetHello() == nil {
			return errors.New("expected agent hello")
		}
		nodeConfig := &agentv1.AssignedNodeConfig{WorkloadIpv4Subnet: "10.0.0.0/24"}
		update := &agentv1.NodeConfigUpdate{
			AgentId: "node-1", ClusterId: clusterIdentity,
			NodeConfigVersion: reconciliation.HashNodeConfig(nodeConfig), NodeConfig: nodeConfig,
			AuthorityEpoch: 1, SessionId: hello.GetHello().GetSessionId(),
			AuthorityNotAfter: timestamppb.New(time.Now().Add(15 * time.Second)),
		}
		if err := stream.Send(&agentv1.AgentServerMessage{Payload: &agentv1.AgentServerMessage_NodeConfigUpdate{NodeConfigUpdate: update}}); err != nil {
			return err
		}
		for {
			msg, err := stream.Recv()
			if err != nil {
				return err
			}
			select {
			case messages <- msg:
			default:
			}
		}
	})
	app, creds, certNotAfter, clusterIdentity := newSyncSessionApp(t, authority)

	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	defer sessionCancel()
	sessionDone := make(chan error, 1)
	go func() { sessionDone <- app.runSessionAt(sessionCtx, creds, certNotAfter, clusterIdentity, syncAddr) }()

	sawAck, sawReport := false, false
	deadline := time.After(10 * time.Second)
	for !sawAck || !sawReport {
		select {
		case msg := <-messages:
			switch payload := msg.Payload.(type) {
			case *agentv1.AgentClientMessage_Acknowledgement:
				if payload.Acknowledgement.GetAuthorityEpoch() != 1 {
					t.Fatalf("ack authority epoch = %d, want session epoch 1", payload.Acknowledgement.GetAuthorityEpoch())
				}
				sawAck = true
			case *agentv1.AgentClientMessage_StatusReport:
				sawReport = true
			}
		case <-deadline:
			t.Fatalf("timeout: ack=%v report=%v; the independent update must republish runtime status", sawAck, sawReport)
		}
	}
	sessionCancel()
	select {
	case <-sessionDone:
	case <-time.After(5 * time.Second):
		t.Fatal("sync session did not stop")
	}
}

func TestSyncSessionDefersReportUntilBatchApplied(t *testing.T) {
	t.Parallel()

	// Batches interleave independent updates before allocation payloads.
	// Publishing the runtime report eagerly after the credentials update
	// emits the pre-batch inventory, which the control plane rejects for a
	// removed allocation and closes the stream. The report must arrive only
	// after the whole batch (here: credentials plus the stopping diff) has
	// been acknowledged.
	authority := newTestTLSAuthority(t)
	messages := make(chan *agentv1.AgentClientMessage, 16)
	var clusterIdentity string
	syncAddr := startSyncServer(t, authority, func(stream agentv1.AgentControl_SyncServer) error {
		hello, err := stream.Recv()
		if err != nil {
			return err
		}
		if hello.GetHello() == nil {
			return errors.New("expected agent hello")
		}
		sessionID := hello.GetHello().GetSessionId()
		creds := &agentv1.PullCredentialSet{
			AgentId: "node-1", ClusterId: clusterIdentity,
			Credentials:    []*agentv1.AllocationCredential{{AllocationId: "alloc-1", Username: "user", Password: "pass"}},
			AuthorityEpoch: 1, SessionId: sessionID,
			AuthorityNotAfter: timestamppb.New(time.Now().Add(15 * time.Second)),
		}
		creds.CredentialsVersion = reconciliation.HashCredentials(creds.GetCredentials())
		diff := &agentv1.AllocationDiff{
			AgentId: "node-1", ClusterId: clusterIdentity,
			BaseRevision: 5, TargetRevision: 6, Stops: []string{"alloc-1"},
			AuthorityEpoch: 1, SessionId: sessionID,
			AuthorityNotAfter: timestamppb.New(time.Now().Add(15 * time.Second)),
		}
		if err := stream.Send(&agentv1.AgentServerMessage{Payload: &agentv1.AgentServerMessage_PullCredentials{PullCredentials: creds}}); err != nil {
			return err
		}
		if err := stream.Send(&agentv1.AgentServerMessage{Payload: &agentv1.AgentServerMessage_AllocationDiff{AllocationDiff: diff}}); err != nil {
			return err
		}
		for {
			msg, err := stream.Recv()
			if err != nil {
				return err
			}
			select {
			case messages <- msg:
			default:
			}
		}
	})
	app, creds, certNotAfter, clusterIdentity := newSyncSessionApp(t, authority)

	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	defer sessionCancel()
	sessionDone := make(chan error, 1)
	go func() { sessionDone <- app.runSessionAt(sessionCtx, creds, certNotAfter, clusterIdentity, syncAddr) }()

	acks := 0
	sawReport := false
	deadline := time.After(10 * time.Second)
	for !sawReport {
		select {
		case msg := <-messages:
			switch msg.Payload.(type) {
			case *agentv1.AgentClientMessage_Acknowledgement:
				acks++
			case *agentv1.AgentClientMessage_StatusReport:
				if acks < 2 {
					t.Fatalf("status report published after %d acks; the batch's allocation diff must be applied and acknowledged first", acks)
				}
				sawReport = true
			}
		case <-deadline:
			t.Fatalf("timeout waiting for deferred status report (acks=%d)", acks)
		}
	}
	sessionCancel()
	select {
	case <-sessionDone:
	case <-time.After(5 * time.Second):
		t.Fatal("sync session did not stop")
	}
}
