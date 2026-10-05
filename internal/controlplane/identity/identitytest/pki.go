// Package identitytest provides real mTLS credentials for transport tests.
package identitytest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/signkeys/signkeystest"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

type PKI struct {
	Authority *identity.TLSAuthority
	Keys      *signkeystest.Fake
}

func New(t *testing.T) *PKI {
	t.Helper()
	keys := signkeystest.New(t)
	authority, err := identity.NewTLSAuthority(context.Background(), config.ControlPlaneConfig{
		StateDir: t.TempDir(), InternalGRPC: config.ListenerConfig{TLS: config.ServerTLSConfig{ServerNames: []string{"controlplane"}}},
	}, keys)
	if err != nil {
		t.Fatal(err)
	}
	return &PKI{Authority: authority, Keys: keys}
}

func (p *PKI) Material(t *testing.T, class identity.CallerClass, id string) identity.ClientIdentityMaterial {
	t.Helper()
	m, err := identity.IssueClientCertificate(context.Background(), p.Keys, class, id, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (p *PKI) Client(t *testing.T, class identity.CallerClass, id string) *tls.Config {
	t.Helper()
	m := p.Material(t, class, id)
	cert, err := tls.X509KeyPair(m.CertPEM, m.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(m.CAPEM)
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: "controlplane", MinVersion: tls.VersionTLS13}
}

func Context(t *testing.T, ctx context.Context, nodeID string) context.Context {
	t.Helper()
	cfg := New(t).Client(t, identity.CallerIngress, nodeID)
	cert, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return peer.NewContext(ctx, &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}}})
}
