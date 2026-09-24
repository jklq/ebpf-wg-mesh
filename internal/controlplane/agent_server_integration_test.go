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
	"encoding/pem"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"

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
		Ingress:   config.IngressConfig{PublicAddr: "platform.local"},
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
	// Batches may interleave independent streams around allocation payloads;
	// assertions synthesize a checkpoint view from diff starts/updates.
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
					// Identity and fencing fields echo as on the wire so
					// assertions cover the diff path too.
					ClusterId:   diff.GetClusterId(),
					GeneratedAt: diff.GetGeneratedAt(),
				}
				synthesized.Services = append(synthesized.Services, diff.GetStarts()...)
				synthesized.Services = append(synthesized.Services, diff.GetUpdates()...)
				synthesized.Volumes = append(synthesized.Volumes, diff.GetVolumeStarts()...)
				return synthesized
			}
			// Skip node config, credentials, and replica messages.
			continue
		case <-deadline:
			t.Fatal("timeout waiting for desired state")
			return nil
		}
	}
}
