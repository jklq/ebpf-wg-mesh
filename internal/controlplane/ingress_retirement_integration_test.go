//go:build integration

package controlplane

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/ingressnodes"
	"ebof-wg-mesh/internal/controlplane/xds"
	"ebof-wg-mesh/internal/testutil"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

type retirementSource struct{ backends []xds.Backend }

func (s *retirementSource) IngressInputs(context.Context) (xds.Inputs, error) {
	return xds.Inputs{Backends: s.backends}, nil
}
func (*retirementSource) WithLeaseGuard(_ context.Context, fn func() error) error { return fn() }

func TestIngressRetirementUnblocksCertificateAndRolloutBarriers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)
	registry := ingressnodes.New(store.db)
	for _, id := range []string{"envoy-1", "envoy-2"} {
		if err := registry.Register(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	source := &retirementSource{backends: []xds.Backend{{Domain: "app.example.com", Upstream: "10.0.0.1:80"}}}
	server := xds.NewServer(ctx)
	server.SetNodeStore(registry)
	publisher := xds.NewPublisher(xds.PublisherConfig{Source: source, Nodes: registry, Publications: store.routing, Server: server, HTTPListenAddrs: []string{":8080"}})
	if err := publisher.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if count, converged, err := publisher.Applied(ctx); err != nil || count != 2 || converged {
		t.Fatalf("provisioned barrier = %d, %v, %v", count, converged, err)
	}
	address := serveXDSServer(t, server, store)
	newClient := func(id string) *xds.Client {
		t.Helper()
		material, err := identity.IssueClientCertificate(ctx, address.keys, identity.CallerIngress, id, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := xds.WriteIdentity(dir, dir, material); err != nil {
			t.Fatal(err)
		}
		cfg, err := xds.ClientTLS(dir, "controlplane")
		if err != nil {
			t.Fatal(err)
		}
		client, err := xds.Dial(ctx, address.address, id, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { client.Close() })
		return client
	}
	apply := func(client *xds.Client) {
		t.Helper()
		for _, typ := range server.Status().RequiredTypes {
			if err := client.Request(typ, "", "", nil); err != nil {
				t.Fatal(err)
			}
			deadline, cancel := context.WithTimeout(ctx, 3*time.Second)
			response, err := client.RecvContext(deadline)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Request(typ, response.VersionInfo, response.Nonce, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	subscribedTypes := server.Status().RequiredTypes
	live := newClient("envoy-1")
	gone := newClient("envoy-2")
	apply(live)
	apply(gone)
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 3 * time.Second}, func(ctx context.Context) (bool, error) { return publisher.Converged(ctx) }); err != nil {
		t.Fatal(err)
	}
	// Close transport only. This is a temporary outage until the operator retires it.
	gone.Close()
	source.backends = nil
	if err := publisher.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	version := server.Status().Version
	// Only the surviving Envoy ACKs the withdrawal; the absent active member blocks it.
	for range subscribedTypes {
		deadline, cancel := context.WithTimeout(ctx, 3*time.Second)
		response, err := live.RecvContext(deadline)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if err := live.Request(response.TypeUrl, response.VersionInfo, response.Nonce, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 3 * time.Second}, func(context.Context) (bool, error) {
		status := server.Status().Nodes["envoy-1"]
		return status.FullyApplied(version, server.Status().RequiredTypes), nil
	}); err != nil {
		t.Fatal(err)
	}
	if count, converged, err := publisher.Applied(ctx); err != nil || count != 2 || converged {
		t.Fatalf("disconnected certificate barrier = %d, %v, %v", count, converged, err)
	}
	if converged, err := publisher.Converged(ctx); err != nil || converged {
		t.Fatalf("temporary outage released drains: %v, %v", converged, err)
	}
	if err := registry.Retire(ctx, "envoy-2"); err != nil {
		t.Fatal(err)
	}
	if count, converged, err := publisher.Applied(ctx); err != nil || count != 1 || !converged {
		t.Fatalf("retired certificate barrier = %d, %v, %v", count, converged, err)
	}
	if converged, err := publisher.Converged(ctx); err != nil || !converged {
		t.Fatalf("retirement did not release drains: %v, %v", converged, err)
	}
	// A delayed report from another replica cannot recreate membership or its barrier.
	if err := registry.UpsertNodeObservations(ctx, []xds.NodeObservation{{NodeID: "envoy-2", AppliedVersion: "old"}, {NodeID: "unknown", AppliedVersion: "old"}}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(ctx, "envoy-2"); !errors.Is(err, ingressnodes.ErrRetired) {
		t.Fatalf("retired identity reactivation = %v", err)
	}
	// A registry reconstructed after restart sees the same tombstone.
	if active, err := ingressnodes.New(store.db).NodeActive(ctx, "envoy-2"); err != nil || active {
		t.Fatalf("retired membership = %v, %v", active, err)
	}
	reconnecting := newClient("envoy-2")
	deadline, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if response, err := reconnecting.Subscribe(deadline, resourcev3.EndpointType); err == nil {
		t.Fatalf("retired client retrieved configuration: %v", response)
	}
	if count, converged, err := publisher.Applied(ctx); err != nil || count != 1 || !converged {
		t.Fatalf("stale reports changed barrier = %d, %v, %v", count, converged, err)
	}
}

func TestConcurrentIngressRetirementCannotBeReactivatedByObservations(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	registry := ingressnodes.New(store.db)
	ctx := context.Background()
	if err := registry.Register(ctx, "envoy-1"); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	errorsCh := make(chan error, 2)
	workers.Add(2)
	go func() { defer workers.Done(); errorsCh <- registry.Retire(ctx, "envoy-1") }()
	go func() {
		defer workers.Done()
		for range 20 {
			if err := registry.UpsertNodeObservations(ctx, []xds.NodeObservation{{NodeID: "envoy-1", AppliedVersion: "stale"}}); err != nil {
				errorsCh <- err
				return
			}
		}
		errorsCh <- nil
	}()
	workers.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if observations, err := registry.ListNodeObservations(ctx); err != nil || len(observations) != 0 {
		t.Fatalf("retired observations = %v, %v", observations, err)
	}
	if err := registry.Register(ctx, "envoy-1"); !errors.Is(err, ingressnodes.ErrRetired) {
		t.Fatalf("retired registration = %v", err)
	}
}

func TestIngressRetirementReleasesActualAllocationDrain(t *testing.T) {
	store, _, service := createHealthyRollingService(t, 1, 1)
	ctx := context.Background()
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, testUser("user-1"), "web.example.com", service.ID, 8080); err != nil {
		t.Fatal(err)
	}
	old := allocationForGeneration(t, store, service.ID, 1)[0]
	if _, _, err := updateService(ctx, store, "user-1", service.ID, "", rollingTestSpec("example.test/web:b", 1, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := releaseEnvironmentServiceForTest(ctx, store, "user-1", service.EnvironmentID, service.ID); err != nil {
		t.Fatal(err)
	}
	markRolloutAllocationReady(t, store, allocationForGeneration(t, store, service.ID, 2)[0])
	registry := ingressnodes.New(store.db)
	if err := registry.Register(ctx, "removed-envoy"); err != nil {
		t.Fatal(err)
	}
	server := xds.NewServer(ctx)
	publisher := testXDSPublisher(store, server, "replica-a")
	delivery := newTestDelivery(store, nil, publisher, nil)
	reconciler := newRolloutReconciler(delivery, time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	before := allocationByID(t, store, service.ID, old.ID)
	if before.RolloutState != deliverycore.AllocationRolloutWithdrawing || before.DrainDeadline.Valid {
		t.Fatalf("disconnected ingress allowed drain: %+v", before)
	}
	if err := registry.Retire(ctx, "removed-envoy"); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	after := allocationByID(t, store, service.ID, old.ID)
	if after.RolloutState != deliverycore.AllocationRolloutDraining || !after.DrainDeadline.Valid {
		t.Fatalf("retirement did not release allocation drain: %+v", after)
	}
}
