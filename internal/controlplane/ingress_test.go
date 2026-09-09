//go:build integration

package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/routing"
	"ebof-wg-mesh/internal/testutil"
)

func TestIngressRenderIncludesHealthyDomains(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)

	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, "user-1", "demo.example.com", service.ID, 8080); err != nil {
		t.Fatal(err)
	}
	if err := store.markAllocationHealthyForTest(ctx, service.ID, "10.0.0.10", 8080); err != nil {
		t.Fatal(err)
	}

	syncer := routing.NewIngressSyncer("http://127.0.0.1:2019/load", store.routing)
	cfg, err := syncer.Render(ctx)
	if err != nil {
		t.Fatal(err)
	}
	routes := cfg.Apps.HTTP.Servers["srv0"].Routes
	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(routes))
	}
	hosts := routes[0].Match[0].Host
	if len(hosts) != 1 || hosts[0] != "demo.example.com" {
		t.Fatalf("unexpected ingress host match %+v", hosts)
	}
	if got := routes[0].Handle[0].Upstreams[0].Dial; got != "10.0.0.10:8080" {
		t.Fatalf("expected healthy IPv4 upstream, got %q", got)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE allocation_observations SET healthy_ipv4_ports = '[]'
		WHERE allocation_id IN (SELECT id FROM allocation_assignments WHERE service_id = $1)`, service.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.markAllocationHealthyForTest(ctx, service.ID, "fd00:200:1::10", 8080); err != nil {
		t.Fatal(err)
	}
	cfg, err = syncer.Render(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Apps.HTTP.Servers["srv0"].Routes[0].Handle[0].Upstreams[0].Dial; got != "[fd00:200:1::10]:8080" {
		t.Fatalf("expected healthy IPv6 fallback upstream, got %q", got)
	}
}

func TestIngressRenderRequiresReportedHealthyTargetPort(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, "user-1", "demo.example.com", service.ID, 8080); err != nil {
		t.Fatal(err)
	}
	if err := store.markAllocationHealthyForTest(ctx, service.ID, "attacker.example", 9090); err != nil {
		t.Fatal(err)
	}

	backends, err := store.routing.HealthyIngressBackends(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(backends) != 0 {
		t.Fatalf("expected unhealthy target port to be excluded, got %+v", backends)
	}
}

func TestIngressRenderIncludesStaticRoutesAheadOfDynamicBackends(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)

	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, "user-1", "echo.localtest.me", service.ID, 8080); err != nil {
		t.Fatal(err)
	}
	if err := store.markAllocationHealthyForTest(ctx, service.ID, "10.0.0.10", 8080); err != nil {
		t.Fatal(err)
	}

	syncer := routing.NewIngressSyncer(
		"http://127.0.0.1:2019/load",
		store.routing,
		routing.WithIngressStaticRoutes([]routing.IngressStaticRoute{{
			Hosts:    []string{"platform.localtest.me", "mesh.dev.example.test"},
			Upstream: "host.docker.internal:41235",
		}}),
		routing.WithIngressListenAddrs([]string{":8080"}),
		routing.WithIngressAdminListen(":2019"),
		routing.WithIngressAutomaticHTTPSDisabled(true),
	)
	cfg, err := syncer.Render(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin == nil || cfg.Admin.Listen != ":2019" {
		t.Fatalf("unexpected admin config %+v", cfg.Admin)
	}
	server := cfg.Apps.HTTP.Servers["srv0"]
	if len(server.Listen) != 1 || server.Listen[0] != ":8080" {
		t.Fatalf("unexpected listen addrs %+v", server.Listen)
	}
	if server.AutomaticHTTPS == nil || !server.AutomaticHTTPS.Disable {
		t.Fatalf("expected automatic https disabled, got %+v", server.AutomaticHTTPS)
	}
	if len(server.Routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(server.Routes))
	}
	staticHosts := server.Routes[0].Match[0].Host
	if strings.Join(staticHosts, ",") != "mesh.dev.example.test,platform.localtest.me" {
		t.Fatalf("unexpected static route hosts %+v", staticHosts)
	}
	if got := server.Routes[0].Handle[0].Upstreams[0].Dial; got != "host.docker.internal:41235" {
		t.Fatalf("unexpected static upstream %q", got)
	}
	dynamicHosts := server.Routes[1].Match[0].Host
	if len(dynamicHosts) != 1 || dynamicHosts[0] != "echo.localtest.me" {
		t.Fatalf("unexpected dynamic route hosts %+v", dynamicHosts)
	}
}

func TestIngressSyncSerializesConcurrentPushes(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)

	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	serviceA, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web-a", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, "user-1", "a.example.com", serviceA.ID, 8080); err != nil {
		t.Fatal(err)
	}
	if err := store.markAllocationHealthyForTest(ctx, serviceA.ID, "10.0.0.10", 8080); err != nil {
		t.Fatal(err)
	}

	syncer := routing.NewIngressSyncer("http://caddy.invalid/load", store.routing)
	transport := &blockingIngressTransport{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	routing.WithHTTPClient(&http.Client{Transport: transport})(syncer)

	firstErrCh := make(chan error, 1)
	go func() {
		firstErrCh <- syncer.Sync(ctx)
	}()

	<-transport.firstStarted

	serviceB, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web-b", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, "user-1", "b.example.com", serviceB.ID, 8080); err != nil {
		t.Fatal(err)
	}
	if err := store.markAllocationHealthyForTest(ctx, serviceB.ID, "10.0.0.11", 8080); err != nil {
		t.Fatal(err)
	}

	secondErrCh := make(chan error, 1)
	go func() {
		secondErrCh <- syncer.Sync(ctx)
	}()

	close(transport.releaseFirst)

	if err := <-firstErrCh; err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	if err := <-secondErrCh; err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if transport.overlap.Load() != 0 {
		t.Fatal("expected ingress pushes to be serialized")
	}
	if len(transport.requests) != 2 {
		t.Fatalf("expected 2 ingress pushes, got %d", len(transport.requests))
	}
	if transport.requests[0].method != http.MethodPost || !strings.HasSuffix(transport.requests[0].url, "/load") {
		t.Fatalf("expected first push to POST /load, got %s %s", transport.requests[0].method, transport.requests[0].url)
	}
	if transport.requests[1].method != http.MethodPatch || !strings.HasSuffix(transport.requests[1].url, "/config/apps/http/servers/srv0/routes") {
		t.Fatalf("expected second push to PATCH routes, got %s %s", transport.requests[1].method, transport.requests[1].url)
	}
	firstRoutes := ingressRouteCount(t, transport.requests[0].body)
	secondRoutes := ingressRouteCount(t, transport.requests[1].body)
	if firstRoutes != 1 {
		t.Fatalf("expected first push to contain 1 route, got %d", firstRoutes)
	}
	if secondRoutes != 2 {
		t.Fatalf("expected second push to contain 2 routes, got %d", secondRoutes)
	}
}

func TestIngressRequestSyncCoalescesBurst(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)

	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.markAllocationHealthyForTest(ctx, service.ID, "10.0.0.10", 8080); err != nil {
		t.Fatal(err)
	}

	syncer := routing.NewIngressSyncer("http://caddy.invalid/load", store.routing)
	routing.WithMinSyncInterval(20 * time.Millisecond)(syncer)
	transport := &blockingIngressTransport{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	routing.WithHTTPClient(&http.Client{Transport: transport})(syncer)

	runCtx, cancelRun := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- syncer.Run(runCtx) }()
	t.Cleanup(func() {
		cancelRun()
		if err := <-runDone; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	<-transport.firstStarted
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, "user-1", "web.example.com", service.ID, 8080); err != nil {
		t.Fatalf("createDomainBinding: %v", err)
	}
	syncer.RequestSync()
	syncer.RequestSync()
	syncer.RequestSync()
	close(transport.releaseFirst)

	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: time.Second}, func(ctx context.Context) (bool, error) {
		return transport.calls.Load() == 2, nil
	}); err != nil {
		t.Fatalf("wait for coalesced ingress pushes: %v", err)
	}
	time.Sleep(2 * syncer.MinSyncInterval())
	if got := transport.calls.Load(); got != 2 {
		t.Fatalf("expected exactly 2 ingress pushes, got %d", got)
	}
}

func TestIngressSyncSkipsUnchangedConfig(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}

	syncer := routing.NewIngressSyncer("http://caddy.invalid/load", store.routing)
	transport := &blockingIngressTransport{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	routing.WithHTTPClient(&http.Client{Transport: transport})(syncer)
	close(transport.releaseFirst)

	if err := syncer.Sync(ctx); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	if err := syncer.Sync(ctx); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("expected unchanged config to skip the second push, got %d", got)
	}
}

type recordedIngressPush struct {
	method string
	url    string
	body   []byte
}

type blockingIngressTransport struct {
	firstStarted chan struct{}
	releaseFirst chan struct{}
	calls        atomic.Int32
	inFlight     atomic.Int32
	overlap      atomic.Int32
	mu           sync.Mutex
	requests     []recordedIngressPush
}

func (t *blockingIngressTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.inFlight.Add(1) > 1 {
		t.overlap.Store(1)
	}
	defer t.inFlight.Add(-1)

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.requests = append(t.requests, recordedIngressPush{
		method: req.Method,
		url:    req.URL.String(),
		body:   append([]byte(nil), body...),
	})
	t.mu.Unlock()

	if t.calls.Add(1) == 1 {
		close(t.firstStarted)
		<-t.releaseFirst
	}

	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader("ok")),
		Header:     make(http.Header),
	}, nil
}

func ingressRouteCount(t *testing.T, body []byte) int {
	t.Helper()

	var routes []json.RawMessage
	if err := json.Unmarshal(body, &routes); err == nil && json.Valid(body) && len(body) > 0 && body[0] == '[' {
		return len(routes)
	}

	var cfg struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Routes []json.RawMessage `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("json.Unmarshal ingress body: %v", err)
	}
	server, ok := cfg.Apps.HTTP.Servers["srv0"]
	if !ok {
		t.Fatal("expected srv0 server in ingress body")
	}
	return len(server.Routes)
}

