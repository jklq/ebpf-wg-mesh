//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestLiveOwnerTakeoverStartsUnknown(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	hello := agentHello("node-1")
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", directImageServiceSpec("busybox:1.36", nil), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	allocs, err := store.reads.ListAllocationsByServiceID(ctx, service.ID)
	if err != nil || len(allocs) == 0 {
		t.Fatalf("allocations: %+v %v", allocs, err)
	}
	if err := store.markAllocationHealthyForTest(ctx, service.ID, allocs[0].AllocationIPv4, 8080); err != nil {
		t.Fatal(err)
	}
	allocs, err = store.reads.ListAllocationsByServiceID(ctx, service.ID)
	if err != nil || len(allocs) == 0 || !allocs[0].Healthy {
		t.Fatalf("pre-takeover health: %+v %v", allocs, err)
	}

	testDelivery(store).ResignLive()
	if store.fleet.sessions.Serving() || store.routing.live.Publishing() {
		t.Fatal("former owner still serving")
	}
	if err := testDelivery(store).ObserveAgentHeartbeat(ctx, hello.GetAgentId(), hello.GetSessionId(), false); !errors.Is(err, deliverycore.ErrNotLiveOwner) {
		t.Fatalf("former owner accepted heartbeat: %v", err)
	}

	if err := testDelivery(store).BecomeLive(ctx); err != nil {
		t.Fatal(err)
	}
	allocs, err = store.reads.ListAllocationsByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if allocs[0].Healthy || allocs[0].AppliedRolloutGeneration != 0 {
		t.Fatalf("takeover retained observations: %+v", allocs[0])
	}
	hello.SessionIncarnation++
	hello.SessionId = "after-takeover"
	hello.Allocations = []*agentv1.ServiceCondition{{AllocationId: allocs[0].ID}}
	if _, err := testDelivery(store).RegisterAgent(ctx, hello); err != nil {
		t.Fatal(err)
	}
	if !fixtureLive(store).Admitted(hello.GetAgentId()) {
		t.Fatal("reconciled agent not admitted after takeover")
	}
}

func TestNonOwnerRedirectsAgentThenOwnerAdmitsAndSchedules(t *testing.T) {
	ctx := context.Background()
	owner := openTestStore(t)
	nonOwner, err := openPersistence(config.DatabaseConfig{
		URL: sharedTestDatabase(t), MaxOpenConns: 2, MaxIdleConns: 2,
	}, testMeshConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nonOwner.Close() })
	if nonOwner.fleet.sessions.Serving() {
		t.Fatal("second replica unexpectedly became the live owner")
	}

	// A non-owner agent endpoint refuses the session and points at the owner.
	const ownerAddr = "owner.example:9443"
	nonOwnerAgent := newAgentService(nonOwner.fleet, testDelivery(nonOwner), nil, nil, nil, nil, false, "", "",
		withLiveOwner(staticLiveOwner{held: false, addr: ownerAddr}),
	)
	err = nonOwnerAgent.requireLiveOwner(ctx)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), ownerAddr) {
		t.Fatalf("non-owner agent handshake error = %v, want redirect to %q", err, ownerAddr)
	}

	// The owner admits the agent and schedules a released service on it.
	hello := agentHello("node-1")
	hello.AdvertiseAddr = "fd00:30::1"
	if _, err := upsertTestAgent(t, owner, ctx, hello); err != nil {
		t.Fatal(err)
	}
	if !fixtureLive(owner).Admitted(hello.GetAgentId()) {
		t.Fatal("owner did not admit the redirected agent")
	}
	if err := owner.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := owner.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	environmentID := productionEnvironmentID(t, owner, projects[0].ID)
	service, err := createScheduledService(ctx, owner, "user-1", environmentID, "web", directImageServiceSpec("example.test/web:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := releaseEnvironmentServiceForTest(ctx, owner, "user-1", environmentID, service.ID); err != nil {
		t.Fatal(err)
	}
	allocations, err := owner.reads.ListAllocationsByServiceID(ctx, service.ID)
	if err != nil || len(allocations) == 0 {
		t.Fatalf("allocations: %+v %v", allocations, err)
	}
	if allocations[0].AgentID != hello.GetAgentId() {
		t.Fatalf("allocation agent = %q, want %q", allocations[0].AgentID, hello.GetAgentId())
	}
}

