package builder

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/health"
	"ebof-wg-mesh/internal/reconciliation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type availabilityBuilderServer struct {
	platformv1.UnimplementedBuilderServiceServer
	calls atomic.Int64
	fail  atomic.Bool
}

func (s *availabilityBuilderServer) ReportBuildHeartbeat(context.Context, *platformv1.BuilderHeartbeatRequest) (*emptypb.Empty, error) {
	s.calls.Add(1)
	if s.fail.Load() {
		return nil, status.Error(codes.Unavailable, "effect committed, response unavailable")
	}
	return &emptypb.Empty{}, nil
}

func availabilityServer(t *testing.T, tlsConfig *tls.Config) (*grpc.Server, *availabilityBuilderServer, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	handler := &availabilityBuilderServer{}
	platformv1.RegisterBuilderServiceServer(s, handler)
	go s.Serve(listener)
	t.Cleanup(s.Stop)
	return s, handler, listener.Addr().String()
}

func TestBuilderReplicaFailoverDoesNotReplaySubmittedEffectsAndPausedAdmissionRemainsVisible(t *testing.T) {
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "controlplane.test"}, DNSNames: []string{"controlplane.test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	for name, b := range map[string][]byte{"ca.crt": certPEM, "client.crt": certPEM, "client.key": keyPEM} {
		if err := os.WriteFile(dir+"/"+name, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert}
	first, a, addressA := availabilityServer(t, tlsConfig)
	_, b, addressB := availabilityServer(t, tlsConfig)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	healthAddress := listener.Addr().String()
	listener.Close()
	authority := reconciliation.Authority{InstallationID: "installation", Generation: "replacement", ClusterID: "cluster", Paused: true}
	data, _ := json.Marshal(authority)
	if err := os.WriteFile(dir+"/authority.json", data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.BuilderConfig{ID: "paused-builder", Profile: config.ProfileDevelopment, WorkDir: dir, AuthorityFile: dir + "/authority.json", Health: config.HealthConfig{Listen: healthAddress}, ControlPlane: config.BuilderControlPlaneConfig{Addresses: []string{addressA, addressB}}}
	cfg.ControlPlane.TLS = config.InternalClientTLSConfig{CAFile: dir + "/ca.crt", CertFile: dir + "/client.crt", KeyFile: dir + "/client.key", ServerName: "controlplane.test"}
	if err := config.FinalizeBuilder(&cfg); err != nil {
		t.Fatal(err)
	}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	a.fail.Store(true)
	b.fail.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = app.client.ReportBuildHeartbeat(ctx, &platformv1.BuilderHeartbeatRequest{})
	if status.Code(err) != codes.Unavailable || a.calls.Load()+b.calls.Load() != 1 {
		t.Fatalf("submitted effect replayed: %v, calls %d/%d", err, a.calls.Load(), b.calls.Load())
	}
	a.fail.Store(false)
	b.fail.Store(false)
	first.Stop()
	// Stop closes the server before the client's transport necessarily observes
	// its GOAWAY. Each explicit probe below is a new submission, and retry is
	// allowed only after inspecting that neither native handler received it.
	beforeFailover := a.calls.Load() + b.calls.Load()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := app.client.ReportBuildHeartbeat(ctx, &platformv1.BuilderHeartbeatRequest{}, grpc.WaitForReady(true))
		if err == nil {
			break
		}
		if status.Code(err) != codes.Unavailable || a.calls.Load()+b.calls.Load() != beforeFailover || time.Now().After(deadline) {
			t.Fatal("surviving replica", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if a.calls.Load()+b.calls.Load() != beforeFailover+1 || b.calls.Load() == 0 {
		t.Fatal("failover did not submit exactly once to the survivor")
	}
	before := a.calls.Load() + b.calls.Load()
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	for {
		response, err := http.Get("http://" + healthAddress + "/admissionz")
		if err == nil {
			var observed health.Report
			decode := json.NewDecoder(response.Body).Decode(&observed)
			response.Body.Close()
			if decode != nil || response.StatusCode != 200 || observed.Authority == nil || *observed.Authority != authority {
				t.Fatalf("paused admission %v %+v", decode, observed)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("paused builder has no admission listener")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if a.calls.Load()+b.calls.Load() != before {
		t.Fatal("paused builder submitted background work")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
