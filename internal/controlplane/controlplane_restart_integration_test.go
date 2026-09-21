//go:build integration

package controlplane

import (
	"context"
	"io"
	"net"
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
			Spec: directImageServiceSpec(pinnedImage("a"), &platformv1.ServiceRuntime{
				CpuMillis: 250, MemoryMebibytes: 256, Ports: runtimePortsFromInts([]int32{8080}),
			}),
		},
	})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if _, err := first.dashboard.ReleaseEnvironment(userCtx, &platformv1.ReleaseEnvironmentRequest{EnvironmentId: envs.GetEnvironments()[0].GetId()}); err != nil {
		t.Fatalf("ReleaseEnvironment: %v", err)
	}
	deployed := recvDesiredState(t, stream)
	if len(deployed.GetServices()) != 1 || deployed.GetServices()[0].GetServiceId() != service.GetId() {
		t.Fatalf("deployed desired state: %+v", deployed)
	}
	streamCancel()
	first.stop()

	second := startSystemControlPlane(t, opts)
	if _, err := second.server.EnsureDashboardClientIdentity(systemTestDashboardID); err != nil {
		t.Fatalf("PKI was not restored: %v", err)
	}
	restream, reCancel := openAgentSync(t, second.server, cert, hello)
	defer reCancel()
	restored := recvDesiredState(t, restream)
	if len(restored.GetServices()) != 1 || restored.GetServices()[0].GetSpec().GetImage() != pinnedImage("a") {
		t.Fatalf("reconnect did not restore desired state from the store: %+v", restored)
	}
	fromStore, err := desiredStateForAgent(ctx, second.server.store, agentID)
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
		t.Fatalf("surge agent unexpectedly had allocations before deployment action: %+v", initialSurge)
	}
	beforeRedeploy := make(map[string]int64, 2)
	for _, candidateID := range []string{agentID, surgeID} {
		candidate, err := desiredStateForAgent(ctx, second.server.store, candidateID)
		if err != nil {
			t.Fatal(err)
		}
		beforeRedeploy[candidateID] = candidate.GetReconciliationCursor()
	}
	released, err := second.dashboard.GetService(userCtx, &platformv1.GetServiceRequest{ServiceId: service.GetId()})
	if err != nil {
		t.Fatalf("GetService after restart: %v", err)
	}

	if _, err := second.dashboard.ApplyDeploymentAction(userCtx, &platformv1.ApplyDeploymentActionRequest{
		ServiceId:      service.GetId(),
		DeploymentId:   released.GetLatestDeployment().GetDeploymentId(),
		Action:         platformv1.DeploymentAction_DEPLOYMENT_ACTION_EXACT_REDEPLOY,
		IdempotencyKey: "restart-resync-exact-redeploy",
	}); err != nil {
		t.Fatalf("ApplyDeploymentAction(EXACT_REDEPLOY): %v", err)
	}
	var mutated *agentv1.DesiredNodeState
	var mutatedAgentID string
	for _, candidateID := range []string{agentID, surgeID} {
		candidate, err := desiredStateForAgent(ctx, second.server.store, candidateID)
		if err != nil {
			t.Fatal(err)
		}
		if len(candidate.GetServices()) > 0 && candidate.GetServices()[0].GetDesiredRolloutGeneration() > deployed.GetServices()[0].GetDesiredRolloutGeneration() {
			mutated = candidate
			mutatedAgentID = candidateID
			break
		}
	}
	if mutated == nil {
		t.Fatal("exact redeploy did not persist a newer desired rollout on either eligible agent")
	}
	if mutated.GetReconciliationCursor() <= beforeRedeploy[mutatedAgentID] {
		t.Fatalf("expected a newer revision after redeploy, before=%d after=%d", beforeRedeploy[mutatedAgentID], mutated.GetReconciliationCursor())
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
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	envID := productionEnvironmentID(t, store, projects[0].ID)
	failing, err := createService(ctx, store, "user-1", envID, "failover-web", serviceSpec(), deadID)
	if err != nil {
		t.Fatal(err)
	}
	ingressSvc, err := createService(ctx, store, "user-1", envID, "ingress-web", serviceSpec(), liveID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, testUser("user-1"), "restart.example.com", ingressSvc.ID, 8080); err != nil {
		t.Fatal(err)
	}
	if err := store.markAllocationHealthyForTest(ctx, ingressSvc.ID, "fd00:200:1::10", 8080); err != nil {
		t.Fatal(err)
	}

	// Start with completed deployments so this exercises replacement after node
	// loss, rather than racing the initial rollout with the restart.
	original := mustAllocationOnAgent(t, store, failing.ID, deadID)
	if err := store.markAllocationHealthyForTest(ctx, failing.ID, original.AllocationIPv6, 8080); err != nil {
		t.Fatal(err)
	}
	inventory, err := agentAllocationIDsForTest(ctx, store, liveID)
	if err != nil {
		t.Fatalf("snapshot surviving agent inventory: %v", err)
	}

	// Stop reconciliation before disconnecting the agents. Expiring a session
	// on the first server lets it perform the failover we intend to test on
	// the second. Live sessions are intentionally not restored from storage.
	first.stop()
	deadCancel()
	liveCancel()

	second := startSystemControlPlane(t, opts)
	reconnectedHello := restartAgentHello(liveID, "fd00:30::32")
	reconnectedHello.SessionId += "-reconnected"
	for _, allocationID := range inventory {
		reconnectedHello.Allocations = append(reconnectedHello.Allocations, &agentv1.ServiceCondition{AllocationId: allocationID})
	}
	reconnectedStream, reconnectedCancel := openAgentSync(t, second.server, liveCert, reconnectedHello)
	defer reconnectedCancel()
	_ = recvDesiredState(t, reconnectedStream)
	if session, ok := fixtureLive(second.server.store).Session(liveID); !ok || !session.Ready || !session.Reachable || !session.Reconciled {
		t.Fatalf("surviving agent was not admitted after reconnect: %+v (present=%t)", session, ok)
	}
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 10 * time.Second, Interval: 50 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		state, err := desiredStateForAgent(ctx, second.server.store, liveID)
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
		allocations, readErr := second.server.store.reads.ListAllocationsByServiceID(ctx, failing.ID)
		session, connected := fixtureLive(second.server.store).Session(liveID)
		t.Fatalf("failover did not move the stateless service onto the surviving agent: %v; allocations=%+v (read error=%v); surviving session=%+v (present=%t)", err, allocations, readErr, session, connected)
	}
	_ = requireNodeLossReplacement(t, second.server.store, failing.ID, original.ID, deadID, liveID)
	deadState, err := desiredStateForAgent(ctx, second.server.store, deadID)
	if err != nil {
		t.Fatal(err)
	}
	for _, svc := range deadState.GetServices() {
		if svc.GetServiceId() == failing.ID {
			t.Fatal("dead agent still has the failed-over service in desired state")
		}
	}
	if err := second.server.store.markAllocationHealthyForTest(ctx, ingressSvc.ID, "fd00:200:1::10", 8080); err != nil {
		t.Fatal(err)
	}

	if err := second.server.ingress.Sync(ctx); err != nil {
		t.Fatalf("startup-equivalent ingress Sync: %v", err)
	}
	rendered, err := second.server.ingress.Render(ctx)
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
		WireguardEndpoint:       net.JoinHostPort(addr, "51820"),
		CpuMillisCapacity:       2000,
		MemoryMebibytesCapacity: 4096,
		RuntimeCapabilities:     []string{"containerd", "wireguard", "ebpf-policy"},
		SoftwareVersion:         "test",
		SessionId:               "restart-session-" + id,
	}
}

