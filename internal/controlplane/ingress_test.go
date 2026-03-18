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
	"ebof-wg-mesh/internal/testutil"
)

func TestIngressRenderIncludesHealthyDomains(t *testing.T) {
	store := openTestStore(t)

	ctx := context.Background()
	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := store.createService(ctx, "user-1", projects[0].ID, "web", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.createDomainBinding(ctx, "user-1", projects[0].ID, "demo.example.com", service.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.markAllocationHealthyForTest(ctx, service.ID, "10.0.0.10:8080"); err != nil {
		t.Fatal(err)
	}

	syncer := NewIngressSyncer("http://127.0.0.1:2019/load", store)
	cfg, err := syncer.render(ctx)
	if err != nil {
		t.Fatal(err)
	}
	apps := cfg["apps"].(map[string]any)
	httpApp := apps["http"].(map[string]any)
	servers := httpApp["servers"].(map[string]any)
	srv0 := servers["srv0"].(map[string]any)
	routes := srv0["routes"].([]map[string]any)
	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(routes))
	}
	matchers := routes[0]["match"].([]map[string]any)
	hosts := matchers[0]["host"].([]string)
	if len(hosts) != 1 || hosts[0] != "demo.example.com" {
		t.Fatalf("unexpected ingress host match %+v", hosts)
	}
}

func TestIngressSyncSerializesConcurrentPushes(t *testing.T) {
	store := openTestStore(t)

	ctx := context.Background()
	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	serviceA, err := store.createService(ctx, "user-1", projects[0].ID, "web-a", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.createDomainBinding(ctx, "user-1", projects[0].ID, "a.example.com", serviceA.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.markAllocationHealthyForTest(ctx, serviceA.ID, "10.0.0.10:8080"); err != nil {
		t.Fatal(err)
	}

	syncer := NewIngressSyncer("http://caddy.invalid/load", store)
	transport := &blockingIngressTransport{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	syncer.client = &http.Client{Transport: transport}

	firstErrCh := make(chan error, 1)
	go func() {
		firstErrCh <- syncer.Sync(ctx)
	}()

	<-transport.firstStarted

	serviceB, err := store.createService(ctx, "user-1", projects[0].ID, "web-b", serviceSpec(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.createDomainBinding(ctx, "user-1", projects[0].ID, "b.example.com", serviceB.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.markAllocationHealthyForTest(ctx, serviceB.ID, "10.0.0.11:8080"); err != nil {
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
	if len(transport.bodies) != 2 {
		t.Fatalf("expected 2 ingress pushes, got %d", len(transport.bodies))
	}
	firstRoutes := ingressRouteCount(t, transport.bodies[0])
	secondRoutes := ingressRouteCount(t, transport.bodies[1])
	if firstRoutes != 1 {
		t.Fatalf("expected first push to contain 1 route, got %d", firstRoutes)
	}
	if secondRoutes != 2 {
		t.Fatalf("expected second push to contain 2 routes, got %d", secondRoutes)
	}
}

func TestIngressRequestSyncCoalescesBurst(t *testing.T) {
	store := openTestStore(t)

	ctx := context.Background()
	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := store.createService(ctx, "user-1", projects[0].ID, "web", serviceSpec(), "node-1", []string{"demo.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.markAllocationHealthyForTest(ctx, service.ID, "10.0.0.10:8080"); err != nil {
		t.Fatal(err)
	}

	syncer := NewIngressSyncer("http://caddy.invalid/load", store)
	syncer.minSyncInterval = 20 * time.Millisecond
	transport := &blockingIngressTransport{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	syncer.client = &http.Client{Transport: transport}

	syncer.RequestSync()
	<-transport.firstStarted
	syncer.RequestSync()
	syncer.RequestSync()
	syncer.RequestSync()
	close(transport.releaseFirst)

	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: time.Second}, func(ctx context.Context) (bool, error) {
		return transport.calls.Load() == 2, nil
	}); err != nil {
		t.Fatalf("wait for coalesced ingress pushes: %v", err)
	}
	time.Sleep(2 * syncer.minSyncInterval)
	if got := transport.calls.Load(); got != 2 {
		t.Fatalf("expected exactly 2 ingress pushes, got %d", got)
	}
}

type blockingIngressTransport struct {
	firstStarted chan struct{}
	releaseFirst chan struct{}
	calls        atomic.Int32
	inFlight     atomic.Int32
	overlap      atomic.Int32
	mu           sync.Mutex
	bodies       [][]byte
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
	t.bodies = append(t.bodies, append([]byte(nil), body...))
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

	var cfg map[string]any
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("json.Unmarshal ingress body: %v", err)
	}
	apps := cfg["apps"].(map[string]any)
	httpApp := apps["http"].(map[string]any)
	servers := httpApp["servers"].(map[string]any)
	srv0 := servers["srv0"].(map[string]any)
	routes := srv0["routes"].([]any)
	return len(routes)
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
	}
}

func testMeshConfig() config.ControlPlaneMeshConfig {
	return config.ControlPlaneMeshConfig{
		InterfaceName:              "wg0",
		ListenPort:                 51820,
		NetworkCIDR:                "fd00:44::/64",
		WorkloadPoolCIDR:           "fd00:200::/48",
		PersistentKeepaliveSeconds: 5,
	}
}

func serviceSpec() *platformv1.ServiceSpec {
	return &platformv1.ServiceSpec{
		Image:           "nginx:latest",
		ContainerPort:   8080,
		CpuMillis:       250,
		MemoryMebibytes: 128,
	}
}
