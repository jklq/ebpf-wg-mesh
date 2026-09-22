package xds

import (
	"bytes"
	"sort"
	"testing"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	cachetypes "github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/proto"
)

func testInput() BuildInput {
	return BuildInput{
		Backends: []Backend{
			{Domain: "B.example.com", Upstream: "10.0.0.11:8080", AllocationID: "alloc-2"},
			{Domain: "a.example.com", Upstream: "10.0.0.10:8080", AllocationID: "alloc-1"},
			{Domain: "a.example.com", Upstream: "10.0.0.12:8080", AllocationID: "alloc-3"},
		},
		Static: []StaticRoute{{
			Hosts:    []string{"console.example.test"},
			Upstream: "host.docker.internal:3000",
		}},
		ListenAddrs: []string{":8080"},
	}
}

func snapshotBytes(t *testing.T, snap *Snapshot) []byte {
	t.Helper()
	opts := proto.MarshalOptions{Deterministic: true}
	var blobs [][]byte
	for _, resources := range snap.CacheSnapshot().Resources {
		names := make([]string, 0, len(resources.Items))
		for name := range resources.Items {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			raw, err := opts.Marshal(resources.Items[name].Resource)
			if err != nil {
				t.Fatalf("marshal %s: %v", name, err)
			}
			blobs = append(blobs, raw)
		}
	}
	return bytes.Join(blobs, []byte{0})
}

func TestBuildIsDeterministic(t *testing.T) {
	t.Parallel()

	first, err := Build(testInput())
	if err != nil {
		t.Fatal(err)
	}
	wantVersion := first.Version
	wantBytes := snapshotBytes(t, first)

	// Rebuild from reversed inputs: racing replicas must produce identical
	// bytes and therefore converge on one version.
	reversed := testInput()
	for i, j := 0, len(reversed.Backends)-1; i < j; i, j = i+1, j-1 {
		reversed.Backends[i], reversed.Backends[j] = reversed.Backends[j], reversed.Backends[i]
	}
	for i := 0; i < 25; i++ {
		input := reversed
		if i%2 == 0 {
			input = testInput()
		}
		got, err := Build(input)
		if err != nil {
			t.Fatal(err)
		}
		if got.Version != wantVersion {
			t.Fatalf("build %d: version %s, want %s", i, got.Version, wantVersion)
		}
		if !bytes.Equal(snapshotBytes(t, got), wantBytes) {
			t.Fatalf("build %d: snapshot bytes differ", i)
		}
	}
}

