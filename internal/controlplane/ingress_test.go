//go:build integration

package controlplane

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/proto"

	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/xds"
)

func testXDSPublisher(store *persistence, server *xds.Server, publisherID string) *xds.Publisher {
	return xds.NewPublisher(xds.PublisherConfig{
		Source:       store.routing,
		Publications: store.routing,
		Nodes:        store.routing,
		Server:       server,
		ListenAddrs:  []string{":8080"},
		PublisherID:  publisherID,
	})
}

func serveXDSServer(t *testing.T, server *xds.Server) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := server.GRPCServer()
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String()
}

func subscribeType(t *testing.T, addr, nodeID, typeURL string) *discoveryv3.DiscoveryResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := xds.Dial(ctx, addr, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	resp, err := client.Subscribe(ctx, typeURL)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func edsEndpointCount(t *testing.T, snap *xds.Snapshot) int {
	t.Helper()
	items := snap.CacheSnapshot().Resources[cachev3.GetResponseType(resourcev3.EndpointType)].Items
	count := 0
	for name, item := range items {
		cla, ok := item.Resource.(*endpointv3.ClusterLoadAssignment)
		if !ok {
			t.Fatalf("EDS item %s has type %T", name, item.Resource)
		}
		for _, locality := range cla.GetEndpoints() {
			count += len(locality.GetLbEndpoints())
		}
	}
	return count
}

func endpointsFromEDS(t *testing.T, resp *discoveryv3.DiscoveryResponse) []string {
	t.Helper()
	var out []string
	for _, resource := range resp.GetResources() {
		cla := &endpointv3.ClusterLoadAssignment{}
		if err := resource.UnmarshalTo(cla); err != nil {
			t.Fatalf("unmarshal ClusterLoadAssignment: %v", err)
		}
		for _, locality := range cla.GetEndpoints() {
			for _, endpoint := range locality.GetLbEndpoints() {
				socket := endpoint.GetEndpoint().GetAddress().GetSocketAddress()
				out = append(out, net.JoinHostPort(socket.GetAddress(), fmt.Sprint(socket.GetPortValue())))
			}
		}
	}
	return out
}

func createHealthyBoundService(t *testing.T, hostname, allocationIP string, targetPort int32) (*persistence, string) {
	t.Helper()
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
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
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, testUser("user-1"), hostname, service.ID, targetPort); err != nil {
		t.Fatal(err)
	}
	if err := store.markAllocationHealthyForTest(ctx, service.ID, allocationIP, targetPort); err != nil {
		t.Fatal(err)
	}
	return store, service.ID
}

