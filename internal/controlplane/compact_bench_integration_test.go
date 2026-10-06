//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/agent"
	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/xds"
	"ebof-wg-mesh/internal/mesh"
	"ebof-wg-mesh/internal/testutil"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// This probe holds a saturated pool, releases it, and leaves the idle pool
// alive for an external cgroup sampler. It does not change an existing schema.
func TestCompactPoolResourceProbe(t *testing.T) {
	mode := os.Getenv("COMPACT_BENCH_POOL")
	if mode == "" {
		t.Skip("set COMPACT_BENCH_POOL=legacy or compact")
	}
	cfg := config.DatabaseConfig{URL: os.Getenv("CONTROLPLANE_TEST_DATABASE_URL")}
	normalizeDatabaseConfig(&cfg)
	if mode == "legacy" {
		cfg.MaxOpenConns = 32
		cfg.MaxIdleConns = 16
	} else if mode != "compact" {
		t.Fatal("unknown mode")
	}
	db, err := sql.Open("pgx", cfg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var clients []*sql.Conn
	for i := 0; i < cfg.MaxOpenConns; i++ {
		client, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
	}
	for _, client := range clients {
		if err := client.Close(); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("open limit=%d idle=%d", cfg.MaxOpenConns, db.Stats().Idle)
	if marker := os.Getenv("COMPACT_BENCH_READY_FILE"); marker != "" {
		if err := os.WriteFile(marker, []byte(mode), 0600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Second)
	}
	// Exercise a 32-request burst through the same pool; lower connection limits
	// must queue safely instead of failing under concurrent work.
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	start := time.Now()
	for i := 0; i < 32; i++ {
		wg.Go(func() { _, err := db.ExecContext(ctx, `SELECT pg_sleep(0.01)`); errs <- err })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("32-request burst elapsed=%s wait_count=%d", time.Since(start), db.Stats().WaitCount)
}
func TestCompactControlPlaneResourceProbe(t *testing.T) {
	if os.Getenv("COMPACT_BENCH_SERVER") != "1" {
		t.Skip("set COMPACT_BENCH_SERVER=1")
	}
	cp := startSystemControlPlane(t, systemControlPlaneOptions{maxOpenConns: 8, maxIdleConns: 2, bootstrap: config.BootstrapConfig{Users: []config.BootstrapUser{{ID: "compact", Email: "compact@example.test", Projects: []string{"compact"}}}}, withDashboard: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var serviceID string
	dataDir := t.TempDir()
	identity, err := cp.server.EnsureDashboardClientIdentity(ctx, systemTestDashboardID)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dataDir, "ca.pem")
	if err := os.WriteFile(caPath, identity.CAPEM, 0600); err != nil {
		t.Fatal(err)
	}
	privateKey, err := mesh.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	agentCfg := config.AgentConfig{
		Profile:      config.ProfileDevelopment,
		Node:         config.NodeConfig{ID: "system-test-agent", Name: "compact", AdvertiseAddr: "fd00:44::10", Resources: config.NodeResourcesConfig{CPUMillis: 2000, MemoryMebibytes: 2048, ReservedCPUMillis: 1000, ReservedMemoryMebibytes: 1536}},
		ControlPlane: config.ControlPlaneClientConfig{Addresses: []string{cp.server.InternalAddr()}, TLS: config.ClientTLSConfig{CAFile: caPath, ServerName: "localhost", BootstrapToken: "system-test-bootstrap"}},
		Runtime:      config.RuntimeConfig{DataDir: dataDir, VolumesDir: filepath.Join(dataDir, "volumes"), Snapshotter: "native", VolumeBackend: "directory"},
		Containerd:   config.ContainerdConfig{Socket: "/run/containerd/containerd.sock", Namespace: "compact-probe"},
		Mesh:         config.MeshConfig{Host: config.HostConfig{IPv6: "fd00:44::10"}, WireGuard: config.WireGuard{InterfaceName: "wg-compact", PrivateKey: privateKey, ListenPort: 51820, AdvertiseEndpoint: "[fd00:44::10]:51820"}},
	}
	if err := config.FinalizeAgent(&agentCfg); err != nil {
		t.Fatal(err)
	}
	app, err := agent.New(agentCfg)
	if err != nil {
		t.Fatal(err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(ctx) }()
	defer func() {
		if serviceID != "" {
			if err := cp.server.delivery.DeleteService(ctx, testUser("compact"), serviceID); err != nil {
				t.Error(err)
			}
			if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 30 * time.Second}, func(context.Context) (bool, error) {
				paths, err := filepath.Glob(filepath.Join(dataDir, "netns", "cni-*"))
				return len(paths) == 0, err
			}); err != nil {
				t.Errorf("remove benchmark workload: %v", err)
			}
		}
		cancel()
		select {
		case <-runErr:
		case <-time.After(10 * time.Second):
			t.Error("agent shutdown timed out")
		}
		_ = app.Close()
	}()
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: time.Minute}, func(ctx context.Context) (bool, error) {
		select {
		case err := <-runErr:
			return false, fmt.Errorf("agent exited: %v", err)
		default:
		}
		rec, err := cp.server.delivery.AgentByID(ctx, "system-test-agent")
		return err == nil && rec.MemoryMebibytesCapcity == 512, nil
	}); err != nil {
		t.Fatal(err)
	}
	store := cp.server.store
	projects, err := store.catalog.listProjects(ctx, testUser("compact"), false)
	if err != nil {
		t.Fatal(err)
	}
	spec := directImageServiceSpec("docker.io/library/python:3.12-alpine", &platformv1.ServiceRuntime{
		CpuMillis: 100, MemoryMebibytes: 256, Ports: runtimePortsFromInts([]int32{8080}),
		Command: []string{"python", "-u", "-c"}, Args: []string{"import http.server,socket; http.server.HTTPServer.address_family=socket.AF_INET6; pad = bytearray(192*1024*1024); print('compact-log'); http.server.HTTPServer(('::',8080),http.server.SimpleHTTPRequestHandler).serve_forever()"},
		HealthCheck: &platformv1.HealthCheck{Type: platformv1.HealthCheck_TYPE_HTTP, Path: "/", TimeoutSeconds: 2},
	})
	service, err := cp.server.delivery.CreateService(ctx, testUser("compact"), productionEnvironmentID(t, store, projects[0].ID), "compact", spec, "system-test-agent")
	if err != nil {
		t.Fatal(err)
	}
	serviceID = service.ID
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 90 * time.Second}, func(ctx context.Context) (bool, error) {
		select {
		case err := <-runErr:
			return false, fmt.Errorf("agent exited: %v", err)
		default:
		}
		deployments, err := cp.server.delivery.ListServiceDeployments(ctx, testUser("compact"), service.ID, 1)
		if err != nil {
			return false, err
		}
		if len(deployments) == 0 {
			return false, nil
		}
		if deployments[0].State == deliverycore.DeploymentStateFailed {
			return false, fmt.Errorf("deployment failed: %s", deployments[0].Detail)
		}
		return deployments[0].State == deliverycore.DeploymentStateActive, nil
	}); err != nil {
		t.Fatal(err)
	}
	startCompactBenchmarkIngress(t, ctx, cp.server)
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 15 * time.Second}, func(ctx context.Context) (bool, error) {
		page, err := cp.server.logStore.ListServiceLogs(ctx, &platformv1.ListServiceLogsRequest{ServiceId: service.ID})
		return err == nil && len(page.Lines) > 0, err
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("COMPACT_BENCH_READY_FILE"), []byte(fmt.Sprint(service.ID)), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Second)
}

