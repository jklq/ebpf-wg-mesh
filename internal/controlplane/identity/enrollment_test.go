package identity

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
)

type fakeEnrollmentStore struct {
	consumeCalls int
	lastKey      []byte
	consumeErr   error
}

func (s *fakeEnrollmentStore) AuthorizeAgentCredential(context.Context, string) error {
	return nil
}

func (s *fakeEnrollmentStore) ConsumeOrRecoverAgentBootstrapToken(_ context.Context, _, _ string, publicKeySHA256 []byte) error {
	s.consumeCalls++
	s.lastKey = append([]byte(nil), publicKeySHA256...)
	return s.consumeErr
}

func (s *fakeEnrollmentStore) RecordAgentCertificate(context.Context, string, string) error {
	return nil
}

func TestEnrollAgentBindsBootstrapTokenToCSRPublicKey(t *testing.T) {
	t.Parallel()

	authority, err := NewTLSAuthority(config.ControlPlaneConfig{
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
	store := &fakeEnrollmentStore{}
	enrollment := NewEnrollment(store, authority)
	csrPEM := mustCreateCSR(t, "agent-1")
	resp, err := enrollment.EnrollAgent(context.Background(), &agentv1.EnrollRequest{
		AgentId:        "agent-1",
		CsrPem:         string(csrPEM),
		BootstrapToken: "token",
	})
	if err != nil {
		t.Fatalf("EnrollAgent: %v", err)
	}
	if resp.GetCertPem() == "" {
		t.Fatal("expected issued certificate")
	}
	if store.consumeCalls != 1 || len(store.lastKey) != 32 {
		t.Fatalf("consume calls=%d key=%d", store.consumeCalls, len(store.lastKey))
	}

	store.consumeErr = errors.New("already consumed")
	_, err = enrollment.EnrollAgent(context.Background(), &agentv1.EnrollRequest{
		AgentId:        "agent-1",
		CsrPem:         string(csrPEM),
		BootstrapToken: "token",
	})
	if err == nil {
		t.Fatal("expected consume failure to reject enrollment")
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
