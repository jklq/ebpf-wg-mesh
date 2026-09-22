//go:build integration

package controlplane

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// syncPosition tracks the durable position a sync stream has been advanced to
// and enforces the coverage invariant: an allocation diff may only follow at
// exactly the accepted cursor. A gap means the control plane advanced its sent
// cursor without delivering a covering allocation message — the desync that
// makes the agent reject the next diff on its base revision.
type syncPosition struct {
	epoch       uint64
	cursor      int64
	nodeConfig  string
	credentials string
	replicas    string
}

func (p *syncPosition) observe(t *testing.T, msg *agentv1.AgentServerMessage) {
	t.Helper()
	switch {
	case msg.GetDesiredState() != nil:
		checkpoint := msg.GetDesiredState()
		if checkpoint.GetReconciliationCursor() < p.cursor {
			t.Fatalf("checkpoint cursor went backwards: %d -> %d", p.cursor, checkpoint.GetReconciliationCursor())
		}
		p.epoch = checkpoint.GetAuthorityEpoch()
		p.cursor = checkpoint.GetReconciliationCursor()
		p.nodeConfig = checkpoint.GetNodeConfigVersion()
	case msg.GetAllocationDiff() != nil:
		diff := msg.GetAllocationDiff()
		if diff.GetBaseRevision() != p.cursor {
			t.Fatalf("diff base %d does not chain from the delivered cursor %d", diff.GetBaseRevision(), p.cursor)
		}
		if diff.GetTargetRevision() <= p.cursor {
			t.Fatalf("diff target %d does not advance past %d", diff.GetTargetRevision(), p.cursor)
		}
		p.epoch = diff.GetAuthorityEpoch()
		p.cursor = diff.GetTargetRevision()
	case msg.GetNodeConfigUpdate() != nil:
		update := msg.GetNodeConfigUpdate()
		p.epoch = update.GetAuthorityEpoch()
		p.nodeConfig = update.GetNodeConfigVersion()
	case msg.GetPullCredentials() != nil:
		creds := msg.GetPullCredentials()
		p.epoch = creds.GetAuthorityEpoch()
		p.credentials = creds.GetCredentialsVersion()
	case msg.GetReplicaEndpoints() != nil:
		replicas := msg.GetReplicaEndpoints()
		p.epoch = replicas.GetAuthorityEpoch()
		p.replicas = replicas.GetReplicasVersion()
	default:
		t.Fatalf("unknown sync message payload: %T", msg.GetPayload())
	}
}

// syncMessageReader pumps the stream so no message is lost between timed
// receives and quiet-period probes.
func syncMessageReader(stream agentv1.AgentControl_SyncClient) <-chan *agentv1.AgentServerMessage {
	ch := make(chan *agentv1.AgentServerMessage, 32)
	go func() {
		defer close(ch)
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			ch <- msg
		}
	}()
	return ch
}

func recvSyncMessage(t *testing.T, ch <-chan *agentv1.AgentServerMessage, timeout time.Duration) *agentv1.AgentServerMessage {
	t.Helper()
	select {
	case msg, ok := <-ch:
		if !ok || msg == nil {
			t.Fatal("sync stream closed while waiting for a message")
		}
		return msg
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a sync message")
		return nil
	}
}

// drainSyncQuiet consumes messages until the stream stays quiet for the given
// duration, so the next phase observes only its own batches.
func drainSyncQuiet(t *testing.T, ch <-chan *agentv1.AgentServerMessage, quiet time.Duration, track *syncPosition) {
	t.Helper()
	for {
		select {
		case msg := <-ch:
			if msg == nil {
				return
			}
			track.observe(t, msg)
		case <-time.After(quiet):
			return
		}
	}
}