func TestBuildFromInputsReproducesSnapshot(t *testing.T) {
	t.Parallel()

	snap, err := Build(testInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Inputs) == 0 {
		t.Fatal("Build must record its canonical inputs")
	}
	// A replica that never saw the live state rebuilds the exact snapshot
	// from the published hash preimage.
	rebuilt, err := BuildFromInputs(snap.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Version != snap.Version || rebuilt.Hash != snap.Hash || rebuilt.Counts != snap.Counts {
		t.Fatalf("rebuilt %+v, want version %s counts %+v", rebuilt, snap.Version, snap.Counts)
	}
	if !bytes.Equal(rebuilt.Inputs, snap.Inputs) {
		t.Fatal("rebuilt snapshot must carry identical inputs")
	}
	if !bytes.Equal(snapshotBytes(t, rebuilt), snapshotBytes(t, snap)) {
		t.Fatal("rebuilt snapshot resources differ")
	}

	for _, raw := range [][]byte{nil, {}, []byte("not json"), []byte(`{"domains":[{"name":"x","endpoints":["nope"]}]}`)} {
		if _, err := BuildFromInputs(raw); err == nil {
			t.Fatalf("BuildFromInputs(%q): expected error", raw)
		}
	}
}

func TestBuildGoldenVersion(t *testing.T) {
	t.Parallel()

	snap, err := Build(testInput())
	if err != nil {
		t.Fatal(err)
	}
	// A fixed input must hash to a fixed version across processes and
	// replicas. If this changes deliberately, the shape changed: say so.
	const want = "12210c47172b093efdcad005bd745cec1283ab3c1e32638678ff36e7f29decc9"
	if snap.Version != want {
		t.Fatalf("version = %s, want %s", snap.Version, want)
	}
}

func TestBuildRoutesMultipleReplicasPerService(t *testing.T) {
	t.Parallel()

	snap, err := Build(testInput())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.Domains != 3 {
		t.Fatalf("domains = %d, want 3 (two dynamic plus one static)", snap.Counts.Domains)
	}
	if snap.Counts.Endpoints != 3 {
		t.Fatalf("endpoints = %d, want 3", snap.Counts.Endpoints)
	}

	endpointItems := endpointResources(t, snap)
	cla, ok := endpointItems["endpoints/a.example.com"]
	if !ok {
		t.Fatalf("missing EDS resource, have %v", keys(endpointItems))
	}
	var got []string
	for _, locality := range cla.GetEndpoints() {
		for _, endpoint := range locality.GetLbEndpoints() {
			addr := endpoint.GetEndpoint().GetAddress().GetSocketAddress()
			got = append(got, addr.GetAddress())
		}
	}
	if len(got) != 2 || got[0] != "10.0.0.10" || got[1] != "10.0.0.12" {
		t.Fatalf("endpoints = %v, want sorted [10.0.0.10 10.0.0.12]", got)
	}

	clusters := clusterResources(t, snap)
	cluster, ok := clusters["backend/a.example.com"]
	if !ok {
		t.Fatalf("missing cluster, have %v", keys(clusters))
	}
	if cluster.GetType() != clusterv3.Cluster_EDS {
		t.Fatalf("cluster type = %v, want EDS", cluster.GetType())
	}
	if got := cluster.GetEdsClusterConfig().GetServiceName(); got != "endpoints/a.example.com" {
		t.Fatalf("EDS service name = %q", got)
	}

	routes := routeResources(t, snap)
	routeConfig, ok := routes[RouteConfigName]
	if !ok {
		t.Fatalf("missing route config %q", RouteConfigName)
	}
	vhostByName := make(map[string]*routev3.VirtualHost)
	for _, vhost := range routeConfig.GetVirtualHosts() {
		vhostByName[vhost.GetName()] = vhost
	}
	vhost, ok := vhostByName["vhost/a.example.com"]
	if !ok {
		t.Fatalf("missing vhost, have %v", keys(vhostByName))
	}
	if len(vhost.GetDomains()) != 2 {
		t.Fatalf("domains = %v, want bare plus host:port forms", vhost.GetDomains())
	}
	route := vhost.GetRoutes()[0]
	if got := route.GetRoute().GetClusterSpecifier().(*routev3.RouteAction_Cluster); got == nil || got.Cluster != "backend/a.example.com" {
		t.Fatalf("route cluster = %+v", route.GetRoute().GetClusterSpecifier())
	}

	static, ok := clusters["static/0-console.example.test"]
	if !ok {
		t.Fatalf("missing static cluster, have %v", keys(clusters))
	}
	if static.GetType() != clusterv3.Cluster_STRICT_DNS {
		t.Fatalf("static cluster type = %v, want STRICT_DNS", static.GetType())
	}
}

func TestBuildOmitsDomainsWithoutValidEndpoints(t *testing.T) {
	t.Parallel()

	snap, err := Build(BuildInput{
		Backends: []Backend{
			{Domain: "bad.example.com", Upstream: "not-an-endpoint", AllocationID: "alloc-1"},
			{Domain: "good.example.com", Upstream: "10.0.0.10:8080", AllocationID: "alloc-2"},
		},
		ListenAddrs: []string{":8080"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name := range endpointResources(t, snap) {
		if name == "endpoints/bad.example.com" {
			t.Fatal("domain without a valid endpoint must not be routed")
		}
	}
	if _, ok := endpointResources(t, snap)["endpoints/good.example.com"]; !ok {
		t.Fatal("healthy domain missing from EDS")
	}
}

func TestBuildEmptySnapshotIsConsistent(t *testing.T) {
	t.Parallel()

	snap, err := Build(BuildInput{ListenAddrs: []string{":8080"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := snap.CacheSnapshot().Consistent(); err != nil {
		t.Fatalf("empty snapshot inconsistent: %v", err)
	}
	if len(routeResources(t, snap)[RouteConfigName].GetVirtualHosts()) != 0 {
		t.Fatal("empty snapshot must carry an empty route table")
	}
	if len(listenerResources(t, snap)) != 1 {
		t.Fatal("empty snapshot must still serve its listeners")
	}
}

func TestBuildRejectsBadListenAddr(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{"", "not-an-addr", ":0", ":99999", "example.com:http"} {
		if _, err := Build(BuildInput{ListenAddrs: []string{addr}}); err == nil {
			t.Fatalf("addr %q: expected error", addr)
		}
	}
	if _, err := Build(BuildInput{}); err == nil {
		t.Fatal("expected error with no listen addrs")
	}
}

func TestBuildVersionChangesWithEndpoints(t *testing.T) {
	t.Parallel()

	before, err := Build(testInput())
	if err != nil {
		t.Fatal(err)
	}
	changed := testInput()
	changed.Backends = append(changed.Backends, Backend{Domain: "c.example.com", Upstream: "10.0.0.20:8080"})
	after, err := Build(changed)
	if err != nil {
		t.Fatal(err)
	}
	if before.Version == after.Version {
		t.Fatal("endpoint change must produce a new version")
	}
}

func endpointResources(t *testing.T, snap *Snapshot) map[string]*endpointv3.ClusterLoadAssignment {
	t.Helper()
	out := make(map[string]*endpointv3.ClusterLoadAssignment)
	for name, item := range itemsFor(t, snap, resourcev3.EndpointType) {
		cla, ok := item.Resource.(*endpointv3.ClusterLoadAssignment)
		if !ok {
			t.Fatalf("EDS item %s has type %T", name, item.Resource)
		}
		out[name] = cla
	}
	return out
}

func clusterResources(t *testing.T, snap *Snapshot) map[string]*clusterv3.Cluster {
	t.Helper()
	out := make(map[string]*clusterv3.Cluster)
	for name, item := range itemsFor(t, snap, resourcev3.ClusterType) {
		cluster, ok := item.Resource.(*clusterv3.Cluster)
		if !ok {
			t.Fatalf("cluster item %s has type %T", name, item.Resource)
		}
		out[name] = cluster
	}
	return out
}

func routeResources(t *testing.T, snap *Snapshot) map[string]*routev3.RouteConfiguration {
	t.Helper()
	out := make(map[string]*routev3.RouteConfiguration)
	for name, item := range itemsFor(t, snap, resourcev3.RouteType) {
		routeConfig, ok := item.Resource.(*routev3.RouteConfiguration)
		if !ok {
			t.Fatalf("route item %s has type %T", name, item.Resource)
		}
		out[name] = routeConfig
	}
	return out
}

func listenerResources(t *testing.T, snap *Snapshot) map[string]*listenerv3.Listener {
	t.Helper()
	out := make(map[string]*listenerv3.Listener)
	for name, item := range itemsFor(t, snap, resourcev3.ListenerType) {
		listener, ok := item.Resource.(*listenerv3.Listener)
		if !ok {
			t.Fatalf("listener item %s has type %T", name, item.Resource)
		}
		out[name] = listener
	}
	return out
}

func itemsFor(t *testing.T, snap *Snapshot, typ resourcev3.Type) map[string]cachetypes.ResourceWithTTL {
	t.Helper()
	idx := cachev3.GetResponseType(typ)
	if idx == cachetypes.UnknownType {
		t.Fatalf("unknown response type for %s", typ)
	}
	return snap.CacheSnapshot().Resources[idx].Items
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestRequiredTypesOmitEndpointsWithoutEndpoints(t *testing.T) {
	t.Parallel()

	// Envoy opens EDS subscriptions only for the EDS clusters CDS announces.
	// With no endpoints the drain barrier must not wait for an EDS ACK that
	// can never arrive.
	snap := mustBuild(t, BuildInput{ListenAddrs: []string{":8080"}})
	if got := snap.RequiredTypes(); len(got) != 3 {
		t.Fatalf("RequiredTypes without endpoints = %v, want LDS/CDS/RDS only", got)
	}

	snap = mustBuild(t, BuildInput{
		Backends:    []Backend{{Domain: "a.example.com", Upstream: "10.0.0.10:8080"}},
		ListenAddrs: []string{":8080"},
	})
	if got := snap.RequiredTypes(); len(got) != 4 {
		t.Fatalf("RequiredTypes with endpoints = %v, want all four", got)
	}
}
