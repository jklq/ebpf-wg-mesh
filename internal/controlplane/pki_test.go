package controlplane

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"

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
	authority, err := NewTLSAuthority(cfg)
	if err != nil {
		t.Fatalf("NewTLSAuthority: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cfg.StateDir, pkiDirName, caCertFileName)); err != nil {
		t.Fatalf("stat ca cert: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, pkiDirName, serverCertFileName)); err != nil {
		t.Fatalf("stat server cert: %v", err)
	}

	csrPEM := mustCreateCSR(t, "agent-1")
	resp, err := authority.Enroll(&agentv1.EnrollRequest{
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

	_, err = authority.Enroll(&agentv1.EnrollRequest{AgentId: "agent-2", CsrPem: string(csrPEM)})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for mismatched CSR, got %s", got)
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