func TestControlPlaneRestartBootsFromCompactedJournal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		agentID = "compaction-agent"
		token   = "compaction-bootstrap"
	)
	dbURL := createTestDatabase(t)
	opts := systemControlPlaneOptions{
		databaseURL:     dbURL,
		stateDir:        t.TempDir(),
		bootstrapTokens: []config.AgentBootstrapToken{{AgentID: agentID, Token: token}},
		withDashboard:   true,
	}
	first := startSystemControlPlane(t, opts)
	cert := enrollAgentTLS(t, first.server, agentID, token)
	hello := restartAgentHello(agentID, "fd00:30::41")
	stream, streamCancel := openAgentSync(t, first.server, cert, hello)
	_ = recvDesiredState(t, stream)

	userCtx := userContext(t, ctx, "compaction-user")
	project, err := first.dashboard.CreateProject(userCtx, &platformv1.CreateProjectRequest{Name: "compaction-app"})
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
			Spec: directImageServiceSpec(pinnedImage("a"), &platformv1.ServiceRuntime{
				CpuMillis: 250, MemoryMebibytes: 256, Ports: runtimePortsFromInts([]int32{8080}),
			}),
		},
	})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if _, err := first.dashboard.ReleaseEnvironment(userCtx, &platformv1.ReleaseEnvironmentRequest{EnvironmentId: envs.GetEnvironments()[0].GetId()}); err != nil {
		t.Fatalf("ReleaseEnvironment: %v", err)
	}
	deployed := recvDesiredState(t, stream)
	if len(deployed.GetServices()) != 1 || deployed.GetServices()[0].GetServiceId() != service.GetId() {
		t.Fatalf("deployed desired state: %+v", deployed)
	}

	store := first.server.store
	watermark, err := store.compactJournal(ctx, 0)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	var preWatermark int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM cluster_journal WHERE cluster_id = 'default' AND log_index <= $1`, watermark).Scan(&preWatermark); err != nil {
		t.Fatal(err)
	}
	if preWatermark != 0 {
		t.Fatalf("%d entries survived compaction below watermark %d", preWatermark, watermark)
	}
	streamCancel()
	first.stop()

	second := startSystemControlPlane(t, opts)
	restored, err := desiredStateForAgent(ctx, second.server.store, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.GetServices()) != 1 || restored.GetServices()[0].GetServiceId() != service.GetId() {
		t.Fatalf("restart after compaction did not rebuild desired state: %+v", restored)
	}
	if restored.GetServices()[0].GetAllocationId() != deployed.GetServices()[0].GetAllocationId() {
		t.Fatalf("snapshot boot changed allocation: before=%s after=%s",
			deployed.GetServices()[0].GetAllocationId(), restored.GetServices()[0].GetAllocationId())
	}
}