func enrollSyncAgent(ctx context.Context, t *testing.T, address string, roots *x509.CertPool, agentID, bootstrapToken, name, endpoint, advertiseAddr, publicKey, clusterID string) (*grpc.ClientConn, agentv1.AgentControl_SyncClient) {
	t.Helper()
	bootstrapConn := dialLiveTLS(t, address, &tls.Config{
		RootCAs:    roots,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS13,
	})
	defer bootstrapConn.Close()
	bootstrapClient := agentv1.NewAgentControlClient(bootstrapConn)
	key, csrPEM := newAgentCSR(t, agentID)
	enrolled, err := bootstrapClient.Enroll(ctx, &agentv1.EnrollRequest{
		AgentId: agentID, CsrPem: string(csrPEM), BootstrapToken: bootstrapToken,
	})
	if err != nil {
		t.Fatalf("Enroll %s: %v", agentID, err)
	}
	conn := dialLiveTLS(t, address, &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{mustDecodePEMBlock(t, enrolled.GetCertPem(), "CERTIFICATE")},
			PrivateKey:  key,
		}},
		RootCAs:    roots,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS13,
	})
	stream, err := agentv1.NewAgentControlClient(conn).Sync(ctx)
	if err != nil {
		conn.Close()
		t.Fatalf("Sync %s: %v", agentID, err)
	}
	if err := stream.Send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_Hello{Hello: &agentv1.AgentHello{
		AgentId:                 agentID,
		Name:                    name,
		AdvertiseAddr:           advertiseAddr,
		WireguardPublicKey:      publicKey,
		WireguardListenPort:     51820,
		WireguardEndpoint:       endpoint,
		CpuMillisCapacity:       2000,
		MemoryMebibytesCapacity: 4096,
		RuntimeCapabilities:     []string{"containerd", "wireguard", "ebpf-policy"},
		SoftwareVersion:         "test",
		SessionId:               "peer-bump-session-" + agentID,
		SessionIncarnation:      1,
		ClusterId:               clusterID,
		LocalStoreId:            "peer-bump-store-" + agentID,
		InitializationState:     "uninitialized",
	}}}); err != nil {
		conn.Close()
		t.Fatalf("hello %s: %v", agentID, err)
	}
	return conn, stream
}