func TestXDSPublisherRoutesHealthyDomains(t *testing.T) {
	t.Parallel()

	store, _ := createHealthyBoundService(t, "demo.example.com", "10.0.0.10", 8080)
	ctx := context.Background()

	server := xds.NewServer(ctx)
	publisher := testXDSPublisher(store, server, "replica-a")
	if err := publisher.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	status := server.Status()
	if !status.HasSnapshot {
		t.Fatal("expected a published snapshot")
	}
	pub, err := store.routing.LoadPublication(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pub.Version != status.Version || pub.Version == "" || pub.Publisher != "replica-a" {
		t.Fatalf("publication row = %+v, status version %s", pub, status.Version)
	}

	addr := serveXDSServer(t, server)
	// Every type converges on the same content version.
	for _, typeURL := range []string{
		resourcev3.ListenerType, resourcev3.ClusterType,
		resourcev3.RouteType, resourcev3.EndpointType,
	} {
		resp := subscribeType(t, addr, "envoy-1", typeURL)
		if resp.GetVersionInfo() != status.Version {
			t.Fatalf("type %s: version %s, want %s", typeURL, resp.GetVersionInfo(), status.Version)
		}
	}
	eds := subscribeType(t, addr, "envoy-1", resourcev3.EndpointType)
	endpoints := endpointsFromEDS(t, eds)
	if len(endpoints) != 1 || endpoints[0] != "10.0.0.10:8080" {
		t.Fatalf("EDS endpoints = %v, want [10.0.0.10:8080]", endpoints)
	}
}

func TestXDSRequiresReportedHealthyTargetPort(t *testing.T) {
	t.Parallel()

	store, _ := createHealthyBoundService(t, "demo.example.com", "attacker.example", 9090)

	backends, err := store.routing.HealthyIngressBackends(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(backends) != 0 {
		t.Fatalf("expected unhealthy target port to be excluded, got %+v", backends)
	}
}

func TestXDSRemovesDrainingBackendsBeforeShutdown(t *testing.T) {
	t.Parallel()

	store, serviceID := createHealthyBoundService(t, "demo.example.com", "10.0.0.10", 8080)
	ctx := context.Background()

	server := xds.NewServer(ctx)
	publisher := testXDSPublisher(store, server, "replica-a")
	if err := publisher.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	v1 := server.Status().Version
	addr := serveXDSServer(t, server)
	if got := endpointsFromEDS(t, subscribeType(t, addr, "envoy-1", resourcev3.EndpointType)); len(got) != 1 {
		t.Fatalf("serving EDS endpoints = %v, want one", got)
	}

	// The replacement is ready and the predecessor starts draining while its
	// container still exists: EDS must drop it before destructive shutdown.
	if err := store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE allocation_assignments
			SET rollout_state = $1, updated_at = statement_timestamp()
			WHERE service_id = $2`, deliverycore.AllocationRolloutDraining, serviceID); err != nil {
			return err
		}
		return recordServiceAssignmentsAndRollout(ctx, tx, serviceID)
	}); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Sync(ctx); err != nil {
		t.Fatalf("Sync after drain: %v", err)
	}
	v2 := server.Status().Version
	if v2 == v1 {
		t.Fatal("draining transition must publish a new version")
	}
	if got := endpointsFromEDS(t, subscribeType(t, addr, "envoy-1", resourcev3.EndpointType)); len(got) != 0 {
		t.Fatalf("draining EDS endpoints = %v, want none", got)
	}
	var remaining int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM allocation_assignments WHERE service_id = $1`, serviceID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining == 0 {
		t.Fatal("draining allocation was destroyed before EDS removal")
	}
}

func TestXDSSplitOwnershipConverges(t *testing.T) {
	t.Parallel()

	store, _ := createHealthyBoundService(t, "demo.example.com", "10.0.0.10", 8080)
	ctx := context.Background()

	serverA := xds.NewServer(ctx)
	serverB := xds.NewServer(ctx)
	publisherA := testXDSPublisher(store, serverA, "replica-a")
	publisherB := testXDSPublisher(store, serverB, "replica-b")

	// Racing owners compute identical bytes and converge on one version;
	// the loser adopts the row instead of rewriting it.
	if err := publisherA.Sync(ctx); err != nil {
		t.Fatalf("Sync A: %v", err)
	}
	if err := publisherB.Sync(ctx); err != nil {
		t.Fatalf("Sync B: %v", err)
	}
	versionA, versionB := serverA.Status().Version, serverB.Status().Version
	if versionA == "" || versionA != versionB {
		t.Fatalf("versions diverged: A=%s B=%s", versionA, versionB)
	}
	pub, err := store.routing.LoadPublication(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pub.Version != versionA || pub.Publisher != "replica-a" {
		t.Fatalf("publication row = %+v, want winner replica-a at %s", pub, versionA)
	}
	addrA, addrB := serveXDSServer(t, serverA), serveXDSServer(t, serverB)
	marshal := proto.MarshalOptions{Deterministic: true}
	for _, typeURL := range []string{resourcev3.ListenerType, resourcev3.ClusterType, resourcev3.RouteType, resourcev3.EndpointType} {
		respA := subscribeType(t, addrA, "envoy-1", typeURL)
		respB := subscribeType(t, addrB, "envoy-1", typeURL)
		rawA, err := marshal.Marshal(respA)
		if err != nil {
			t.Fatal(err)
		}
		rawB, err := marshal.Marshal(respB)
		if err != nil {
			t.Fatal(err)
		}
		// Nonces differ per stream; versions and resources must not.
		respA.Nonce, respB.Nonce = "", ""
		rawA, _ = marshal.Marshal(respA)
		rawB, _ = marshal.Marshal(respB)
		if string(rawA) != string(rawB) {
			t.Fatalf("type %s: racing replicas served different bytes", typeURL)
		}
	}

	// A stale owner that has not observed takeover is fenced: no publish,
	// no row write, last-known-good retained.
	fenced := context.WithValue(ctx, leaseContextKey{}, leaseClaim{name: SingletonLeaseName, holder: "dead-owner", token: -1})
	if err := publisherB.Sync(fenced); err == nil {
		t.Fatal("expected a fenced owner to fail Sync")
	}
	if got := serverB.Status().Version; got != versionB {
		t.Fatalf("fenced owner changed served version to %s", got)
	}
}

func TestXDSPublicationRowCompareAndSwap(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	pub, err := store.routing.LoadPublication(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pub.Version != "" || pub.Publisher != "" || len(pub.Inputs) != 0 {
		t.Fatalf("expected no publication, got %+v", pub)
	}
	won, err := store.routing.CompareAndSwapPublication(ctx, "", xds.Publication{
		Version: "v1", Inputs: []byte(`{"v":1}`), Publisher: "replica-a",
	})
	if err != nil || !won {
		t.Fatalf("initial insert won=%v err=%v", won, err)
	}
	won, err = store.routing.CompareAndSwapPublication(ctx, "", xds.Publication{
		Version: "v2", Publisher: "replica-b",
	})
	if err != nil || won {
		t.Fatalf("stale insert won=%v err=%v, want loss", won, err)
	}
	won, err = store.routing.CompareAndSwapPublication(ctx, "v1", xds.Publication{
		Version: "v2", Inputs: []byte(`{"v":2}`), Publisher: "replica-b",
	})
	if err != nil || !won {
		t.Fatalf("CAS update won=%v err=%v", won, err)
	}
	pub, err = store.routing.LoadPublication(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pub.Version != "v2" || pub.Publisher != "replica-b" || string(pub.Inputs) != `{"v":2}` {
		t.Fatalf("publication = %+v", pub)
	}
}

func TestXDSFollowerServesPublicationWithoutLease(t *testing.T) {
	t.Parallel()

	store, _ := createHealthyBoundService(t, "demo.example.com", "10.0.0.10", 8080)
	ctx := context.Background()

	serverA := xds.NewServer(ctx)
	publisherA := testXDSPublisher(store, serverA, "replica-a")
	if err := publisherA.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	published := serverA.Status()

	// Replica B never held the live state and never ran the publish loop. It
	// still serves the durable publication.
	serverB := xds.NewServer(ctx)
	publisherB := testXDSPublisher(store, serverB, "replica-b")
	if err := publisherB.Replicate(ctx); err != nil {
		t.Fatalf("follow: %v", err)
	}
	status := serverB.Status()
	if !status.HasSnapshot || status.Version != published.Version {
		t.Fatalf("follower served %+v, want version %s", status, published.Version)
	}
	addrB := serveXDSServer(t, serverB)
	eds := subscribeType(t, addrB, "envoy-1", resourcev3.EndpointType)
	if eds.GetVersionInfo() != published.Version {
		t.Fatalf("follower EDS version %s, want %s", eds.GetVersionInfo(), published.Version)
	}
	if endpoints := endpointsFromEDS(t, eds); len(endpoints) != 1 || endpoints[0] != "10.0.0.10:8080" {
		t.Fatalf("follower EDS endpoints = %v", endpoints)
	}
}

func TestXDSFirstContactServesCurrentPublication(t *testing.T) {
	t.Parallel()

	store, _ := createHealthyBoundService(t, "demo.example.com", "10.0.0.10", 8080)
	ctx := context.Background()

	serverA := xds.NewServer(ctx)
	publisherA := testXDSPublisher(store, serverA, "replica-a")
	if err := publisherA.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	published := serverA.Status()

	// Replica B is lagging: it never ran its follow tick. A fresh
	// subscriber must still receive the current durable publication at
	// first contact — never the stale local cache.
	serverB := xds.NewServer(ctx)
	publisherB := testXDSPublisher(store, serverB, "replica-b")
	serverB.SetFirstContactHook(publisherB.Refresh)
	addrB := serveXDSServer(t, serverB)
	eds := subscribeType(t, addrB, "envoy-1", resourcev3.EndpointType)
	if eds.GetVersionInfo() != published.Version {
		t.Fatalf("first contact EDS version %s, want current publication %s", eds.GetVersionInfo(), published.Version)
	}
	if endpoints := endpointsFromEDS(t, eds); len(endpoints) != 1 || endpoints[0] != "10.0.0.10:8080" {
		t.Fatalf("first contact EDS endpoints = %v", endpoints)
	}
}

func TestXDSRegistersSubscriberDurablyAtFirstContact(t *testing.T) {
	t.Parallel()

	store, _ := createHealthyBoundService(t, "demo.example.com", "10.0.0.10", 8080)
	ctx := context.Background()

	server := xds.NewServer(ctx)
	server.SetNodeStore(store.routing)
	publisher := testXDSPublisher(store, server, "replica-a")
	if err := publisher.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	addr := serveXDSServer(t, server)
	subscribeType(t, addr, "envoy-1", resourcev3.EndpointType)

	// No follow loop has run: the observation exists only if first contact
	// registered it.
	nodes, err := store.routing.ListNodeObservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].NodeID != "envoy-1" || nodes[0].AppliedVersion != "" {
		t.Fatalf("first-contact observations = %+v, want envoy-1 registered with empty version", nodes)
	}
	if converged, err := publisher.Converged(ctx); err != nil || converged {
		t.Fatalf("Converged = %v, %v; want false while the subscriber has applied nothing", converged, err)
	}
}

func TestXDSNodeObservationRetention(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	// A node fully applies v2, then regresses to mid-apply: the stale
	// version it may route with must be retained until a full apply lands
	// again.
	if err := store.routing.UpsertNodeObservations(ctx, []xds.NodeObservation{
		{NodeID: "envoy-1", AppliedVersion: "v2", NACKs: 1, LastNACK: "bad eds"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.routing.UpsertNodeObservations(ctx, []xds.NodeObservation{
		{NodeID: "envoy-1", AppliedVersion: ""},
	}); err != nil {
		t.Fatal(err)
	}
	nodes, err := store.routing.ListNodeObservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].AppliedVersion != "v2" || nodes[0].NACKs != 1 || nodes[0].LastNACK != "bad eds" {
		t.Fatalf("observations = %+v, want retained v2 with NACK", nodes)
	}

	// A disconnected Envoy keeps its row: it keeps routing with its
	// last-known-good config until its ACK actually arrives.
	if err := store.routing.UpsertNodeObservations(ctx, []xds.NodeObservation{
		{NodeID: "envoy-2", AppliedVersion: "v2"},
	}); err != nil {
		t.Fatal(err)
	}
	nodes, err = store.routing.ListNodeObservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("observations = %+v, want both nodes retained", nodes)
	}
}

func agentHello(id string) *agentv1.AgentHello {
	sum := sha256.Sum256([]byte(id))
	host := fmt.Sprintf("fd00:30::%x", 0x10+int(sum[0]))
	return &agentv1.AgentHello{
		AgentId:                 id,
		Name:                    id,
		AdvertiseAddr:           host,
		WireguardPublicKey:      "test-public-key-" + id,
		WireguardListenPort:     51820,
		WireguardEndpoint:       net.JoinHostPort(host, "51820"),
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
