//go:build integration

package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/xds"
)

func TestIngressHandoverRetainsPublicationUntilFreshReports(t *testing.T) {
	store, _, service := createHealthyRollingService(t, 2, 1)
	ctx := context.Background()
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, testUser("user-1"), "web.example.com", service.ID, 8080); err != nil {
		t.Fatal(err)
	}
	allocations := mustRolloutAllocations(t, store, service.ID)
	if len(allocations) != 2 {
		t.Fatalf("allocations = %v", allocations)
	}
	oldServer := xds.NewServer(ctx)
	oldPublisher := testXDSPublisher(store, oldServer, "former-owner")
	if err := oldPublisher.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	accepted, err := store.routing.LoadPublication(ctx)
	if err != nil || accepted.Version == "" {
		t.Fatalf("initial publication = %+v, %v", accepted, err)
	}

	next, err := openPersistence(config.DatabaseConfig{
		URL: sharedTestDatabase(t), MaxOpenConns: 4, MaxIdleConns: 4,
	}, testMeshConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	newServer := xds.NewServer(ctx)
	newPublisher := testXDSPublisher(next, newServer, "replacement-owner")
	if err := newPublisher.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := newPublisher.Sync(ctx); !errors.Is(err, deliverycore.ErrNotLiveOwner) {
		t.Fatalf("standby publication = %v", err)
	}

	// Take over while the former process still believes it owns live state.
	// A request without a lease token must still fence that process in SQL.
	if _, err := store.db.ExecContext(ctx, `UPDATE control_plane_leases SET expires_at = statement_timestamp() WHERE name = $1`, singletonLeaseName); err != nil {
		t.Fatal(err)
	}
	lease := newLeaseManager(next.database, time.Minute, time.Millisecond)
	ownerCtx, release, err := lease.hold(ctx, singletonLeaseName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	if err := oldPublisher.Sync(ctx); !errors.Is(err, errLeaseLost) {
		t.Fatalf("former owner publication = %v", err)
	}
	owner := testDelivery(next)
	if err := owner.BecomeLive(ownerCtx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.ResignLive)
	assertRetained := func() {
		t.Helper()
		if err := newPublisher.Sync(ctx); !errors.Is(err, deliverycore.ErrIngressObservationsPending) {
			t.Fatalf("premature replacement publication = %v", err)
		}
		pub, err := next.routing.LoadPublication(ctx)
		if err != nil || pub.Version != accepted.Version || pub.Publisher != accepted.Publisher || string(pub.Inputs) != string(accepted.Inputs) {
			t.Fatalf("accepted publication replaced: %+v, %v", pub, err)
		}
		for _, server := range []*xds.Server{oldServer, newServer} {
			if status := server.Status(); status.Version != accepted.Version || status.Counts.Endpoints != 2 {
				t.Fatalf("handover changed served configuration: %+v", status)
			}
		}
	}
	assertRetained()
	for _, alloc := range allocations {
		if err := next.liveImplementation.BeginSession(alloc.AgentID, "fresh-session", []string{alloc.ID}, []string{alloc.ID}, true); err != nil {
			t.Fatal(err)
		}
		if err := owner.ObserveAgentHeartbeat(ctx, alloc.AgentID, "fresh-session", false); err != nil {
			t.Fatal(err)
		}
	}
	assertRetained()
	// Missing health must not let the failover loop withdraw these endpoints.
	if err := next.liveImplementation.EndSession(allocations[1].AgentID, "fresh-session"); err != nil {
		t.Fatal(err)
	}
	owner.failoverNow = func() time.Time { return time.Now().UTC().Add(2 * deliverycore.AgentHealthyTTL) }
	if result, err := owner.ReconcileFailover(ownerCtx, deliverycore.AgentHealthyTTL); err != nil || result.IngressChanged {
		t.Fatalf("unknown workload failed over: %+v, %v", result, err)
	}
	assertRetained()
	for index, alloc := range allocations {
		report := &agentv1.StatusReport{Services: []*agentv1.ServiceCondition{{
			AllocationId: alloc.ID, ServiceId: service.ID,
			DesiredSpecRevision: alloc.DesiredSpecRevision, AppliedSpecRevision: alloc.DesiredSpecRevision,
			DesiredRolloutGeneration: alloc.DesiredRolloutGeneration, AppliedRolloutGeneration: alloc.DesiredRolloutGeneration,
			Healthy: index == 0, Phase: "Healthy", HealthyIpv6Ports: []int32{8080},
		}}}
		if index == 1 {
			report.Services[0].Phase = "Unhealthy"
			report.Services[0].HealthyIpv6Ports = nil
		}
		if _, _, err := owner.recordStatusReport(ownerCtx, alloc.AgentID, report); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			assertRetained()
		}
	}
	if err := newPublisher.Sync(ctx); err != nil {
		t.Fatalf("fresh replacement publication: %v", err)
	}
	if status := newServer.Status(); status.Version == accepted.Version || status.Counts.Endpoints != 1 {
		t.Fatalf("fresh health did not replace configuration: %+v", status)
	}
	if err := oldPublisher.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	if oldServer.Status().Version != newServer.Status().Version {
		t.Fatal("former owner did not follow accepted replacement")
	}
}
