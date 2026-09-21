package identity

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/signkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys/signkeystest"

	"google.golang.org/grpc/credentials"
)

// TestTLSAuthorityRotationKeepsIssuanceAndVerification walks a CA rotation:
// issuance signs with the active key, verification accepts both keys through
// the overlap, and the retiring key stops verifying after finish while the
// server leaf flips to the new CA without a restart.
func TestTLSAuthorityRotationKeepsIssuanceAndVerification(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	keys := signkeystest.New(t)
	authority, err := NewTLSAuthority(ctx, config.ControlPlaneConfig{
		StateDir: t.TempDir(),
		InternalGRPC: config.ListenerConfig{TLS: config.ServerTLSConfig{
			ServerNames:             []string{"controlplane"},
			ServerCertValidityHours: 24,
			ClientCertValidityHours: 24,
		}},
	}, keys)
	if err != nil {
		t.Fatalf("NewTLSAuthority: %v", err)
	}
	enrolled := func(agentID string) *x509.Certificate {
		t.Helper()
		resp, err := authority.Enroll(ctx, &agentv1.EnrollRequest{
			AgentId: agentID,
			CsrPem:  string(mustCreateCSR(t, agentID)),
		})
		if err != nil {
			t.Fatalf("Enroll: %v", err)
		}
		return mustParseLeaf(t, resp.GetCertPem())
	}

	before := enrolled("agent-1")
	clusterBefore, err := authority.ClusterIdentity(ctx)
	if err != nil {
		t.Fatalf("ClusterIdentity: %v", err)
	}
	if ok, err := authority.VerifyClusterID(ctx, clusterBefore); err != nil || !ok {
		t.Fatalf("VerifyClusterID(before) = %v, %v", ok, err)
	}

	keys.Rotate(t, signkeys.ScopeInternalCA)

	after := enrolled("agent-1")
	clusterAfter, err := authority.ClusterIdentity(ctx)
	if err != nil {
		t.Fatalf("ClusterIdentity: %v", err)
	}
	if clusterAfter == clusterBefore {
		t.Fatal("cluster identity did not flip at rotate-start")
	}
	for _, id := range []string{clusterBefore, clusterAfter} {
		if ok, err := authority.VerifyClusterID(ctx, id); err != nil || !ok {
			t.Fatalf("VerifyClusterID(%s) = %v, %v", id, ok, err)
		}
	}
	if ok, err := authority.VerifyClusterID(ctx, "bogus"); err != nil || ok {
		t.Fatalf("VerifyClusterID(bogus) = %v, %v", ok, err)
	}

	// Both certificates verify against the overlap bundle, and a live TLS
	// handshake accepts both.
	bundle, err := authority.TrustBundle(ctx)
	if err != nil {
		t.Fatalf("TrustBundle: %v", err)
	}
	if got := countCertificates(t, bundle); got != 2 {
		t.Fatalf("overlap bundle holds %d certificates, want 2", got)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle) {
		t.Fatal("append overlap bundle")
	}
	for name, leaf := range map[string]*x509.Certificate{"before": before, "after": after} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
			t.Fatalf("overlap verify (%s): %v", name, err)
		}
	}
	mats, err := keys.Verifying(ctx, signkeys.ScopeInternalCA)
	if err != nil {
		t.Fatalf("Verifying: %v", err)
	}
	if len(mats) != 2 {
		t.Fatalf("verifying set holds %d keys, want 2", len(mats))
	}
	active, retiring := mats[0], mats[1]
	if err := after.CheckSignatureFrom(active.Cert); err != nil {
		t.Fatalf("post-rotation certificate is not signed by the active CA: %v", err)
	}
	if err := before.CheckSignatureFrom(retiring.Cert); err != nil {
		t.Fatalf("pre-rotation certificate is not signed by the retiring CA: %v", err)
	}

	// The server leaf still chains to the retiring CA through the overlap.
	if err := authority.RefreshServerCertificate(ctx); err != nil {
		t.Fatalf("RefreshServerCertificate: %v", err)
	}
	leaf := authority.currentServerLeaf(t)
	if err := leaf.CheckSignatureFrom(retiring.Cert); err != nil {
		t.Fatalf("overlap server leaf is not signed by the retiring CA: %v", err)
	}

	keys.Finish(t, signkeys.ScopeInternalCA)

	if ok, err := authority.VerifyClusterID(ctx, clusterBefore); err != nil || ok {
		t.Fatalf("VerifyClusterID(retired) = %v, %v", ok, err)
	}
	finished, err := authority.TrustBundle(ctx)
	if err != nil {
		t.Fatalf("TrustBundle: %v", err)
	}
	if got := countCertificates(t, finished); got != 1 {
		t.Fatalf("finished bundle holds %d certificates, want 1", got)
	}
	// A replica that restarts (or ticks) after finish flips its leaf to the
	// new CA on its own.
	if err := authority.RefreshServerCertificate(ctx); err != nil {
		t.Fatalf("RefreshServerCertificate: %v", err)
	}
	flipped := authority.currentServerLeaf(t)
	if err := flipped.CheckSignatureFrom(active.Cert); err != nil {
		t.Fatalf("post-finish server leaf is not signed by the active CA: %v", err)
	}
}