func agentHello(id string) *agentv1.AgentHello {
	return &agentv1.AgentHello{
		AgentId:                 id,
		Name:                    id,
		AdvertiseAddr:           "fd00:30::10",
		WireguardPublicKey:      "test-public-key",
		WireguardListenPort:     51820,
		CpuMillisCapacity:       2000,
		MemoryMebibytesCapacity: 4096,
		RuntimeCapabilities:     []string{"containerd", "wireguard", "ebpf-policy"},
		SoftwareVersion:         "test",
		SessionId:               "test-session-" + id,
	}
}

func testMeshConfig() config.ControlPlaneMeshConfig {
	return config.ControlPlaneMeshConfig{
		InterfaceName:              "wg0",
		ListenPort:                 51820,
		NetworkCIDR:                "fd00:44::/64",
		WorkloadIPv4PoolCIDR:       "10.200.0.0/16",
		WorkloadIPv4NodePrefixBits: 24,
		WorkloadPoolCIDR:           "fd00:200::/48",
		PersistentKeepaliveSeconds: 5,
	}
}

func serviceSpec() *platformv1.ServiceSpec {
	return directImageServiceSpec("nginx:latest", &platformv1.ServiceRuntime{
		Ports:           runtimePortsFromInts([]int32{8080}),
		CpuMillis:       250,
		MemoryMebibytes: 128,
	})
}
