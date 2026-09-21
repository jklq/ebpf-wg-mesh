package controlplane

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/signkeys/signkeystest"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestTLSAuthorityEnrollsAgentCertificates(t *testing.T) {
	t.Parallel()

	cfg := config.ControlPlaneConfig{
		StateDir: t.TempDir(),
		InternalGRPC: config.ListenerConfig{
			TLS: config.ServerTLSConfig{
				ServerNames:             []string{"controlplane", "localhost"},
				BootstrapTokens:         []config.AgentBootstrapToken{{AgentID: "agent-1", Token: "bootstrap-token"}},
				ServerCertValidityHours: 24,
				ClientCertValidityHours: 6,
			},
		},
	}
	authority, err := NewTLSAuthority(context.Background(), cfg, signkeystest.New(t))
	if err != nil {
		t.Fatalf("NewTLSAuthority: %v", err)
	}

	// The CA is shared key state, not a file: only the replica-local
	// server leaf lives under the state directory.
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "pki", "ca.crt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file-based CA must be gone, stat: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "pki", "server.crt")); err != nil {
		t.Fatalf("stat server cert: %v", err)
	}

	csrPEM := mustCreateCSR(t, "agent-1")
	resp, err := authority.Enroll(context.Background(), &agentv1.EnrollRequest{
		AgentId:        "agent-1",
		CsrPem:         string(csrPEM),
		BootstrapToken: "bootstrap-token",
	})
	if err != nil {
		t.Fatalf("Enroll(valid): %v", err)
	}
	if resp.GetCertPem() == "" || resp.GetCaPem() == "" || resp.GetNotAfter() == nil {
		t.Fatal("expected cert, ca, and not_after in enroll response")
	}
	cert := mustParseCertificate(t, resp.GetCertPem())
	if cert.Subject.CommonName != "agent-1" {
		t.Fatalf("unexpected enrolled common name %q", cert.Subject.CommonName)
	}

	_, err = authority.Enroll(context.Background(), &agentv1.EnrollRequest{AgentId: "agent-2", CsrPem: string(csrPEM)})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for mismatched CSR, got %s", got)
	}
}

func TestAgentServiceIssuesManagedDashboardCertificateOnlyToTrustedAgent(t *testing.T) {
	t.Parallel()

	authority, err := NewTLSAuthority(context.Background(), config.ControlPlaneConfig{
		StateDir: t.TempDir(),
		InternalGRPC: config.ListenerConfig{TLS: config.ServerTLSConfig{
			ServerNames:             []string{"controlplane"},
			ServerCertValidityHours: 24,
			ClientCertValidityHours: 6,
		}},
	}, signkeystest.New(t))
	if err != nil {
		t.Fatalf("NewTLSAuthority: %v", err)
	}
	service := NewAgentService(nil, nil, nil, nil, authority, nil, true, "agent-trusted", "dashboard-1", WithReplicaAddresses([]string{" replica-a:9443", "replica-b:9443", "replica-a:9443"}))
	req := &agentv1.ManagedDashboardCertificateRequest{
		AgentId: "agent-trusted",
		CsrPem:  string(mustCreateCSR(t, "locally-generated")),
	}

	resp, err := service.IssueManagedDashboardCertificate(
		contextWithClientIdentity(serviceCallerAgent, "agent-trusted"),
		req,
	)
	if err != nil {
		t.Fatalf("IssueManagedDashboardCertificate: %v", err)
	}
	if got, want := strings.Join(resp.GetReplicaAddresses(), ","), "replica-a:9443,replica-b:9443"; got != want {
		t.Fatalf("replica addresses = %q, want %q", got, want)
	}
	cert := mustParseCertificate(t, resp.GetCertPem())
	if cert.Subject.CommonName != "dashboard-1" {
		t.Fatalf("unexpected dashboard common name %q", cert.Subject.CommonName)
	}
	if len(cert.Subject.OrganizationalUnit) != 1 || cert.Subject.OrganizationalUnit[0] != string(serviceCallerDashboard) {
		t.Fatalf("unexpected dashboard organizational unit %#v", cert.Subject.OrganizationalUnit)
	}
	if _, err := os.Stat(filepath.Join(authority.PKIDir(), "clients")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dashboard issuance persisted client key material on the control plane: %v", err)
	}

	_, err = service.IssueManagedDashboardCertificate(
		contextWithClientIdentity(serviceCallerAgent, "agent-other"),
		&agentv1.ManagedDashboardCertificateRequest{AgentId: "agent-other", CsrPem: req.GetCsrPem()},
	)
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for untrusted agent, got %s", got)
	}
}

func mustCreateCSR(t *testing.T, commonName string) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: commonName},
	}, key)
	if err != nil {
		t.Fatalf("CreateCertificateRequest: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
}

func mustParseCertificate(t *testing.T, certPEM string) *x509.Certificate {
	t.Helper()

	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatal("Decode certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return cert
}