func TestTLSAuthorityHandshakeAcceptsBothGenerationsThroughOverlap(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	keys := signkeystest.New(t)
	authority, err := NewTLSAuthority(ctx, config.ControlPlaneConfig{
		StateDir: t.TempDir(),
		InternalGRPC: config.ListenerConfig{TLS: config.ServerTLSConfig{
			ServerNames:             []string{"controlplane"},
			ServerCertValidityHours: 24,
			ClientCertValidityHours: 24,
		}},
	}, keys)
	if err != nil {
		t.Fatalf("NewTLSAuthority: %v", err)
	}
	materialBefore, err := authority.EnsureClientIdentity(ctx, CallerAgent, "node-1")
	if err != nil {
		t.Fatalf("EnsureClientIdentity: %v", err)
	}
	pairBefore, err := tls.X509KeyPair(materialBefore.CertPEM, materialBefore.KeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	keys.Rotate(t, signkeys.ScopeInternalCA)
	// The cached pre-rotation identity still verifies (both keys verify),
	// so the cache is kept; mint the post-rotation identity fresh.
	materialAfter, err := IssueClientCertificate(ctx, keys, CallerAgent, "node-1", 24*time.Hour)
	if err != nil {
		t.Fatalf("IssueClientCertificate: %v", err)
	}
	pairAfter, err := tls.X509KeyPair(materialAfter.CertPEM, materialAfter.KeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(materialAfter.CAPEM) {
		t.Fatal("append overlap bundle")
	}
	serverCredentials := credentials.NewTLS(authority.HTTPConfig())
	for name, pair := range map[string]tls.Certificate{"before": pairBefore, "after": pairAfter} {
		client := credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{pair},
			RootCAs:      roots,
			ServerName:   "controlplane",
			MinVersion:   tls.VersionTLS13,
		})
		if serverErr, clientErr := runTLSHandshake(t, serverCredentials, client); serverErr != nil || clientErr != nil {
			t.Fatalf("overlap handshake (%s): server=%v client=%v", name, serverErr, clientErr)
		}
	}
}

func TestTLSAuthorityHandshakeRejectsRetiredGenerationAfterFinish(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	keys := signkeystest.New(t)
	authority, err := NewTLSAuthority(ctx, config.ControlPlaneConfig{
		StateDir: t.TempDir(),
		InternalGRPC: config.ListenerConfig{TLS: config.ServerTLSConfig{
			ServerNames:             []string{"controlplane"},
			ServerCertValidityHours: 24,
			ClientCertValidityHours: 24,
		}},
	}, keys)
	if err != nil {
		t.Fatalf("NewTLSAuthority: %v", err)
	}
	materialBefore, err := authority.EnsureClientIdentity(ctx, CallerAgent, "node-1")
	if err != nil {
		t.Fatalf("EnsureClientIdentity: %v", err)
	}
	pairBefore, err := tls.X509KeyPair(materialBefore.CertPEM, materialBefore.KeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	keys.Rotate(t, signkeys.ScopeInternalCA)
	keys.Finish(t, signkeys.ScopeInternalCA)
	if err := authority.RefreshServerCertificate(ctx); err != nil {
		t.Fatalf("RefreshServerCertificate: %v", err)
	}
	bundle, err := authority.TrustBundle(ctx)
	if err != nil {
		t.Fatalf("TrustBundle: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundle) {
		t.Fatal("append trust bundle")
	}
	serverCredentials := credentials.NewTLS(authority.HTTPConfig())
	client := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{pairBefore},
		RootCAs:      roots,
		ServerName:   "controlplane",
		MinVersion:   tls.VersionTLS13,
	})
	serverErr, _ := runTLSHandshake(t, serverCredentials, client)
	// The server rejects the retired client certificate. (In TLS 1.3 the
	// client observes this as a failed connection, not a handshake error,
	// so integration tests assert the dial fails.)
	if serverErr == nil {
		t.Fatal("post-finish handshake accepted the retired generation")
	}
}

func mustParseLeaf(t *testing.T, certPEM string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatal("decode issued certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse issued certificate: %v", err)
	}
	return leaf
}

func countCertificates(t *testing.T, bundle []byte) int {
	t.Helper()
	count := 0
	rest := bundle
	for len(rest) > 0 {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			t.Fatal("decode trust bundle")
		}
		if block.Type == "CERTIFICATE" {
			count++
		}
	}
	return count
}

func (a *TLSAuthority) currentServerLeaf(t *testing.T) *x509.Certificate {
	t.Helper()
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.serverLeaf == nil {
		t.Fatal("server leaf is not loaded")
	}
	return a.serverLeaf
}
