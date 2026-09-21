package agent

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/signkeys/signkeystest"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type enrollFuncServer struct {
	agentv1.UnimplementedAgentControlServer
	enroll func(context.Context, *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error)
}

func (s *enrollFuncServer) Enroll(ctx context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
	return s.enroll(ctx, req)
}

func TestEnrollWalksPastStalledReplicaFollowsRedirectAndSkipsDeadOwner(t *testing.T) {
	oldRPC, oldDial, oldCooldown := replicaRPCTimeout, replicaDialTimeout, deadOwnerCooldown
	replicaRPCTimeout = 250 * time.Millisecond
	replicaDialTimeout = time.Second
	deadOwnerCooldown = time.Minute
	t.Cleanup(func() {
		replicaRPCTimeout, replicaDialTimeout, deadOwnerCooldown = oldRPC, oldDial, oldCooldown
	})

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

	stalledAddr := startEnrollServer(t, authority, func(ctx context.Context, _ *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	healthyAddr := startEnrollServer(t, authority, func(ctx context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
		return authority.Enroll(ctx, req)
	})
	redirectAddr := startEnrollServer(t, authority, func(_ context.Context, _ *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
		return nil, agentv1.LiveOwnerRedirect(stalledAddr)
	})

	app := &App{cfg: config.AgentConfig{
		Node: config.NodeConfig{ID: "agent-1"},
		ControlPlane: config.ControlPlaneClientConfig{
			Addresses: []string{redirectAddr, stalledAddr, healthyAddr},
			TLS: config.ClientTLSConfig{
				CAFile:         writeTrustBundleFile(t, authority),
				ServerName:     "localhost",
				BootstrapToken: "bootstrap-token",
			},
		},
		Runtime: config.RuntimeConfig{DataDir: t.TempDir()},
	}}

	start := time.Now()
	_, err = app.enrollClientCertificate(context.Background(), nil)
	if !errors.Is(err, errRedirectOwner) {
		t.Fatalf("first enroll = %v, want redirect", err)
	}
	if app.controlPlaneAddr != stalledAddr {
		t.Fatalf("pin = %q, want stalled owner %q", app.controlPlaneAddr, stalledAddr)
	}

	material, err := app.enrollClientCertificate(context.Background(), nil)
	if err != nil {
		t.Fatalf("enroll after dead-owner redirect: %v", err)
	}
	if material == nil || material.certificate.Certificate == nil {
		t.Fatal("expected enrolled certificate material")
	}
	if app.controlPlaneAddr != healthyAddr {
		t.Fatalf("pin after fallback = %q, want healthy %q", app.controlPlaneAddr, healthyAddr)
	}
	if app.ownerQuarantined(stalledAddr) != true {
		t.Fatal("expected stalled owner to be quarantined")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("enrollment walked too slowly: %s", time.Since(start))
	}
}

func TestEnrollRecoversLiveOwnerAfterQuarantineCooldown(t *testing.T) {
	oldRPC, oldDial, oldCooldown := replicaRPCTimeout, replicaDialTimeout, deadOwnerCooldown
	replicaRPCTimeout = 80 * time.Millisecond
	replicaDialTimeout = time.Second
	deadOwnerCooldown = 150 * time.Millisecond
	t.Cleanup(func() {
		replicaRPCTimeout, replicaDialTimeout, deadOwnerCooldown = oldRPC, oldDial, oldCooldown
	})

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

	var ownerCalls atomic.Int32
	var ownerAddr string
	ownerAddr = startEnrollServer(t, authority, func(ctx context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
		if ownerCalls.Add(1) == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return authority.Enroll(ctx, req)
	})
	standbyAddr := startEnrollServer(t, authority, func(_ context.Context, _ *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
		return nil, agentv1.LiveOwnerRedirect(ownerAddr)
	})

	app := &App{cfg: config.AgentConfig{
		Node: config.NodeConfig{ID: "agent-1"},
		ControlPlane: config.ControlPlaneClientConfig{
			Addresses: []string{ownerAddr, standbyAddr},
			TLS: config.ClientTLSConfig{
				CAFile:         writeTrustBundleFile(t, authority),
				ServerName:     "localhost",
				BootstrapToken: "bootstrap-token",
			},
		},
		Runtime: config.RuntimeConfig{DataDir: t.TempDir()},
	}}

	deadline := time.Now().Add(time.Second)
	var material *clientTLSMaterial
	for time.Now().Before(deadline) {
		material, err = app.enrollClientCertificate(context.Background(), nil)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("did not recover live owner after cooldown: %v", err)
	}
	if material == nil || material.certificate.Certificate == nil {
		t.Fatal("expected enrolled certificate material")
	}
	if app.controlPlaneAddr != ownerAddr {
		t.Fatalf("pin after recovery = %q, want %q", app.controlPlaneAddr, ownerAddr)
	}
}

func startEnrollServer(t *testing.T, authority *identity.TLSAuthority, enroll func(context.Context, *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error)) string {
	t.Helper()
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(authority.HTTPConfig())))
	agentv1.RegisterAgentControlServer(server, &enrollFuncServer{enroll: enroll})
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

func writeTrustBundleFile(t *testing.T, authority *identity.TLSAuthority) string {
	t.Helper()
	bundle, err := authority.TrustBundle(context.Background())
	if err != nil {
		t.Fatalf("TrustBundle: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, bundle, 0o644); err != nil {
		t.Fatalf("write trust bundle: %v", err)
	}
	return path
}