func TestLiveOwnerPausedProcessStopsPublication(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	hello := agentHello("node-1")
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}
	store.publication.SetPublishing(false)
	if store.routing.live.Publishing() {
		t.Fatal("paused owner still publishing")
	}
	if err := testDelivery(store).ObserveAgentHeartbeat(ctx, hello.GetAgentId(), hello.GetSessionId(), false); err != nil {
		t.Fatalf("heartbeat during pause: %v", err)
	}
	lease := newLeaseManager(store.database, 0, 0)
	claim, ok, err := lease.acquire(ctx, "paused-publication")
	if err != nil || !ok {
		t.Fatalf("acquire: %v %v", ok, err)
	}
	if err := lease.release(ctx, claim); err != nil {
		t.Fatal(err)
	}
	stale := context.WithValue(ctx, leaseContextKey{}, claim)
	if err := store.withProductTx(stale, func(context.Context, *sql.Tx) error { return nil }); !errors.Is(err, errLeaseLost) {
		t.Fatalf("paused process committed after lease loss: %v", err)
	}
	if err := store.withLeaseGuard(stale, func() error { return errors.New("published") }); !errors.Is(err, errLeaseLost) {
		t.Fatalf("paused process published after lease loss: %v", err)
	}
}

func TestLiveOwnerStaleSessionRejected(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	hello := agentHello("node-1")
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}
	if err := testDelivery(store).ObserveAgentHeartbeat(ctx, hello.GetAgentId(), hello.GetSessionId(), false); err != nil {
		t.Fatal(err)
	}
	hello.SessionId = "replacement"
	hello.SessionIncarnation++
	if _, err := testDelivery(store).RegisterAgent(ctx, hello); err != nil {
		t.Fatal(err)
	}
	if err := testDelivery(store).ObserveAgentHeartbeat(ctx, hello.GetAgentId(), "test-session-node-1", false); !errors.Is(err, deliverycore.ErrStaleAgentSession) {
		t.Fatalf("old heartbeat: %v", err)
	}
	report := &agentv1.StatusReport{AgentId: hello.GetAgentId(), SessionId: "test-session-node-1", ObservationSequence: 1}
	if err := testDelivery(store).ObserveAgentStatus(ctx, hello.GetAgentId(), report); !errors.Is(err, deliverycore.ErrStaleAgentSession) {
		t.Fatalf("old report: %v", err)
	}
}

func TestLiveOwnerDatabaseOutageKeepsMemoryStopsPublication(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	hello := agentHello("node-1")
	if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
		t.Fatal(err)
	}
	if err := testDelivery(store).ObserveAgentHeartbeat(ctx, hello.GetAgentId(), hello.GetSessionId(), false); err != nil {
		t.Fatal(err)
	}
	store.publication.SetPublishing(false)
	closed := store.db
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := testDelivery(store).ObserveAgentHeartbeat(ctx, hello.GetAgentId(), hello.GetSessionId(), false); err != nil {
		t.Fatalf("heartbeat during database outage: %v", err)
	}
	session, ok := fixtureLive(store).Session(hello.GetAgentId())
	if !ok || !session.Reachable {
		t.Fatal("session lost during database outage")
	}
	if store.routing.live.Publishing() {
		t.Fatal("published without a fence")
	}
	if err := testDelivery(store).EvaluateObservedDeploymentForTest(ctx, "missing"); err == nil {
		// missing allocation is a no-op; journaled evaluation still cannot commit
	}
	if err := store.withProductTx(ctx, func(context.Context, *sql.Tx) error { return nil }); err == nil {
		t.Fatal("journal commit succeeded with closed database")
	}
}

