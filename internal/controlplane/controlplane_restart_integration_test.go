//go:build integration

package controlplane

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/testutil"
)

func TestControlPlaneRestartResyncsAgentFromStore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		agentID    = "restart-agent"
		token      = "restart-bootstrap"
		surgeID    = "restart-surge"
		surgeToken = "restart-surge-bootstrap"
	)
	dbURL := createTestDatabase(t)
	stateDir := t.TempDir()
	opts := systemControlPlaneOptions{
		databaseURL:     dbURL,
		stateDir:        stateDir,
		bootstrapTokens: []config.AgentBootstrapToken{{AgentID: agentID, Token: token}, {AgentID: surgeID, Token: surgeToken}},
		withDashboard:   true,
	}
	first := startSystemControlPlane(t, opts)
	cert := enrollAgentTLS(t, first.server, agentID, token)
	hello := restartAgentHello(agentID, "fd00:30::21")
	stream, streamCancel := openAgentSync(t, first.server, cert, hello)
	initial := recvDesiredState(t, stream)
	if initial.GetAgentId() != agentID {
		t.Fatalf("initial agent id = %q", initial.GetAgentId())
	}

	userCtx := userContext(t, ctx, "restart-user")
	project, err := first.dashboard.CreateProject(userCtx, &platformv1.CreateProjectRequest{Name: "restart-app"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	envs, err := first.dashboard.ListEnvironments(userCtx, &platformv1.ListEnvironmentsRequest{ProjectId: project.GetId()})
	if err != nil || len(envs.GetEnvironments()) != 1 {
		t.Fatalf("ListEnvironments: %v", err)
	}
	service, err := first.dashboard.CreateService(userCtx, &platformv1.CreateServiceRequest{
		EnvironmentId: envs.GetEnvironments()[0].GetId(),
		Service: &platformv1.ServiceInput{
			Name: "web",
			Spec: directImageServiceSpec("example.test/restart:1", &platformv1.ServiceRuntime{
				CpuMillis: 250, MemoryMebibytes: 256, Ports: runtimePortsFromInts([]int32{8080}),
			}),
		},
	})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if _, err := first.dashboard.DeployEnvironment(userCtx, &platformv1.DeployEnvironmentRequest{EnvironmentId: envs.GetEnvironments()[0].GetId()}); err != nil {
		t.Fatalf("DeployEnvironment: %v", err)
	}
	deployed := recvDesiredState(t, stream)
	if len(deployed.GetServices()) != 1 || deployed.GetServices()[0].GetServiceId() != service.GetId() {
		t.Fatalf("deployed desired state: %+v", deployed)
	}
	beforeRev := deployed.GetRevision()
	streamCancel()
	first.stop()

	second := startSystemControlPlane(t, opts)
	if _, err := second.server.EnsureDashboardClientIdentity(systemTestDashboardID); err != nil {
		t.Fatalf("PKI was not restored: %v", err)
	}
	restream, reCancel := openAgentSync(t, second.server, cert, hello)
	defer reCancel()
	restored := recvDesiredState(t, restream)
	if len(restored.GetServices()) != 1 || restored.GetServices()[0].GetSpec().GetImage() != "example.test/restart:1" {
		t.Fatalf("reconnect did not restore desired state from the store: %+v", restored)
	}
	fromStore, err := second.server.store.desiredStateForAgent(ctx, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if fromStore.GetServices()[0].GetAllocationId() != restored.GetServices()[0].GetAllocationId() {
		t.Fatalf("stream state does not match store: stream=%+v store=%+v", restored, fromStore)
	}
	surgeCert := enrollAgentTLS(t, second.server, surgeID, surgeToken)
	surgeStream, surgeCancel := openAgentSync(t, second.server, surgeCert, restartAgentHello(surgeID, "fd00:30::22"))
	defer surgeCancel()
	if initialSurge := recvDesiredState(t, surgeStream); len(initialSurge.GetServices()) != 0 {
		t.Fatalf("surge agent unexpectedly had allocations before redeploy: %+v", initialSurge)
	}

	if _, err := second.dashboard.RedeployService(userCtx, &platformv1.RedeployServiceRequest{ServiceId: service.GetId()}); err != nil {
		t.Fatalf("RedeployService: %v", err)
	}
	var mutated *agentv1.DesiredNodeState
	for _, candidateID := range []string{agentID, surgeID} {
		candidate, err := second.server.store.desiredStateForAgent(ctx, candidateID)
		if err != nil {
			t.Fatal(err)
		}
		if len(candidate.GetServices()) > 0 && candidate.GetServices()[0].GetDesiredRolloutGeneration() > deployed.GetServices()[0].GetDesiredRolloutGeneration() {
			mutated = candidate
			break
		}
	}
	if mutated == nil {
		t.Fatal("redeploy did not persist a newer desired rollout on either eligible agent")
	}
	if mutated.GetRevision() <= beforeRev {
		t.Fatalf("expected a newer revision after redeploy, before=%d after=%d", beforeRev, mutated.GetRevision())
	}
	if mutated.GetServices()[0].GetDesiredRolloutGeneration() <= deployed.GetServices()[0].GetDesiredRolloutGeneration() {
		t.Fatalf("redeploy did not advance rollout: before=%d after=%d",
			deployed.GetServices()[0].GetDesiredRolloutGeneration(), mutated.GetServices()[0].GetDesiredRolloutGeneration())
	}
}

func TestControlPlaneRestartContinuesFailoverAndIngress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		deadID  = "restart-dead"
		liveID  = "restart-live"
		deadTok = "restart-dead-token"
		liveTok = "restart-live-token"
	)
	var (
		mu         sync.Mutex
		syncBodies int
	)
	caddy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		syncBodies++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(caddy.Close)

	dbURL := createTestDatabase(t)
	stateDir := t.TempDir()
	opts := systemControlPlaneOptions{
		databaseURL:     dbURL,
		stateDir:        stateDir,
		ingressAdminURL: caddy.URL + "/load",
		bootstrap: config.BootstrapConfig{Users: []config.BootstrapUser{
			{ID: "user-1", Email: "user@example.com", Projects: []string{"ha"}},
		}},
		bootstrapTokens: []config.AgentBootstrapToken{
			{AgentID: deadID, Token: deadTok},
			{AgentID: liveID, Token: liveTok},
		},
		withDashboard: true,
	}
	first := startSystemControlPlane(t, opts)
	deadCert := enrollAgentTLS(t, first.server, deadID, deadTok)
	liveCert := enrollAgentTLS(t, first.server, liveID, liveTok)
	deadStream, deadCancel := openAgentSync(t, first.server, deadCert, restartAgentHello(deadID, "fd00:30::31"))
	liveStream, liveCancel := openAgentSync(t, first.server, liveCert, restartAgentHello(liveID, "fd00:30::32"))
	_ = recvDesiredState(t, deadStream)
	_ = recvDesiredState(t, liveStream)

	store := first.server.store
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	envID := productionEnvironmentID(t, store, projects[0].ID)
	failing, err := store.createService(ctx, "user-1", envID, "failover-web", serviceSpec(), deadID)
	if err != nil {
		t.Fatal(err)
	}
	ingressSvc, err := store.createService(ctx, "user-1", envID, "ingress-web", serviceSpec(), liveID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.createDomainBinding(ctx, "user-1", projects[0].ID, "restart.example.com", ingressSvc.ID, 8080); err != nil {
		t.Fatal(err)
	}
	if err := store.markAllocationHealthyForTest(ctx, ingressSvc.ID, "fd00:200:1::10", 8080); err != nil {
		t.Fatal(err)
	}

	deadCancel()
	liveCancel()
	if _, err := store.db.ExecContext(ctx, `UPDATE agents SET last_seen_at = $1 WHERE id = $2`, time.Now().UTC().Add(-2*agentHealthyTTL), deadID); err != nil {
		t.Fatalf("mark stale: %v", err)
	}
	first.stop()

	second := startSystemControlPlane(t, opts)
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 10 * time.Second, Interval: 50 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		state, err := second.server.store.desiredStateForAgent(ctx, liveID)
		if err != nil {
			return false, err
		}
		for _, svc := range state.GetServices() {
			if svc.GetServiceId() == failing.ID {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("failover did not move the stateless service onto the surviving agent: %v", err)
	}
	deadState, err := second.server.store.desiredStateForAgent(ctx, deadID)
	if err != nil {
		t.Fatal(err)
	}
	for _, svc := range deadState.GetServices() {
		if svc.GetServiceId() == failing.ID {
			t.Fatal("dead agent still has the failed-over service in desired state")
		}
	}

	if err := second.server.ingress.Sync(ctx); err != nil {
		t.Fatalf("startup-equivalent ingress Sync: %v", err)
	}
	rendered, err := second.server.ingress.render(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, route := range rendered.Apps.HTTP.Servers["srv0"].Routes {
		if len(route.Match) == 0 {
			continue
		}
		for _, host := range route.Match[0].Host {
			if host == "restart.example.com" {
				found = true
				if len(route.Handle) == 0 || !strings.Contains(route.Handle[0].Upstreams[0].Dial, ":8080") {
					t.Fatalf("ingress backend missing after restart: %+v", route)
				}
			}
		}
	}
	if !found {
		t.Fatalf("rendered ingress after restart missing restart.example.com: %+v", rendered)
	}
	mu.Lock()
	defer mu.Unlock()
	if syncBodies == 0 {
		t.Fatal("expected at least one ingress Sync against the admin endpoint")
	}
}

func restartAgentHello(id, addr string) *agentv1.AgentHello {
	return &agentv1.AgentHello{
		AgentId:                 id,
		Name:                    id,
		AdvertiseAddr:           addr,
		WireguardPublicKey:      id + "-pubkey",
		WireguardListenPort:     51820,
		CpuMillisCapacity:       2000,
		MemoryMebibytesCapacity: 4096,
		RuntimeCapabilities:     []string{"containerd", "wireguard", "ebpf-policy"},
		SoftwareVersion:         "test",
	}
}
