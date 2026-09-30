//go:build integration

package controlplane

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/reconciliation"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestAgentEnrollAndSyncOverLiveTLS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		agentID        = "e2e-agent"
		bootstrapToken = "e2e-single-use-bootstrap-token"
	)
	cfg := config.ControlPlaneConfig{
		Profile: config.ProfileDevelopment,
		InternalGRPC: config.ListenerConfig{
			Listen: "127.0.0.1:0",
			TLS: config.ServerTLSConfig{
				ServerNames:             []string{"localhost"},
				BootstrapTokens:         []config.AgentBootstrapToken{{AgentID: agentID, Token: bootstrapToken}},
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
		Ingress:   config.IngressConfig{PublicAddr: "platform.local", XDSListen: "127.0.0.1:0"},
		Dashboard: config.ManagedDashboardConfig{ServiceCallerID: "dashboard-test"},
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

	dashboardIdentity, err := server.EnsureDashboardClientIdentity(ctx, "dashboard-test")
	if err != nil {
		t.Fatalf("EnsureDashboardClientIdentity: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(dashboardIdentity.CAPEM) {
		t.Fatal("AppendCertsFromPEM: no certificates added")
	}

	bootstrapConn := dialLiveTLS(t, server.InternalAddr(), &tls.Config{
		RootCAs:    roots,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS13,
	})
	bootstrapClient := agentv1.NewAgentControlClient(bootstrapConn)
	key, csrPEM := newAgentCSR(t, agentID)
	enrolled, err := bootstrapClient.Enroll(ctx, &agentv1.EnrollRequest{
		AgentId:        agentID,
		CsrPem:         string(csrPEM),
		BootstrapToken: bootstrapToken,
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if enrolled.GetCertPem() == "" || enrolled.GetCaPem() == "" {
		t.Fatal("Enroll returned incomplete certificate material")
	}

	retry, err := bootstrapClient.Enroll(ctx, &agentv1.EnrollRequest{
		AgentId:        agentID,
		CsrPem:         string(csrPEM),
		BootstrapToken: bootstrapToken,
	})
	if err != nil {
		t.Fatalf("same-key enrollment retry after success: %v", err)
	}
	if retry.GetCertPem() == "" {
		t.Fatal("same-key enrollment retry returned no certificate")
	}
	_, otherKeyPEM := newAgentCSR(t, agentID)
	_, err = bootstrapClient.Enroll(ctx, &agentv1.EnrollRequest{
		AgentId:        agentID,
		CsrPem:         string(otherKeyPEM),
		BootstrapToken: bootstrapToken,
	})
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("consumed token with a different key: got %s, want Unauthenticated", got)
	}
	if err := bootstrapConn.Close(); err != nil {
		t.Fatalf("close bootstrap connection: %v", err)
	}

	agentCert := tls.Certificate{
		Certificate: [][]byte{mustDecodePEMBlock(t, enrolled.GetCertPem(), "CERTIFICATE")},
		PrivateKey:  key,
	}
	agentConn := dialLiveTLS(t, server.InternalAddr(), &tls.Config{
		Certificates: []tls.Certificate{agentCert},
		RootCAs:      roots,
		ServerName:   "localhost",
		MinVersion:   tls.VersionTLS13,
	})
	defer agentConn.Close()
	stream, err := agentv1.NewAgentControlClient(agentConn).Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	sessionID := "agent-server-session"
	clusterID, err := server.authority.ClusterIdentity(ctx)
	if err != nil {
		t.Fatalf("ClusterIdentity: %v", err)
	}
	if err := stream.Send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_Hello{Hello: &agentv1.AgentHello{
		AgentId:                 agentID,
		Name:                    "E2E agent",
		AdvertiseAddr:           "fd00:30::10",
		WireguardPublicKey:      "e2e-public-key",
		WireguardListenPort:     51820,
		WireguardEndpoint:       "[fd00:30::10]:51820",
		CpuMillisCapacity:       2000,
		MemoryMebibytesCapacity: 4096,
		RuntimeCapabilities:     []string{"containerd", "wireguard", "ebpf-policy"},
		SoftwareVersion:         "test",
		SessionId:               sessionID,
		SessionIncarnation:      1,
		ClusterId:               clusterID, LocalStoreId: "test-store-" + agentID, InitializationState: "uninitialized",
	}}}); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	initial := recvDesiredState(t, stream)
	if initial.GetAgentId() != agentID || len(initial.GetServices()) != 0 {
		t.Fatalf("unexpected initial desired state: %+v", initial)
	}

	dashboardConn := newDashboardPlatformClientConn(t, server.InternalAddr(), dashboardIdentity)
	defer dashboardConn.Close()
	platformClient := platformv1.NewPlatformServiceClient(dashboardConn)
	userCtx := metadata.AppendToOutgoingContext(ctx, userAssertionHeader, signedLiveUserAssertion(t, server, "e2e-user"))
	project, err := platformClient.CreateProject(userCtx, &platformv1.CreateProjectRequest{Name: "agent-sync-e2e"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	environments, err := platformClient.ListEnvironments(userCtx, &platformv1.ListEnvironmentsRequest{ProjectId: project.GetId()})
	if err != nil || len(environments.GetEnvironments()) != 1 {
		t.Fatalf("ListEnvironments: %+v: %v", environments, err)
	}
	environmentID := environments.GetEnvironments()[0].GetId()
	service, err := platformClient.CreateService(userCtx, &platformv1.CreateServiceRequest{
		EnvironmentId: environmentID,
		Service: &platformv1.ServiceInput{
			Name: "web",
			Spec: directImageServiceSpec(pinnedImage("e"), &platformv1.ServiceRuntime{
				CpuMillis:       250,
				MemoryMebibytes: 256,
				Ports:           runtimePortsFromInts([]int32{8080}),
			}),
		},
	})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if _, err := platformClient.ReleaseEnvironment(userCtx, &platformv1.ReleaseEnvironmentRequest{EnvironmentId: environmentID}); err != nil {
		t.Fatalf("ReleaseEnvironment: %v", err)
	}
	desired := recvDesiredState(t, stream)
	if len(desired.GetServices()) != 1 {
		t.Fatalf("desired services: got %d, want 1", len(desired.GetServices()))
	}
	desiredService := desired.GetServices()[0]
	if desiredService.GetServiceId() != service.GetId() || desiredService.GetSpec().GetImage() != pinnedImage("e") {
		t.Fatalf("unexpected desired service: %+v", desiredService)
	}

	if err := stream.Send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_StatusReport{StatusReport: &agentv1.StatusReport{
		AgentId:              agentID,
		SessionId:            sessionID,
		ObservationSequence:  1,
		AuthorityEpoch:       desired.GetAuthorityEpoch(),
		ReconciliationCursor: desired.GetReconciliationCursor(),
		Services: []*agentv1.ServiceCondition{{
			AllocationId:             desiredService.GetAllocationId(),
			ServiceId:                service.GetId(),
			DesiredSpecRevision:      desiredService.GetDesiredSpecRevision(),
			AppliedSpecRevision:      desiredService.GetDesiredSpecRevision(),
			DesiredRolloutGeneration: desiredService.GetDesiredRolloutGeneration(),
			AppliedRolloutGeneration: desiredService.GetDesiredRolloutGeneration(),
			Phase:                    "Healthy",
			Healthy:                  true,
			AllocationIpv4:           desiredService.GetPrivateIpv4(),
			AllocationIpv6:           desiredService.GetPrivateIpv6(),
			HealthyIpv4Ports:         []int32{8080},
			HealthyIpv6Ports:         []int32{8080},
		}},
	}}}); err != nil {
		t.Fatalf("send status report: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		statusResp, statusErr := platformClient.GetServiceStatus(userCtx, &platformv1.GetServiceStatusRequest{
			ServiceId: service.GetId(),
		})
		if statusErr == nil && statusResp.GetAllocation().GetHealthy() && statusResp.GetAllocation().GetAppliedSpecRevision() == desiredService.GetDesiredSpecRevision() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status report was not persisted: response=%+v err=%v", statusResp, statusErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func dialLiveTLS(t *testing.T, address string, tlsConfig *tls.Config) *grpc.ClientConn {
	t.Helper()
	dialCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, address, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), grpc.WithBlock())
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	return conn
}

