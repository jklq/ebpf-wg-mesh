//go:build integration

package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/source"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/builder"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/localteststack"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

const systemTestDashboardID = "dashboard-test"

type systemControlPlane struct {
	cfg           config.ControlPlaneConfig
	server        *Server
	cancel        context.CancelFunc
	dashboard     platformv1.PlatformServiceClient
	dashboardConn *grpc.ClientConn
	registry      *localteststack.ManagedRegistry
	stopped       bool
}

func startSystemControlPlane(t *testing.T, opts systemControlPlaneOptions) *systemControlPlane {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stateDir := t.TempDir()
	bootstrapTokens := opts.bootstrapTokens
	if len(bootstrapTokens) == 0 {
		bootstrapTokens = []config.AgentBootstrapToken{{AgentID: "system-test-agent", Token: "system-test-bootstrap"}}
	}
	cfg := config.ControlPlaneConfig{
		Profile: config.ProfileDevelopment,
		InternalGRPC: config.ListenerConfig{
			Listen: "127.0.0.1:0",
			TLS: config.ServerTLSConfig{
				ServerNames:             []string{"localhost"},
				BootstrapTokens:         bootstrapTokens,
				ServerCertValidityHours: 24,
				ClientCertValidityHours: 24,
			},
		},
		UserAssertions: config.UserAssertionConfig{HMACSecret: testUserAssertionSecret},
		Database: config.DatabaseConfig{
			URL:          deliverycore.FirstNonEmpty(opts.databaseURL, createTestDatabase(t)),
			MaxOpenConns: 4,
			MaxIdleConns: 4,
		},
		Logs:      config.LogCaptureConfig{ClickHouse: config.ClickHouseConfig{URL: opts.clickhouseURL}},
		StateDir:  deliverycore.FirstNonEmpty(opts.stateDir, stateDir),
		Ingress:   config.IngressConfig{PublicAddr: "platform.local", AdminURL: deliverycore.FirstNonEmpty(opts.ingressAdminURL, "http://127.0.0.1:9/load")},
		Dashboard: config.ManagedDashboardConfig{ServiceCallerID: systemTestDashboardID},
		Bootstrap: opts.bootstrap,
		Registry:  opts.registry,
		Mesh:      testMeshConfig(),
	}
	if err := config.FinalizeControlPlane(&cfg); err != nil {
		cancel()
		t.Fatalf("FinalizeControlPlane: %v", err)
	}
	server, err := NewServer(ctx, cfg)
	if err != nil {
		cancel()
		t.Fatalf("NewServer: %v", err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- server.Run(ctx) }()
	harness := &systemControlPlane{cfg: cfg, server: server, cancel: cancel}
	t.Cleanup(func() {
		harness.stop()
		select {
		case err := <-runErr:
			if err != nil && !strings.Contains(err.Error(), "closed") {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("timeout waiting for server.Run to exit")
		}
	})
	waitForListener(t, server.InternalAddr())
	if !opts.standby {
		waitForSingletonLease(t, server)
	}

	if opts.withDashboard {
		identity, err := server.EnsureDashboardClientIdentity(systemTestDashboardID)
		if err != nil {
			t.Fatalf("EnsureDashboardClientIdentity: %v", err)
		}
		conn := newDashboardPlatformClientConn(t, server.InternalAddr(), identity)
		t.Cleanup(func() { _ = conn.Close() })
		harness.dashboardConn = conn
		harness.dashboard = platformv1.NewPlatformServiceClient(conn)
	}
	if opts.registry.Host != "" {
		registry, err := localteststack.StartManagedRegistry(ctx, localteststack.LocalRegistryConfig{
			StateDir:       filepath.Join(t.TempDir(), "registry"),
			ContainerName:  fmt.Sprintf("sys-registry-%d", time.Now().UnixNano()),
			HostPort:       registryHostPort(t, opts.registry.Host),
			TokenRealm:     "http://" + server.RegistryAuthAddr() + RegistryTokenPath,
			TokenService:   cfg.Registry.TokenService,
			TokenIssuer:    cfg.Registry.TokenIssuer,
			RootCertBundle: server.RegistryAuthCertificatePath(),
		}, localteststack.ExecDockerRunner{})
		if err != nil {
			t.Fatalf("StartManagedRegistry: %v", err)
		}
		t.Cleanup(func() {
			if err := registry.Close(); err != nil {
				t.Errorf("close registry: %v", err)
			}
		})
		harness.registry = registry
	}
	return harness
}

func (h *systemControlPlane) stop() {
	if h == nil || h.stopped {
		return
	}
	h.stopped = true
	if h.cancel != nil {
		h.cancel()
	}
	if h.server != nil {
		_ = h.server.Close()
	}
}

type systemControlPlaneOptions struct {
	databaseURL     string
	stateDir        string
	clickhouseURL   string
	ingressAdminURL string
	bootstrap       config.BootstrapConfig
	bootstrapTokens []config.AgentBootstrapToken
	registry        config.RegistryConfig
	withDashboard   bool
	standby         bool
}

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := (localteststack.ExecDockerRunner{}).Run(context.Background(), "version", "--format", "{{.Server.Version}}"); err != nil {
		t.Skipf("docker is unavailable: %v", err)
	}
}

func startTestClickHouse(t *testing.T) string {
	t.Helper()
	requireDocker(t)
	port := availableLocalPort(t)
	managed, err := localteststack.StartManagedClickHouse(context.Background(), localteststack.LocalClickHouseConfig{
		ContainerName: fmt.Sprintf("sys-clickhouse-%d", time.Now().UnixNano()),
		NativePort:    port,
	}, localteststack.ExecDockerRunner{})
	if err != nil {
		t.Skipf("ClickHouse cannot be started: %v", err)
	}
	t.Cleanup(func() {
		if err := managed.Close(); err != nil {
			t.Errorf("close ClickHouse: %v", err)
		}
	})
	return managed.URL()
}

func startTestBuilder(t *testing.T, server *Server, builderID string) *builder.App {
	t.Helper()
	identity, err := server.EnsureBuilderClientIdentity(builderID)
	if err != nil {
		t.Fatalf("EnsureBuilderClientIdentity: %v", err)
	}
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	certPath := filepath.Join(dir, "builder.crt")
	keyPath := filepath.Join(dir, "builder.key")
	if err := os.WriteFile(caPath, identity.CAPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, identity.CertPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, identity.KeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	buildBinary := "docker"
	if _, err := exec.LookPath(buildBinary); err != nil {
		t.Skipf("docker is required to run the real builder: %v", err)
	}
	if err := exec.Command(buildBinary, "buildx", "version").Run(); err != nil {
		t.Skipf("docker buildx is required to run the real builder: %v", err)
	}
	cfg := config.BuilderConfig{
		Profile: config.ProfileDevelopment,
		ID:      builderID,
		Name:    builderID,
		ControlPlane: config.BuilderControlPlaneConfig{
			Address: server.InternalAddr(),
			TLS: config.InternalClientTLSConfig{
				CAFile:     caPath,
				CertFile:   certPath,
				KeyFile:    keyPath,
				ServerName: "localhost",
			},
		},
		WorkDir:                  filepath.Join(dir, "work"),
		PollIntervalSeconds:      1,
		HeartbeatIntervalSeconds: 5,
		BuildctlBinary:           buildBinary,
		BuildkitAddress:          "docker-buildx",
		CleanupWorkDir:           true,
	}
	if err := config.FinalizeBuilder(&cfg); err != nil {
		t.Fatalf("FinalizeBuilder: %v", err)
	}
	app, err := builder.New(cfg)
	if err != nil {
		t.Fatalf("builder.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- app.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = app.Close()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Error("timeout waiting for builder.App to exit")
		}
	})
	return app
}

func dialBuilderClient(t *testing.T, server *Server, builderID string) platformv1.BuilderServiceClient {
	t.Helper()
	identity, err := server.EnsureBuilderClientIdentity(builderID)
	if err != nil {
		t.Fatalf("EnsureBuilderClientIdentity: %v", err)
	}
	cert, err := tls.X509KeyPair(identity.CertPEM, identity.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(identity.CAPEM) {
		t.Fatal("builder CA")
	}
	dialCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, server.InternalAddr(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      roots,
		ServerName:   "localhost",
		MinVersion:   tls.VersionTLS13,
	})), grpc.WithBlock())
	if err != nil {
		t.Fatalf("dial builder: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return platformv1.NewBuilderServiceClient(conn)
}

func userContext(t *testing.T, ctx context.Context, userID string) context.Context {
	t.Helper()
	return metadata.AppendToOutgoingContext(ctx, userAssertionHeader, signedLiveUserAssertion(t, userID))
}

func dockerfileMarkerArchive(marker string) []byte {
	files := map[string]string{
		"repo/Dockerfile": "FROM scratch\nCOPY marker.txt /marker.txt\n",
		"repo/marker.txt": marker + "\n",
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			panic(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			panic(err)
		}
	}
	if err := tw.Close(); err != nil {
		panic(err)
	}
	if err := gz.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func seedDockerfileSourceState(t *testing.T, store *persistence, service deliverycore.ServiceRecord, commitSHA, marker string) {
	t.Helper()
	archive := dockerfileMarkerArchive(marker)
	digest, objectKey, err := store.source.StoreSourceArchive(context.Background(), archive)
	if err != nil {
		t.Fatalf("storeSourceArchive: %v", err)
	}
	if err := store.withProductTx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		binding, err := store.source.UpsertSourceBindingTx(context.Background(), tx, source.SourceBindingRecord{
			ServiceID:                    service.ID,
			ProjectID:                    service.ProjectID,
			Provider:                     "github",
			RepositorySelector:           "octocat/hello",
			TrackedRef:                   "main",
			ProviderRepositoryExternalID: "repo-1",
			AccessState:                  source.SourceAccessStateAvailable,
			BuildRecipe:                  &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
			ResolvedAt:                   time.Now().UTC(),
			FreshUntil:                   time.Now().UTC().Add(time.Hour),
		})
		if err != nil {
			return err
		}
		revision, err := store.source.UpsertSourceRevisionTx(context.Background(), tx, source.SourceRevisionRecord{
			SourceBindingID:              binding.ID,
			ServiceID:                    service.ID,
			Provider:                     binding.Provider,
			ProviderRepositoryExternalID: binding.ProviderRepositoryExternalID,
			TrackedRef:                   binding.TrackedRef,
			CommitSHA:                    commitSHA,
			CommitMessage:                "fixture " + commitSHA,
			CommitAuthor:                 "system-test",
			ObservedAt:                   time.Now().UTC(),
		})
		if err != nil {
			return err
		}
		_, err = store.source.UpsertSourceSnapshotTx(context.Background(), tx, source.SourceSnapshotRecord{
			SourceRevisionID:             revision.ID,
			Provider:                     binding.Provider,
			ProviderRepositoryExternalID: binding.ProviderRepositoryExternalID,
			CommitSHA:                    commitSHA,
			Digest:                       digest,
			ObjectKey:                    objectKey,
			ArchiveSizeBytes:             int64(len(archive)),
			Ready:                        true,
			FetchedAt:                    sql.NullTime{Time: time.Now().UTC(), Valid: true},
		})
		return err
	}); err != nil {
		t.Fatalf("seed dockerfile source state: %v", err)
	}
}

func availableLocalPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func registryHostPort(t *testing.T, host string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(host)
	if err != nil {
		t.Fatalf("registry host %q: %v", host, err)
	}
	var n int
	if _, err := fmt.Sscanf(port, "%d", &n); err != nil {
		t.Fatalf("registry port %q: %v", port, err)
	}
	return n
}

func localRegistryConfig(t *testing.T) config.RegistryConfig {
	t.Helper()
	port := availableLocalPort(t)
	host := fmt.Sprintf("localhost:%d", port)
	return config.RegistryConfig{
		Host:                 host,
		NamespacePrefix:      "mesh",
		AuthListen:           "127.0.0.1:0",
		TokenIssuer:          "system-publish-test",
		TokenService:         host,
		CredentialTTLSeconds: 300,
	}
}

func enrollAgentTLS(t *testing.T, server *Server, agentID, token string) tls.Certificate {
	t.Helper()
	identity, err := server.EnsureDashboardClientIdentity(systemTestDashboardID)
	if err != nil {
		t.Fatalf("dashboard identity for CA: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(identity.CAPEM) {
		t.Fatal("ca")
	}
	bootstrapConn := dialLiveTLS(t, server.InternalAddr(), &tls.Config{
		RootCAs:    roots,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS13,
	})
	defer bootstrapConn.Close()
	key, csrPEM := newAgentCSR(t, agentID)
	enrolled, err := agentv1.NewAgentControlClient(bootstrapConn).Enroll(context.Background(), &agentv1.EnrollRequest{
		AgentId:        agentID,
		CsrPem:         string(csrPEM),
		BootstrapToken: token,
	})
	if err != nil {
		t.Fatalf("Enroll(%s): %v", agentID, err)
	}
	return tls.Certificate{
		Certificate: [][]byte{mustDecodePEMBlock(t, enrolled.GetCertPem(), "CERTIFICATE")},
		PrivateKey:  key,
	}
}

func openAgentSync(t *testing.T, server *Server, cert tls.Certificate, hello *agentv1.AgentHello) (agentv1.AgentControl_SyncClient, context.CancelFunc) {
	t.Helper()
	identity, err := server.EnsureDashboardClientIdentity(systemTestDashboardID)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(identity.CAPEM) {
		t.Fatal("ca")
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn := dialLiveTLS(t, server.InternalAddr(), &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      roots,
		ServerName:   "localhost",
		MinVersion:   tls.VersionTLS13,
	})
	t.Cleanup(func() { _ = conn.Close() })
	stream, err := agentv1.NewAgentControlClient(conn).Sync(ctx)
	if err != nil {
		cancel()
		t.Fatalf("Sync: %v", err)
	}
	if err := server.store.db.QueryRowContext(ctx, `SELECT session_incarnation + 1 FROM agent_registrations WHERE id = $1`, hello.GetAgentId()).Scan(&hello.SessionIncarnation); err != nil {
		t.Fatal(err)
	}
	hello.ClusterId = server.authority.ClusterIdentity()
	hello.LocalStoreId = "test-store-" + hello.GetAgentId()
	hello.InitializationState = "ready"
	if err := stream.Send(&agentv1.AgentClientMessage{Payload: &agentv1.AgentClientMessage_Hello{Hello: hello}}); err != nil {
		cancel()
		t.Fatalf("hello: %v", err)
	}
	return stream, cancel
}
