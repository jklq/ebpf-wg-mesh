package controlplane

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

func TestCertificateRevocationsReloadsHexSerials(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "revoked-serials.txt")
	revocations, err := NewCertificateRevocations(path)
	if err != nil {
		t.Fatalf("NewCertificateRevocations: %v", err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(0xabcd)}
	if err := revocations.Check(cert); err != nil {
		t.Fatalf("unexpected initial revocation check: %v", err)
	}
	if err := os.WriteFile(path, []byte("# compromised agent\nAB:CD # openssl output\n"), 0o600); err != nil {
		t.Fatalf("write revocation file: %v", err)
	}
	if err := revocations.Check(cert); !errors.Is(err, errClientCertificateRevoked) {
		t.Fatalf("expected live revocation, got %v", err)
	}
	if err := os.WriteFile(path, []byte("not-a-serial\n"), 0o600); err != nil {
		t.Fatalf("write malformed revocation file: %v", err)
	}
	if err := revocations.Check(cert); err == nil || errors.Is(err, errClientCertificateRevoked) {
		t.Fatalf("expected malformed denylist to fail closed, got %v", err)
	}
}

func TestCertificateRevocationsAddIsIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "revoked-serials.txt")
	revocations, err := NewCertificateRevocations(path)
	if err != nil {
		t.Fatalf("NewCertificateRevocations: %v", err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(0xabcd)}
	if err := revocations.Add("AB:CD", "abcd"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := revocations.Check(cert); !errors.Is(err, errClientCertificateRevoked) {
		t.Fatalf("expected added serial to be revoked, got %v", err)
	}
}

func TestInternalAuthRejectsRevokedCertificatesForEveryCallerClass(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "revoked-serials.txt")
	if err := os.WriteFile(path, []byte("2a\n"), 0o600); err != nil {
		t.Fatalf("write revocation file: %v", err)
	}
	revocations, err := NewCertificateRevocations(path)
	if err != nil {
		t.Fatalf("NewCertificateRevocations: %v", err)
	}
	authz := NewInternalAuth("dashboard-1", testUserAssertionSecret, revocations)
	tests := []struct {
		class  serviceCallerClass
		id     string
		method string
	}{
		{class: serviceCallerAgent, id: "agent-1", method: "/agent.v1.AgentControl/Enroll"},
		{class: serviceCallerAgent, id: "agent-1", method: "/agent.v1.AgentControl/Sync"},
		{class: serviceCallerBuilder, id: "builder-1", method: "/platform.v1.BuilderService/ClaimBuild"},
		{class: serviceCallerDashboard, id: "dashboard-1", method: "/platform.v1.OpsService/IngestGitHubWebhook"},
	}
	for _, test := range tests {
		test := test
		t.Run(string(test.class)+test.method, func(t *testing.T) {
			ctx := contextWithCertificate(test.class, test.id, big.NewInt(0x2a))
			_, err := authz.authorizeGRPCContext(ctx, test.method)
			if status.Code(err) != codes.Unauthenticated {
				t.Fatalf("expected revoked %s certificate to be rejected, got %v", test.class, err)
			}
		})
	}
}

func TestTLSHandshakeRejectsRevokedCertificateButAllowsCertificateFreeBootstrap(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	revocationPath := filepath.Join(stateDir, "revoked.txt")
	authority, err := NewTLSAuthority(config.ControlPlaneConfig{
		StateDir: stateDir,
		InternalGRPC: config.ListenerConfig{TLS: config.ServerTLSConfig{
			ServerNames:                  []string{"controlplane"},
			ServerCertValidityHours:      24,
			ClientCertValidityHours:      6,
			RevokedClientCertSerialsFile: revocationPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewTLSAuthority: %v", err)
	}
	material, err := authority.EnsureClientIdentity(serviceCallerAgent, "node-1")
	if err != nil {
		t.Fatalf("EnsureClientIdentity: %v", err)
	}
	pair, err := tls.X509KeyPair(material.CertPEM, material.KeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	if err := os.WriteFile(revocationPath, []byte(leaf.SerialNumber.Text(16)+"\n"), 0o600); err != nil {
		t.Fatalf("write revocation file: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(material.CAPEM) {
		t.Fatal("append CA")
	}
	serverCredentials := credentials.NewTLS(authority.HTTPConfig())

	clientWithRevokedCert := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      roots,
		ServerName:   "controlplane",
		MinVersion:   tls.VersionTLS13,
	})
	serverErr, clientErr := runTLSHandshake(t, serverCredentials, clientWithRevokedCert)
	if serverErr == nil && clientErr == nil {
		t.Fatal("expected revoked client certificate handshake to fail")
	}

	certificateFreeClient := credentials.NewTLS(&tls.Config{
		RootCAs:    roots,
		ServerName: "controlplane",
		MinVersion: tls.VersionTLS13,
	})
	serverErr, clientErr = runTLSHandshake(t, serverCredentials, certificateFreeClient)
	if serverErr != nil || clientErr != nil {
		t.Fatalf("expected certificate-free bootstrap handshake to pass, server=%v client=%v", serverErr, clientErr)
	}
}

func runTLSHandshake(t *testing.T, serverCredentials, clientCredentials credentials.TransportCredentials) (error, error) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	deadline := time.Now().Add(2 * time.Second)
	if err := serverConn.SetDeadline(deadline); err != nil {
		t.Fatalf("set server deadline: %v", err)
	}
	if err := clientConn.SetDeadline(deadline); err != nil {
		t.Fatalf("set client deadline: %v", err)
	}
	serverResult := make(chan error, 1)
	go func() {
		_, _, err := serverCredentials.ServerHandshake(serverConn)
		serverResult <- err
	}()
	_, _, clientErr := clientCredentials.ClientHandshake(context.Background(), "controlplane", clientConn)
	return <-serverResult, clientErr
}

func contextWithCertificate(class serviceCallerClass, id string, serial *big.Int) context.Context {
	cert := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:         id,
			OrganizationalUnit: []string{string(class)},
		},
	}
	return context.WithValue(
		context.Background(),
		verifiedClientCertificateContextKey{},
		cert,
	)
}
