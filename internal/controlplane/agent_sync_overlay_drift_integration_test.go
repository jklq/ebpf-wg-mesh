//go:build integration

package controlplane

import (
	"context"
	"crypto/x509"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

// TestAgentSyncRepairsObservationOverlayOnConnectedSession: observation
// overlay drift at a fixed cursor must reach the connected agent as a
// same-cursor repair checkpoint, and the session goes quiet afterwards.
func TestAgentSyncRepairsObservationOverlayOnConnectedSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	const (
		agentID   = "overlay-drift-a"
		tokenA    = "e2e-overlay-drift-token"
		sessionID = "peer-bump-session-overlay-drift-a"
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
		StateDir: t.TempDir(),
		Bootstrap: config.BootstrapConfig{
			Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
		},
		Ingress:   config.IngressConfig{PublicAddr: "platform.local"},
		Dashboard: config.ManagedDashboardConfig{ServiceCallerID: "dashboard-overlay-drift"},
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

	dashboardIdentity, err := server.EnsureDashboardClientIdentity(ctx, "dashboard-overlay-drift")
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

	projects, err := server.store.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %#v, %v", projects, err)
	}
	envID := productionEnvironmentID(t, server.store, projects[0].ID)

	// Establish the session first so the repair is measured against the
	// initial checkpoint.
	conn, stream := enrollSyncAgent(ctx, t, server.InternalAddr(), roots, agentID, tokenA, agentID, "[fd00:31::30]:51820", "fd00:31::30", "overlay-drift-key", clusterID)
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

	// A healthy serving allocation contributes an internal host entry.
	// The image is a pinned digest: deploy-by-digest resolves mutable
	// tags at create, which this repair test does not exercise.
	service, err := server.delivery.CreateService(ctx, testUser("user-1"), envID, "web", directImageServiceSpec(pinnedImage("a"), &platformv1.ServiceRuntime{
		Ports: runtimePortsFromInts([]int32{8080}), CpuMillis: 100, MemoryMebibytes: 64,
	}), agentID)
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	alloc, err := server.store.reads.allocationByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatalf("allocationByServiceID: %v", err)
	}
	if err := server.store.markAllocationHealthyForTest(ctx, service.ID, alloc.AllocationIPv4, 8080); err != nil {
		t.Fatalf("markAllocationHealthyForTest: %v", err)
	}
	drainSyncQuiet(t, messages, 2*time.Second, track)

	hostname := deliverycore.InternalServiceHostname("web", service.ID)
	baseline, err := server.delivery.DesiredStateForAgent(ctx, agentID)
	if err != nil {
		t.Fatalf("DesiredStateForAgent: %v", err)
	}
	if !hasInternalHost(baseline.GetServices(), hostname) {
		t.Fatalf("fixture did not establish internal host %q: %#v", hostname, baseline.GetServices())
	}

	condition := &agentv1.ServiceCondition{
		AllocationId: alloc.ID, ServiceId: service.ID,
		DesiredSpecRevision: alloc.DesiredSpecRevision, AppliedSpecRevision: alloc.DesiredSpecRevision,
		DesiredRolloutGeneration: alloc.DesiredRolloutGeneration, AppliedRolloutGeneration: alloc.DesiredRolloutGeneration,
		AllocationIpv4: alloc.AllocationIPv4, AllocationIpv6: alloc.AllocationIPv6,
		Phase: "Healthy", Healthy: true, HealthyIpv4Ports: []int32{8080},
	}
	report := func(sequence uint64) *agentv1.StatusReport {
		return &agentv1.StatusReport{
			AgentId: agentID, SessionId: sessionID, AuthorityEpoch: track.epoch,
			ObservationSequence: sequence, Services: []*agentv1.ServiceCondition{condition},
		}
	}

	// Unhealthy removes the internal host entry without a cursor bump.
	unhealthy := &agentv1.ServiceCondition{
		AllocationId: condition.AllocationId, ServiceId: condition.ServiceId,
		DesiredSpecRevision: condition.DesiredSpecRevision, AppliedSpecRevision: condition.AppliedSpecRevision,
		DesiredRolloutGeneration: condition.DesiredRolloutGeneration, AppliedRolloutGeneration: condition.AppliedRolloutGeneration,
		AllocationIpv4: condition.AllocationIpv4, AllocationIpv6: condition.AllocationIpv6,
		Phase: "Unhealthy", Healthy: false,
	}
	condition = unhealthy
	cursorBefore := track.cursor
	if err := stream.Send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_StatusReport{StatusReport: report(2)}}); err != nil {
		t.Fatalf("send unhealthy report: %v", err)
	}
	repair := recvSyncMessage(t, messages, 30*time.Second)
	checkpoint := repair.GetDesiredState()
	if checkpoint == nil {
		t.Fatalf("overlay drift delivered %T, want a same-cursor repair checkpoint", repair.GetPayload())
	}
	if checkpoint.GetReconciliationCursor() != cursorBefore {
		t.Fatalf("repair checkpoint cursor = %d, want unchanged %d", checkpoint.GetReconciliationCursor(), cursorBefore)
	}
	if hasInternalHost(checkpoint.GetServices(), hostname) {
		t.Fatalf("repair checkpoint kept stale internal host %q: %#v", hostname, checkpoint.GetServices())
	}
	track.observe(t, repair)
	drainSyncQuiet(t, messages, time.Second, track)

	// With the overlay repaired the session is quiet: nothing else changed.
	select {
	case msg, ok := <-messages:
		if !ok {
			t.Fatal("sync stream closed after repair")
		}
		t.Fatalf("unexpected message after overlay repair: %T", msg.GetPayload())
	case <-time.After(3 * time.Second):
	}

	// Flipping healthy again must repair again: the comparison runs on every
	// sync check.
	condition = &agentv1.ServiceCondition{
		AllocationId: unhealthy.AllocationId, ServiceId: unhealthy.ServiceId,
		DesiredSpecRevision: unhealthy.DesiredSpecRevision, AppliedSpecRevision: unhealthy.AppliedSpecRevision,
		DesiredRolloutGeneration: unhealthy.DesiredRolloutGeneration, AppliedRolloutGeneration: unhealthy.AppliedRolloutGeneration,
		AllocationIpv4: unhealthy.AllocationIpv4, AllocationIpv6: unhealthy.AllocationIpv6,
		Phase: "Healthy", Healthy: true, HealthyIpv4Ports: []int32{8080},
	}
	cursorBefore = track.cursor
	if err := stream.Send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_StatusReport{StatusReport: report(3)}}); err != nil {
		t.Fatalf("send healthy report: %v", err)
	}
	repair = recvSyncMessage(t, messages, 30*time.Second)
	checkpoint = repair.GetDesiredState()
	if checkpoint == nil {
		t.Fatalf("second overlay drift delivered %T, want a same-cursor repair checkpoint", repair.GetPayload())
	}
	if checkpoint.GetReconciliationCursor() != cursorBefore {
		t.Fatalf("second repair checkpoint cursor = %d, want unchanged %d", checkpoint.GetReconciliationCursor(), cursorBefore)
	}
	if !hasInternalHost(checkpoint.GetServices(), hostname) {
		t.Fatalf("repair checkpoint did not restore internal host %q: %#v", hostname, checkpoint.GetServices())
	}
	track.observe(t, repair)
	drainSyncQuiet(t, messages, time.Second, track)
}

func hasInternalHost(services []*agentv1.DesiredService, hostname string) bool {
	for _, svc := range services {
		for _, host := range svc.GetInternalHosts() {
			if host.GetHostname() == hostname {
				return true
			}
		}
	}
	return false
}
