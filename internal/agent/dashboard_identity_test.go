package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane"

	"google.golang.org/grpc"
)

type testDashboardCertificateIssuer struct {
	authority *controlplane.TLSAuthority
	calls     int
	request   *agentv1.ManagedDashboardCertificateRequest
	err       error
}

func (i *testDashboardCertificateIssuer) IssueManagedDashboardCertificate(_ context.Context, req *agentv1.ManagedDashboardCertificateRequest, _ ...grpc.CallOption) (*agentv1.EnrollResponse, error) {
	i.calls++
	i.request = req
	if i.err != nil {
		return nil, i.err
	}
	return i.authority.IssueManagedDashboardCertificate("dashboard-1", req.GetCsrPem())
}

func TestManagedDashboardIdentityIsCreatedLocallyAndReused(t *testing.T) {
	t.Parallel()

	secretsDir := t.TempDir()
	authority, err := controlplane.NewTLSAuthority(config.ControlPlaneConfig{
		StateDir: t.TempDir(),
		InternalGRPC: config.ListenerConfig{TLS: config.ServerTLSConfig{
			ServerNames:             []string{"controlplane"},
			ServerCertValidityHours: 24,
			ClientCertValidityHours: 6,
		}},
	})
	if err != nil {
		t.Fatalf("NewTLSAuthority: %v", err)
	}
	issuer := &testDashboardCertificateIssuer{authority: authority}
	app := &App{cfg: config.AgentConfig{
		Node: config.NodeConfig{ID: "agent-trusted"},
		ControlPlane: config.ControlPlaneClientConfig{TLS: config.ClientTLSConfig{
			RenewBeforeMinutes: 30,
		}},
		Runtime: config.RuntimeConfig{ManagedDashboardSecretsDir: secretsDir},
	}}
	state := managedDashboardDesiredState()

	changed, err := app.ensureManagedDashboardIdentity(context.Background(), issuer, state)
	if err != nil {
		t.Fatalf("ensureManagedDashboardIdentity(first): %v", err)
	}
	if !changed || issuer.calls != 1 || issuer.request.GetAgentId() != "agent-trusted" {
		t.Fatalf("expected one local enrollment, changed=%t calls=%d request=%+v", changed, issuer.calls, issuer.request)
	}

	keyPath := filepath.Join(secretsDir, managedDashboardKeyFileName)
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read local dashboard key: %v", err)
	}
	if info, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stat local dashboard key: %v", err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("dashboard key mode = %o, want 600", got)
	}
	certPEM, err := os.ReadFile(filepath.Join(secretsDir, managedDashboardCertFileName))
	if err != nil {
		t.Fatalf("read dashboard certificate: %v", err)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatalf("dashboard certificate does not match local key: %v", err)
	}

	changed, err = app.ensureManagedDashboardIdentity(context.Background(), issuer, state)
	if err != nil {
		t.Fatalf("ensureManagedDashboardIdentity(second): %v", err)
	}
	if changed || issuer.calls != 1 {
		t.Fatalf("expected valid identity reuse, changed=%t calls=%d", changed, issuer.calls)
	}
}

func TestManagedDashboardIdentityKeepsValidCertificateWhenRenewalFails(t *testing.T) {
	t.Parallel()

	secretsDir := t.TempDir()
	authority, err := controlplane.NewTLSAuthority(config.ControlPlaneConfig{
		StateDir: t.TempDir(),
		InternalGRPC: config.ListenerConfig{TLS: config.ServerTLSConfig{
			ServerNames:             []string{"controlplane"},
			ServerCertValidityHours: 24,
			ClientCertValidityHours: 1,
		}},
	})
	if err != nil {
		t.Fatalf("NewTLSAuthority: %v", err)
	}
	issuer := &testDashboardCertificateIssuer{authority: authority}
	app := &App{cfg: config.AgentConfig{
		Node: config.NodeConfig{ID: "agent-trusted"},
		ControlPlane: config.ControlPlaneClientConfig{TLS: config.ClientTLSConfig{
			RenewBeforeMinutes: 60,
		}},
		Runtime: config.RuntimeConfig{ManagedDashboardSecretsDir: secretsDir},
	}}
	state := managedDashboardDesiredState()
	if changed, err := app.ensureManagedDashboardIdentity(context.Background(), issuer, state); err != nil || !changed {
		t.Fatalf("initial dashboard enrollment: changed=%t err=%v", changed, err)
	}

	issuer.err = errors.New("temporary control-plane failure")
	changed, err := app.ensureManagedDashboardIdentity(context.Background(), issuer, state)
	if err != nil {
		t.Fatalf("expected valid certificate fallback, got %v", err)
	}
	if changed || issuer.calls != 2 {
		t.Fatalf("expected failed renewal to retain existing identity, changed=%t calls=%d", changed, issuer.calls)
	}
}

func TestCertificateNeedsRenewalUsesBoundedWindow(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	cert := &x509.Certificate{NotBefore: now.Add(-40 * time.Minute), NotAfter: now.Add(20 * time.Minute)}
	if !certificateNeedsRenewal(cert, now, 30*time.Minute) {
		t.Fatal("expected certificate inside configured renewal window to renew")
	}
	if certificateNeedsRenewal(cert, now, 5*time.Minute) {
		t.Fatal("did not expect certificate outside short renewal window to renew")
	}
}

func TestContainerdRuntimeRestartsOnlyManagedDashboard(t *testing.T) {
	t.Parallel()

	engine := &fakeEngine{status: map[string]serviceStatus{}, created: map[string]bool{}}
	runtime := &ContainerdRuntime{engine: engine}
	state := managedDashboardDesiredState()
	state.Services = append(state.Services, &agentv1.DesiredService{AllocationId: "user-allocation"})

	if err := runtime.RestartManagedDashboard(context.Background(), state); err != nil {
		t.Fatalf("RestartManagedDashboard: %v", err)
	}
	if len(engine.removed) != 1 || engine.removed[0] != "dashboard-allocation" {
		t.Fatalf("unexpected restarted allocations %#v", engine.removed)
	}
}

func managedDashboardDesiredState() *agentv1.DesiredNodeState {
	return &agentv1.DesiredNodeState{Services: []*agentv1.DesiredService{{
		AllocationId: "dashboard-allocation",
		Spec: &platformv1.ResolvedServiceSpec{Runtime: &platformv1.ServiceRuntime{Env: map[string]string{
			managedDashboardSecretMarker: managedDashboardSecretValue,
		}}},
	}}}
}