// TestAgentSyncPeerOnlyBumpKeepsAllocationCursorInLockstep pins the sent-cursor
// contract of checkpoint-plus-diff sync. A peer-only change (another agent's
// registration) bumps desired_revision without changing allocation content,
// and the batch must still deliver a cursor-covering allocation message — the
// empty no-op cursor diff — alongside the node config update. If the control
// plane advanced its sent cursor without that diff, the follow-up allocation
// diff would be based on a position the agent never accepted and the agent
// would reject it, ending the sync session.
func TestAgentSyncPeerOnlyBumpKeepsAllocationCursorInLockstep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	const (
		agentA = "peer-bump-a"
		agentB = "peer-bump-b"
		tokenA = "e2e-peer-bump-token-a"
		tokenB = "e2e-peer-bump-token-b"
	)
	cfg := config.ControlPlaneConfig{
		Profile: config.ProfileDevelopment,
		InternalGRPC: config.ListenerConfig{
			Listen: "127.0.0.1:0",
			TLS: config.ServerTLSConfig{
				ServerNames:             []string{"localhost"},
				BootstrapTokens:         []config.AgentBootstrapToken{{AgentID: agentA, Token: tokenA}, {AgentID: agentB, Token: tokenB}},
				ServerCertValidityHours: 24,
				ClientCertValidityHours: 24,
			},
		},
		Database: config.DatabaseConfig{
			URL:          createTestDatabase(t),
			MaxOpenConns: 4,
			MaxIdleConns: 4,
		},
		StateDir:  t.TempDir(),
		Ingress:   config.IngressConfig{PublicAddr: "platform.local"},
		Dashboard: config.ManagedDashboardConfig{ServiceCallerID: "dashboard-peer-bump"},
		Mesh:      testMeshConfig(),
	}
	if err := config.FinalizeControlPlane(&cfg); err != nil {
		t.Fatalf("FinalizeControlPlane: %v", err)
	}

	server, err := NewServer(ctx, cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- server.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := server.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		select {
		case err := <-runErrCh:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("timeout waiting for server.Run to exit")
		}
	})
	waitForListener(t, server.InternalAddr())
	waitForSingletonLease(t, server)

	dashboardIdentity, err := server.EnsureDashboardClientIdentity(ctx, "dashboard-peer-bump")
	if err != nil {
		t.Fatalf("EnsureDashboardClientIdentity: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(dashboardIdentity.CAPEM) {
		t.Fatal("failed to parse CA PEM")
	}
	clusterID, err := server.authority.ClusterIdentity(ctx)
	if err != nil {
		t.Fatalf("ClusterIdentity: %v", err)
	}
	if _, err := server.store.db.ExecContext(ctx,
		`INSERT INTO platform_operators(user_id, created_at) VALUES ($1, statement_timestamp())`, "peer-bump-user"); err != nil {
		t.Fatalf("seed operator: %v", err)
	}

	connA, streamA := enrollSyncAgent(ctx, t, server.InternalAddr(), roots, agentA, tokenA, agentA, "[fd00:31::10]:51820", "fd00:31::10", "peer-bump-key-a", clusterID)
	defer connA.Close()
	connB, _ := enrollSyncAgent(ctx, t, server.InternalAddr(), roots, agentB, tokenB, agentB, "[fd00:31::11]:51820", "fd00:31::11", "peer-bump-key-b", clusterID)
	defer connB.Close()
	// Stream B stays idle below: its hello only registers the peer whose
	// admin rename is the peer-only change under test.

	messages := syncMessageReader(streamA)
	track := &syncPosition{}
	for {
		msg := recvSyncMessage(t, messages, 30*time.Second)
		track.observe(t, msg)
		if msg.GetDesiredState() != nil {
			break
		}
	}
	if track.cursor == 0 {
		t.Fatal("initialization checkpoint did not establish a cursor")
	}

	dashboardConn := newDashboardPlatformClientConn(t, server.InternalAddr(), dashboardIdentity)
	defer dashboardConn.Close()
	platformClient := platformv1.NewPlatformServiceClient(dashboardConn)
	opsClient := platformv1.NewOpsServiceClient(dashboardConn)
	userCtx := metadata.AppendToOutgoingContext(ctx, userAssertionHeader, signedLiveUserAssertion(t, server, "peer-bump-user"))

	project, err := platformClient.CreateProject(userCtx, &platformv1.CreateProjectRequest{Name: "agent-sync-peer-bump"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	environments, err := platformClient.ListEnvironments(userCtx, &platformv1.ListEnvironmentsRequest{ProjectId: project.GetId()})
	if err != nil || len(environments.GetEnvironments()) != 1 {
		t.Fatalf("ListEnvironments: %+v: %v", environments, err)
	}
	environmentID := environments.GetEnvironments()[0].GetId()

	// Deterministic placement: each agent serves exactly one region, and the
	// services under test pin their placement region so both agents hold an
	// allocation in the shared environment (the peer-change bump fanout).
	setAgent := func(id, name, region string) {
		t.Helper()
		if _, err := opsClient.UpdateAgent(userCtx, &platformv1.UpdateAgentRequest{
			AgentId: id, Name: name, Region: region, FailureDomain: "fd-" + region,
		}); err != nil {
			t.Fatalf("UpdateAgent %s: %v", id, err)
		}
	}
	setAgent(agentA, agentA, "region-a")
	setAgent(agentB, agentB, "region-b")

	newService := func(name, image, region string) *platformv1.Service {
		t.Helper()
		spec := directImageServiceSpec(image, &platformv1.ServiceRuntime{
			CpuMillis: 250, MemoryMebibytes: 256, Ports: runtimePortsFromInts([]int32{8080}),
		})
		spec.PlacementRegion = region
		created, err := platformClient.CreateService(userCtx, &platformv1.CreateServiceRequest{
			EnvironmentId: environmentID,
			Service:       &platformv1.ServiceInput{Name: name, Spec: spec},
		})
		if err != nil {
			t.Fatalf("CreateService %s: %v", name, err)
		}
		return created
	}
	release := func() {
		t.Helper()
		if _, err := platformClient.ReleaseEnvironment(userCtx, &platformv1.ReleaseEnvironmentRequest{EnvironmentId: environmentID}); err != nil {
			t.Fatalf("ReleaseEnvironment: %v", err)
		}
	}

	// Phase 1: one allocation per agent in the shared environment.
	webA := newService("web-a", "example.test/e2e:1", "region-a")
	newService("web-b", "example.test/e2e:1", "region-b")
	release()
	sawAllocationA := false
	for !sawAllocationA {
		msg := recvSyncMessage(t, messages, 30*time.Second)
		track.observe(t, msg)
		for _, svc := range msg.GetAllocationDiff().GetStarts() {
			sawAllocationA = sawAllocationA || svc.GetServiceId() == webA.GetId()
		}
		for _, svc := range msg.GetDesiredState().GetServices() {
			sawAllocationA = sawAllocationA || svc.GetServiceId() == webA.GetId()
		}
	}
	drainSyncQuiet(t, messages, time.Second, track)

	// Phase 2: the peer-only change. Renaming agent B changes A's node
	// config (the peer entry) and bumps A's desired_revision without any
	// allocation content change.
	deliveredCursor := track.cursor
	nodeConfigVersion := track.nodeConfig
	setAgent(agentB, "renamed-"+agentB, "region-b")
	msg := recvSyncMessage(t, messages, 30*time.Second)
	track.observe(t, msg)
	if update := msg.GetNodeConfigUpdate(); update == nil {
		t.Fatalf("peer-only batch must lead with a node config update, got %T", msg.GetPayload())
	} else if update.GetNodeConfigVersion() == "" || update.GetNodeConfigVersion() == nodeConfigVersion {
		t.Fatalf("node config version did not change: %q -> %q", nodeConfigVersion, update.GetNodeConfigVersion())
	}
	var noop *agentv1.AllocationDiff
	for noop == nil {
		msg = recvSyncMessage(t, messages, 30*time.Second)
		track.observe(t, msg)
		noop = msg.GetAllocationDiff()
	}
	if noop.GetBaseRevision() != deliveredCursor {
		t.Fatalf("no-op cursor diff base %d does not cover delivered cursor %d", noop.GetBaseRevision(), deliveredCursor)
	}
	if noop.GetTargetRevision() <= deliveredCursor {
		t.Fatalf("no-op cursor diff target %d does not advance past %d", noop.GetTargetRevision(), deliveredCursor)
	}
	if len(noop.GetStarts())+len(noop.GetUpdates())+len(noop.GetStops())+len(noop.GetVolumeStarts())+len(noop.GetVolumeStops()) != 0 {
		t.Fatalf("peer-only diff changed allocations: %+v", noop)
	}
	drainSyncQuiet(t, messages, time.Second, track)

	// Phase 3: the follow-up allocation change must chain from the cursor the
	// no-op diff delivered. A skipped base is the rejection that would end
	// the sync session against a real agent.
	webA2 := newService("web-a2", "example.test/e2e:2", "region-a")
	release()
	sawAllocationA2 := false
	for !sawAllocationA2 {
		msg := recvSyncMessage(t, messages, 30*time.Second)
		track.observe(t, msg)
		if diff := msg.GetAllocationDiff(); diff != nil {
			for _, svc := range diff.GetStarts() {
				sawAllocationA2 = sawAllocationA2 || svc.GetServiceId() == webA2.GetId()
			}
		}
	}
}