// assertLiveAllocationsMatchDurable guards the invariant that the owner's
// in-memory allocation view is exactly the durable allocation set after every
// rollout mutation. A violation means live reads drift from the database.
func assertLiveAllocationsMatchDurable(t *testing.T, store *persistence, serviceID string) int {
	t.Helper()
	live := store.liveImplementation.AllocationsByService(serviceID)
	liveIDs := make([]string, 0, len(live))
	for _, alloc := range live {
		liveIDs = append(liveIDs, alloc.ID)
	}
	rows, err := store.db.QueryContext(context.Background(), `SELECT id FROM allocation_assignments WHERE service_id = $1 ORDER BY id`, serviceID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	durableIDs := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		durableIDs = append(durableIDs, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(liveIDs)
	sort.Strings(durableIDs)
	if !reflect.DeepEqual(liveIDs, durableIDs) {
		t.Fatalf("live allocations %v != durable allocations %v", liveIDs, durableIDs)
	}
	return len(liveIDs)
}

func TestOwnerLiveAllocationsMatchDurableAcrossRolloutMutations(t *testing.T) {
	store, _, service := createHealthyRollingService(t, 1, 1)
	ctx := context.Background()

	// create + release + initial rollout completion
	if count := assertLiveAllocationsMatchDurable(t, store, service.ID); count != 1 {
		t.Fatalf("allocations after create = %d, want 1", count)
	}

	// rolling replacement: a new allocation is added and the predecessor withdrawn
	if _, _, err := updateService(ctx, store, "user-1", service.ID, "", rollingTestSpec("example.test/web:b", 1, 1)); err != nil {
		t.Fatalf("updateService: %v", err)
	}
	if _, err := releaseEnvironmentServiceForTest(ctx, store, "user-1", service.EnvironmentID, service.ID); err != nil {
		t.Fatalf("releaseEnvironment: %v", err)
	}
	if count := assertLiveAllocationsMatchDurable(t, store, service.ID); count != 2 {
		t.Fatalf("allocations after release = %d, want 2 (overlap)", count)
	}

	target := allocationForGeneration(t, store, service.ID, 2)
	for _, alloc := range target {
		markRolloutAllocationReady(t, store, alloc)
	}
	probe := &rolloutIngressProbe{store: store}
	reconciler := newRolloutReconciler(newTestDelivery(store, nil, probe, nil), time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile replacement: %v", err)
	}
	if count := assertLiveAllocationsMatchDurable(t, store, service.ID); count != 2 {
		t.Fatalf("allocations after promote = %d, want 2 (withdrawing predecessor)", count)
	}

	// drain completion for the withdrawn predecessor
	markAllDrainingComplete(t, store, service.ID)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile drain: %v", err)
	}
	if count := assertLiveAllocationsMatchDurable(t, store, service.ID); count != 1 {
		t.Fatalf("allocations after drain = %d, want 1", count)
	}

	// delete marks the service for removal
	if err := deleteService(ctx, store, "user-1", service.ID); err != nil {
		t.Fatalf("deleteService: %v", err)
	}
	assertLiveAllocationsMatchDurable(t, store, service.ID)
}

// redirectConn follows explicit live-owner redirects between known replicas,
// mirroring the stress fixture's client. A mutation transport failure is not
// retried; only a FailedPrecondition redirect with a known peer is followed.
type redirectConn struct {
	grpc.ClientConnInterface
	peers map[string]grpc.ClientConnInterface
}

func (c redirectConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	conn := c.ClientConnInterface
	for attempt := 0; attempt <= len(c.peers); attempt++ {
		err := conn.Invoke(ctx, method, args, reply, opts...)
		if status.Code(err) != codes.FailedPrecondition {
			return err
		}
		addr, ok := deliverycore.ParseLiveOwnerRedirect(status.Convert(err).Message())
		if !ok {
			return err
		}
		next, ok := c.peers[addr]
		if !ok {
			return fmt.Errorf("live owner redirected outside the fixture: %q", addr)
		}
		conn = next
	}
	return status.Error(codes.Unavailable, "live owner redirect loop")
}

// TestNonOwnerReadRedirectsToOwnerOverGRPC guards that reads served from
// owner-local live allocation state redirect a standby replica to the live
// owner instead of failing with Internal or hanging until the RPC deadline.
func TestNonOwnerReadRedirectsToOwnerOverGRPC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	owner := startSystemControlPlane(t, systemControlPlaneOptions{withDashboard: true})
	standby := startSystemControlPlane(t, systemControlPlaneOptions{
		databaseURL: owner.cfg.Database.URL,
		stateDir:    owner.cfg.StateDir,
		standby:     true,
	})

	if held, _, err := standby.server.leases.Lookup(ctx, singletonLeaseName); err != nil || held {
		t.Fatalf("standby replica holds the singleton lease: held=%v err=%v", held, err)
	}

	identity, err := owner.server.EnsureDashboardClientIdentity(ctx, systemTestDashboardID)
	if err != nil {
		t.Fatalf("EnsureDashboardClientIdentity: %v", err)
	}
	ownerConn := newDashboardPlatformClientConn(t, owner.server.InternalAddr(), identity)
	defer ownerConn.Close()
	standbyConn := newDashboardPlatformClientConn(t, standby.server.InternalAddr(), identity)
	defer standbyConn.Close()

	peers := map[string]grpc.ClientConnInterface{
		owner.server.InternalAddr():   ownerConn,
		standby.server.InternalAddr(): standbyConn,
	}
	ownerClient := platformv1.NewPlatformServiceClient(ownerConn)
	redirectClient := platformv1.NewPlatformServiceClient(redirectConn{ClientConnInterface: standbyConn, peers: peers})
	standbyRaw := platformv1.NewPlatformServiceClient(standbyConn)

	delegatedCtx := metadata.AppendToOutgoingContext(ctx, userAssertionHeader, signedLiveUserAssertion(t, owner.server, "user-1"))
	project, err := ownerClient.CreateProject(delegatedCtx, &platformv1.CreateProjectRequest{Name: "redirect-test"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	environments, err := ownerClient.ListEnvironments(delegatedCtx, &platformv1.ListEnvironmentsRequest{ProjectId: project.GetId()})
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(environments.GetEnvironments()) != 1 {
		t.Fatalf("expected one environment, got %d", len(environments.GetEnvironments()))
	}
	environmentID := environments.GetEnvironments()[0].GetId()

	// A raw standby connection must itself redirect, so removing the
	// requireLiveOwner gate cannot pass this test via the auto-following client.
	if _, err := standbyRaw.ListServices(delegatedCtx, &platformv1.ListServicesRequest{EnvironmentId: environmentID}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("standby ListServices without redirect = %v, want FailedPrecondition", err)
	} else if addr, ok := deliverycore.ParseLiveOwnerRedirect(status.Convert(err).Message()); !ok || addr != owner.server.InternalAddr() {
		t.Fatalf("standby ListServices redirect = %q, want owner %q", status.Convert(err).Message(), owner.server.InternalAddr())
	}
	if _, err := standbyRaw.GetServiceStatus(delegatedCtx, &platformv1.GetServiceStatusRequest{ServiceId: "missing"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("standby GetServiceStatus without redirect = %v, want FailedPrecondition", err)
	}

	listed, err := redirectClient.ListServices(delegatedCtx, &platformv1.ListServicesRequest{EnvironmentId: environmentID})
	if err != nil {
		t.Fatalf("standby ListServices: %v", err)
	}
	if listed.GetIndex() == 0 {
		t.Fatal("standby ListServices returned zero index")
	}
	if len(listed.GetServices()) != 0 {
		t.Fatalf("expected empty service list, got %d", len(listed.GetServices()))
	}

	if _, err := redirectClient.GetService(delegatedCtx, &platformv1.GetServiceRequest{ServiceId: "missing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("standby GetService missing = %v, want NotFound", err)
	}
	if _, err := redirectClient.GetServiceStatus(delegatedCtx, &platformv1.GetServiceStatusRequest{ServiceId: "missing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("standby GetServiceStatus missing = %v, want NotFound", err)
	}
}