func newAgentCSR(t *testing.T, commonName string) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: commonName},
	}, key)
	if err != nil {
		t.Fatalf("CreateCertificateRequest: %v", err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func mustDecodePEMBlock(t *testing.T, raw, blockType string) []byte {
	t.Helper()
	block, rest := pem.Decode([]byte(raw))
	if block == nil || block.Type != blockType || len(rest) != 0 {
		t.Fatalf("decode %s PEM", blockType)
	}
	return block.Bytes
}

func recvDesiredState(t *testing.T, stream agentv1.AgentControl_SyncClient) *agentv1.DesiredNodeState {
	t.Helper()
	// Batches may interleave independent streams around allocation payloads; assertions synthesize a checkpoint view from diff starts/updates.
	deadline := time.After(10 * time.Second)
	for {
		type result struct {
			message *agentv1.AgentServerMessage
			err     error
		}
		resultCh := make(chan result, 1)
		go func() {
			message, err := stream.Recv()
			resultCh <- result{message: message, err: err}
		}()
		select {
		case received := <-resultCh:
			if received.err != nil {
				t.Fatalf("receive desired state: %v", received.err)
			}
			if checkpoint := received.message.GetDesiredState(); checkpoint != nil {
				return checkpoint
			}
			if diff := received.message.GetAllocationDiff(); diff != nil {
				synthesized := &agentv1.DesiredNodeState{
					AgentId:              diff.GetAgentId(),
					AuthorityEpoch:       diff.GetAuthorityEpoch(),
					ReconciliationCursor: diff.GetTargetRevision(),
					// Identity and fencing fields echo as on the wire so assertions cover the diff path too.
					ClusterId:   diff.GetClusterId(),
					GeneratedAt: diff.GetGeneratedAt(),
				}
				synthesized.Services = append(synthesized.Services, diff.GetStarts()...)
				synthesized.Services = append(synthesized.Services, diff.GetUpdates()...)
				synthesized.Volumes = append(synthesized.Volumes, diff.GetVolumeStarts()...)
				return synthesized
			}
			continue
		case <-deadline:
			t.Fatal("timeout waiting for desired state")
			return nil
		}
	}
}

func TestAuthorityCutoverWaitsForPartitionedAgentGrant(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	hello := &agentv1.AgentHello{AgentId: "isolated", SessionId: "former"}
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}
	epoch, err := store.agentAuthorityEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deadline, err := store.fleet.grantAgentCommand(ctx, hello.AgentId, hello.SessionId, epoch, deliverycore.SyncVersions{Cursor: 0})
	if err != nil {
		t.Fatal(err)
	}
	delayed := &agentv1.DesiredNodeState{}
	stampAgentCommand(delayed, hello.SessionId, epoch, deadline)
	// Simulate a holder losing its stream while an already granted command
	// remains buffered in the network. Disconnect must not shorten the grant.
	if err := testDelivery(store).EndAgentSession(ctx, hello.AgentId, hello.SessionId); err != nil {
		t.Fatal(err)
	}
	if err := store.advanceAgentAuthority(ctx, epoch); err == nil {
		t.Fatal("takeover bypassed outstanding grant")
	}
	// Exercise the actual persisted wall-clock deadline, not a synthetic epoch
	// update. The agent stays partitioned and never observes the successor.
	time.Sleep(time.Until(deadline) + 10*time.Millisecond)
	if err := store.advanceAgentAuthority(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	if err := reconciliation.ValidateCommand(delayed, hello.SessionId, time.Now().Add(-reconciliation.MaxClockSkew)); err == nil {
		t.Fatal("isolated agent accepted delayed former command")
	}
	if _, err := store.fleet.grantAgentCommand(ctx, hello.AgentId, hello.SessionId, epoch, deliverycore.SyncVersions{Cursor: 1}); err == nil {
		t.Fatal("paused former holder renewed after takeover")
	}
	if err := store.advanceAgentAuthority(ctx, epoch); err == nil {
		t.Fatal("stale owner advanced epoch twice")
	}
}

func TestSessionAcknowledgementAndFreshStoreRecovery(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	hello := &agentv1.AgentHello{AgentId: "agent", SessionId: "first", LocalStoreId: "durable-store"}
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}
	if _, err := store.fleet.grantAgentCommand(ctx, hello.AgentId, hello.SessionId, 1, deliverycore.SyncVersions{Cursor: 7}); err != nil {
		t.Fatal(err)
	}
	ack := &agentv1.DesiredStateAcknowledgement{AgentId: hello.AgentId, SessionId: hello.SessionId, AuthorityEpoch: 1, ReconciliationCursor: 7}
	if err := store.fleet.acknowledgeAgentDesired(ctx, ack); err != nil {
		t.Fatal(err)
	}
	if err := store.fleet.acknowledgeAgentDesired(ctx, ack); err != nil {
		t.Fatalf("duplicate durable acceptance: %v", err)
	}
	session, ok := fixtureLive(store).Session(hello.AgentId)
	if !ok {
		t.Fatal("missing live session")
	}
	if session.Sequence != 0 || session.AcceptedCursor != 7 {
		t.Fatalf("ack was interpreted as runtime observation: sequence=%d cursor=%d", session.Sequence, session.AcceptedCursor)
	}
	ack.ReconciliationCursor = 8
	if err := store.fleet.acknowledgeAgentDesired(ctx, ack); err == nil {
		t.Fatal("accepted acknowledgement beyond offered state")
	}
	replacement := &agentv1.AgentHello{AgentId: hello.AgentId, SessionId: "empty", LocalStoreId: "fresh-store", InitializationState: "uninitialized"}
	if _, err := testDelivery(store).RegisterAgent(ctx, replacement); err == nil {
		t.Fatal("fresh storage reused existing identity")
	}
	hello.SessionId = "second"
	hello.SessionIncarnation++
	if _, err := testDelivery(store).RegisterAgent(ctx, hello); err != nil {
		t.Fatal(err)
	}
	hello.SessionIncarnation--
	if _, err := testDelivery(store).RegisterAgent(ctx, hello); err == nil {
		t.Fatal("delayed hello superseded a newer incarnation")
	}
	ack.ReconciliationCursor = 7
	if err := store.fleet.acknowledgeAgentDesired(ctx, ack); err == nil {
		t.Fatal("accepted delayed acknowledgement from superseded session")
	}
	if _, err := store.fleet.grantAgentCommand(ctx, hello.AgentId, "first", 1, deliverycore.SyncVersions{Cursor: 8}); err == nil {
		t.Fatal("superseded session received new command grant")
	}
	if err := testDelivery(store).ObserveAgentHeartbeat(ctx, hello.AgentId, "first", false); err == nil {
		t.Fatal("superseded heartbeat accepted")
	}
}

func TestAllocationObservationRejectsStaleSessionSequenceAndForeignOwner(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("list projects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-2")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "owned", directImageServiceSpec("busybox:1.36", nil), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	allocations, err := store.reads.ListAllocationsByServiceID(ctx, service.ID)
	if err != nil || len(allocations) != 1 {
		t.Fatalf("allocations: %#v, %v", allocations, err)
	}
	allocation := allocations[0]
	report := observationReport(allocation, "test-session-node-1", 1, allocation.DesiredRolloutGeneration)
	if err := testDelivery(store).ObserveAgentStatus(ctx, "node-1", report); err != nil {
		t.Fatalf("initial observation: %v", err)
	}

	report.Services[0].Message = "must not overwrite"
	if err := testDelivery(store).ObserveAgentStatus(ctx, "node-1", report); !errors.Is(err, deliverycore.ErrStaleObservation) {
		t.Fatalf("duplicate sequence: got %v", err)
	}
	obs, ok := fixtureLive(store).Observation(allocation.ID, allocation.DesiredRolloutGeneration)
	if !ok {
		t.Fatal("missing live observation")
	}
	if obs.Message == "must not overwrite" {
		t.Fatal("stale sequence overwrote the observation")
	}

	foreign := observationReport(allocation, "test-session-node-2", 1, allocation.DesiredRolloutGeneration)
	foreign.AgentId = "node-2"
	if err := testDelivery(store).ObserveAgentStatus(ctx, "node-2", foreign); !errors.Is(err, deliverycore.ErrAllocationOwnership) {
		t.Fatalf("foreign allocation: got %v", err)
	}

	reconnected := agentHello("node-1")
	reconnected.SessionId = "replacement-session"
	if _, err := registerAgent(ctx, store, reconnected); err != nil {
		t.Fatal(err)
	}
	if err := testDelivery(store).ObserveAgentHeartbeat(ctx, "node-1", "test-session-node-1", false); !errors.Is(err, deliverycore.ErrStaleAgentSession) {
		t.Fatalf("old-session heartbeat: got %v", err)
	}
	report.ObservationSequence = 2
	if err := testDelivery(store).ObserveAgentStatus(ctx, "node-1", report); !errors.Is(err, deliverycore.ErrStaleAgentSession) {
		t.Fatalf("old session: got %v", err)
	}

	wrongAddress := observationReport(allocation, "replacement-session", 1, allocation.DesiredRolloutGeneration)
	wrongAddress.Services[0].AllocationIpv4 = "10.255.255.255"
	if err := testDelivery(store).ObserveAgentStatus(ctx, "node-1", wrongAddress); !errors.Is(err, deliverycore.ErrAllocationOwnership) {
		t.Fatalf("address ownership: got %v", err)
	}
}

func TestOlderGenerationObservationCannotSatisfyCurrentAssignment(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, _ := store.catalog.listProjects(ctx, testUser("user-1"), false)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "generation", directImageServiceSpec("busybox:1.36", nil), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	allocations, _ := store.reads.ListAllocationsByServiceID(ctx, service.ID)
	allocation := allocations[0]
	if err := store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE allocation_assignments SET desired_rollout_generation = $1 WHERE id = $2`, allocation.DesiredRolloutGeneration+1, allocation.ID); err != nil {
			return err
		}
		journal.AssignmentRow(allocation.ID).Capture(ctx)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	report := observationReport(allocation, "test-session-node-1", 1, allocation.DesiredRolloutGeneration)
	if err := testDelivery(store).ObserveAgentStatus(ctx, "node-1", report); err != nil {
		t.Fatalf("older observation: %v", err)
	}
	current, err := store.reads.ListAllocationsByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current[0].Healthy || current[0].AppliedRolloutGeneration != 0 || deliverycore.AllocationReady(current[0]) {
		t.Fatalf("older generation satisfied current readiness: %+v", current[0])
	}
	if _, ok := fixtureLive(store).Observation(allocation.ID, allocation.DesiredRolloutGeneration); !ok {
		t.Fatal("older observation was not retained")
	}
}

func observationReport(allocation deliverycore.AllocationRecord, sessionID string, sequence uint64, generation int64) *agentv1.StatusReport {
	return &agentv1.StatusReport{
		AgentId: allocation.AgentID, SessionId: sessionID, ObservationSequence: sequence,
		Services: []*agentv1.ServiceCondition{{
			AllocationId: allocation.ID, ServiceId: allocation.ServiceID,
			DesiredSpecRevision: allocation.DesiredSpecRevision, AppliedSpecRevision: allocation.DesiredSpecRevision,
			DesiredRolloutGeneration: generation, AppliedRolloutGeneration: generation,
			AllocationIpv4: allocation.AllocationIPv4, AllocationIpv6: allocation.AllocationIPv6,
			Phase: "Healthy", Healthy: true,
		}},
	}
}
