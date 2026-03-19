//go:build integration

package controlplane

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/testutil"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestControlPlaneServerIntegrationRunsProjectFlowOverRealTLSAndStore(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cfg := config.ControlPlaneConfig{
		PublicHTTP: config.ListenerConfig{
			Listen: "127.0.0.1:0",
		},
		InternalGRPC: config.ListenerConfig{
			Listen: "127.0.0.1:0",
			TLS: config.ServerTLSConfig{
				ServerNames:             []string{"localhost"},
				BootstrapTokens:         []string{"bootstrap-token"},
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
		Ingress: config.IngressConfig{
			PublicAddr: "platform.local",
		},
		Mesh: config.ControlPlaneMeshConfig{
			InterfaceName:              "wg0",
			ListenPort:                 51820,
			NetworkCIDR:                "fd00:44::/64",
			WorkloadPoolCIDR:           "fd00:200::/48",
			PersistentKeepaliveSeconds: 5,
		},
	}
	if err := config.FinalizeControlPlane(&cfg); err != nil {
		t.Fatalf("FinalizeControlPlane: %v", err)
	}

	server, err := NewServer(ctx, cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- server.Run(ctx)
	}()
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
			t.Errorf("timeout waiting for server.Run to exit")
		}
	})

	waitForHTTPHealthz(t, "http://"+server.PublicAddr())
	waitForListener(t, server.InternalAddr())

	identity, err := server.EnsureDashboardClientIdentity("dashboard-test")
	if err != nil {
		t.Fatalf("EnsureDashboardClientIdentity: %v", err)
	}

	conn := newDashboardPlatformClientConn(t, server.InternalAddr(), identity)
	defer conn.Close()

	client := platformv1.NewPlatformServiceClient(conn)
	if _, err := client.EnsurePrincipal(ctx, &platformv1.EnsurePrincipalRequest{
		Subject: "user-1",
		Email:   "user@example.com",
	}); err != nil {
		t.Fatalf("EnsurePrincipal: %v", err)
	}

	delegatedCtx := metadata.AppendToOutgoingContext(
		ctx,
		delegatedUserSubjectHeader, "user-1",
		delegatedUserEmailHeader, "user@example.com",
	)

	projects, err := client.ListProjects(delegatedCtx, &emptypb.Empty{})
	if err != nil {
		t.Fatalf("ListProjects(before create): %v", err)
	}
	if got := len(projects.GetProjects()); got != 0 {
		t.Fatalf("expected no projects before create, got %d", got)
	}

	created, err := client.CreateProject(delegatedCtx, &platformv1.CreateProjectRequest{
		Name: "demo-app",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if created.GetId() == "" || created.GetName() != "demo-app" {
		t.Fatalf("unexpected created project %+v", created)
	}
	if created.GetKind() != platformv1.ProjectKind_PROJECT_KIND_USER {
		t.Fatalf("expected user project kind, got %s", created.GetKind())
	}

	projects, err = client.ListProjects(delegatedCtx, &emptypb.Empty{})
	if err != nil {
		t.Fatalf("ListProjects(after create): %v", err)
	}
	if got := projects.GetProjects(); len(got) != 1 {
		t.Fatalf("expected one project after create, got %d", len(got))
	} else if got[0].GetId() != created.GetId() || got[0].GetName() != "demo-app" {
		t.Fatalf("unexpected listed project %+v", got[0])
	}
}

func waitForHTTPHealthz(t *testing.T, baseURL string) {
	t.Helper()

	if err := testutil.Poll(context.Background(), testutil.PollConfig{Timeout: 10 * time.Second}, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
		if err != nil {
			return false, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK, nil
	}); err != nil {
		t.Fatalf("wait for public HTTP healthz: %v", err)
	}
}

func waitForListener(t *testing.T, address string) {
	t.Helper()

	if err := testutil.Poll(context.Background(), testutil.PollConfig{Timeout: 10 * time.Second}, func(ctx context.Context) (bool, error) {
		conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err != nil {
			return false, nil
		}
		_ = conn.Close()
		return true, nil
	}); err != nil {
		t.Fatalf("wait for listener %s: %v", address, err)
	}
}

func newDashboardPlatformClientConn(t *testing.T, address string, identity ClientIdentityMaterial) *grpc.ClientConn {
	t.Helper()

	clientCert, err := tls.X509KeyPair(identity.CertPEM, identity.KeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(identity.CAPEM) {
		t.Fatal("AppendCertsFromPEM: no certificates added")
	}

	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      roots,
		ServerName:   "localhost",
	})

	dialCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, address, grpc.WithTransportCredentials(creds), grpc.WithBlock())
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	return conn
}