// The optional Envoy process joins the same cgroup as the runtime probe.
func startCompactBenchmarkIngress(t *testing.T, ctx context.Context, server *Server) {
	t.Helper()
	binary := os.Getenv("COMPACT_BENCH_ENVOY")
	if binary == "" {
		return
	}
	dir := t.TempDir()
	material, err := server.ProvisionIngressIdentity(ctx, "compact-envoy")
	if err != nil {
		t.Fatal(err)
	}
	if err := xds.WriteIdentity(dir, dir, material); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := xds.RenderBootstrap(xds.BootstrapConfig{NodeID: "compact-envoy", IdentityDir: dir, ServerName: "localhost", XDSAddresses: []string{server.XDSAddr()}, AdminAddress: "127.0.0.1:19000"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bootstrap.yaml")
	if err := os.WriteFile(path, []byte(bootstrap), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(dir, "envoy.log"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, binary, "-c", path, "--concurrency", "2", "--log-level", "warn")
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait(); _ = output.Close() })
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 30 * time.Second}, func(ctx context.Context) (bool, error) {
		response, err := http.Get("http://127.0.0.1:19000/ready")
		if err != nil {
			return false, nil
		}
		defer response.Body.Close()
		return response.StatusCode == 200, nil
	}); err != nil {
		data, _ := os.ReadFile(output.Name())
		t.Fatalf("Envoy not ready: %v, %s", err, data)
	}
}
