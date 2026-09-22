//go:build integration

package controlplane

import (
	"context"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/reconciliation"

	"crypto/x509"
)

// TestAgentSyncRepairsObservationOverlayOnReconnect pins the reconnect repair
// for observation-derived desired fields. Internal hosts and restart
// observations rebuild from live control-plane observations without a
// desired_revision bump, so a ready reconnect that echoes a stale observation
// overlay version must receive a repair checkpoint even though its cursor,
// inventory, and every stream version match. The repair applies at the same
// cursor (the agent treats the overlay as live-derived, like node config). A
// reconnect echoing the current overlay version still sends nothing.
func TestAgentSyncRepairsObservationOverlayOnReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	const (
		agentID   = "overlay-repair-a"
		tokenA    = "e2e-overlay-repair-token"
		sessionID = "overlay-repair-session"
	)
	cfg := config.ControlPlaneConfig{
		Profile: config.ProfileDevelopment,
		InternalGRPC: config.ListenerConfig{
			Listen: "127.0.0.1:0",
			TLS: config.ServerTLSConfig{
				ServerNames:             []string{"localhost"},
				BootstrapTokens:         []config.AgentBootstrapToken{{AgentID: agentID, Token: tokenA}},
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
		Dashboard: config.ManagedDashboardConfig{ServiceCallerID: "dashboard-overlay-repair"},
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

	dashboardIdentity, err := server.EnsureDashboardClientIdentity(ctx, "dashboard-overlay-repair")
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

	// Session 1: a fresh agent is established with an initialization
	// checkpoint and learns the current content versions.
	conn, stream := enrollSyncAgent(ctx, t, server.InternalAddr(), roots, agentID, tokenA, agentID, "[fd00:31::20]:51820", "fd00:31::20", "overlay-repair-key", clusterID)
	defer conn.Close()
	messages := syncMessageReader(stream)
	track := &syncPosition{}
	var initial *agentv1.DesiredNodeState
	for initial == nil {
		msg := recvSyncMessage(t, messages, 30*time.Second)
		track.observe(t, msg)
		initial = msg.GetDesiredState()
	}
	drainSyncQuiet(t, messages, time.Second, track)
	overlayVersion := reconciliation.HashObservationOverlay(initial.GetServices())

	// Reconnects take over the previous session and must present a strictly
	// newer session incarnation.
	reconnect := func(t *testing.T, overlay string, incarnation uint64) (agentv1.AgentControl_SyncClient, <-chan *agentv1.AgentServerMessage) {
		t.Helper()
		stream, err := agentv1.NewAgentControlClient(conn).Sync(ctx)
		if err != nil {
			t.Fatalf("Sync reconnect: %v", err)
		}
		if err := stream.Send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_Hello{Hello: &agentv1.AgentHello{
			AgentId:                 agentID,
			Name:                    agentID,
			AdvertiseAddr:           "fd00:31::20",
			WireguardPublicKey:      "overlay-repair-key",
			WireguardListenPort:     51820,
			WireguardEndpoint:       "[fd00:31::20]:51820",
			CpuMillisCapacity:       2000,
			MemoryMebibytesCapacity: 4096,
			RuntimeCapabilities:     []string{"containerd"},
			SoftwareVersion:         "test",
			SessionId:               sessionID + "-" + overlay,
			SessionIncarnation:      incarnation,
			ClusterId:               clusterID,
			// Same local store as the session-1 hello (enrollSyncAgent); a
			// differing store id is identity recovery, not a reconnect.
			LocalStoreId:                      "peer-bump-store-" + agentID,
			InitializationState:               "ready",
			AcceptedAuthorityEpoch:            track.epoch,
			ReconciliationCursor:              track.cursor,
			AcceptedNodeConfigVersion:         track.nodeConfig,
			AcceptedCredentialsVersion:        track.credentials,
			AcceptedReplicasVersion:           track.replicas,
			AcceptedObservationOverlayVersion: overlay,
		}}}); err != nil {
			t.Fatalf("send reconnect hello: %v", err)
		}
		return stream, syncMessageReader(stream)
	}

	// Reconnect echoing the current observation overlay version: nothing is
	// out of date, so the unchanged reconnect must send nothing.
	stream2, messages2 := reconnect(t, overlayVersion, 2)
	defer stream2.CloseSend()
	select {
	case msg, ok := <-messages2:
		if !ok {
			t.Fatal("unchanged reconnect stream closed")
		}
		t.Fatalf("unchanged reconnect received %T; want no messages", msg.GetPayload())
	case <-time.After(3 * time.Second):
	}

	// Reconnect echoing a stale observation overlay version: the cursor,
	// inventory, and every stream version match, but the overlay must be
	// repaired with a checkpoint at the same cursor.
	stream3, messages3 := reconnect(t, "stale-observation-overlay", 3)
	defer stream3.CloseSend()
	repair := recvSyncMessage(t, messages3, 30*time.Second)
	if checkpoint := repair.GetDesiredState(); checkpoint == nil {
		t.Fatalf("stale overlay repair sent %T, want a checkpoint", repair.GetPayload())
	} else if checkpoint.GetReconciliationCursor() != track.cursor {
		t.Fatalf("repair checkpoint cursor = %d, want unchanged %d", checkpoint.GetReconciliationCursor(), track.cursor)
	}
}
