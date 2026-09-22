package xds

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

type fakeSource struct {
	backends []Backend
	calls    atomic.Int32
	guardErr error
	// firstStarted is closed when the first read starts; the read then
	// waits for blockFirst to close so bursts queue behind it.
	firstStarted chan struct{}
	blockFirst   chan struct{}
}

func (f *fakeSource) HealthyIngressBackends(context.Context) ([]Backend, error) {
	if f.calls.Add(1) == 1 && f.blockFirst != nil {
		close(f.firstStarted)
		<-f.blockFirst
	}
	return append([]Backend(nil), f.backends...), nil
}

func (f *fakeSource) WithLeaseGuard(_ context.Context, fn func() error) error {
	if f.guardErr != nil {
		return f.guardErr
	}
	return fn()
}

type fakePublications struct {
	mu     sync.Mutex
	pub    Publication
	writes int
}

func (f *fakePublications) LoadPublication(context.Context) (Publication, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pub, nil
}

func (f *fakePublications) CompareAndSwapPublication(_ context.Context, oldHash string, pub Publication) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pub.Hash != oldHash {
		return false, nil
	}
	f.pub = pub
	f.writes++
	return true, nil
}

type fakeNodes struct {
	mu      sync.Mutex
	nodes   map[string]NodeObservation
	flushes int
}

func newFakeNodes() *fakeNodes {
	return &fakeNodes{nodes: make(map[string]NodeObservation)}
}

func (f *fakeNodes) UpsertNodeObservations(_ context.Context, observations []NodeObservation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushes++
	for _, observation := range observations {
		stored := f.nodes[observation.NodeID]
		if observation.AppliedHash == "" {
			observation.AppliedHash = stored.AppliedHash
		}
		f.nodes[observation.NodeID] = observation
	}
	return nil
}

func (f *fakeNodes) ListNodeObservations(context.Context) ([]NodeObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]NodeObservation, 0, len(f.nodes))
	for _, node := range f.nodes {
		out = append(out, node)
	}
	return out, nil
}

func testPublisher(source *fakeSource, pubs *fakePublications) (*Publisher, *Server) {
	return testPublisherWithNodes(source, pubs, nil)
}

func testPublisherWithNodes(source *fakeSource, pubs *fakePublications, nodes *fakeNodes) (*Publisher, *Server) {
	ctx, cancel := context.WithCancel(context.Background())
	_ = cancel
	server := NewServer(ctx)
	publisher := NewPublisher(PublisherConfig{
		Source:       source,
		Publications: pubs,
		Nodes:        nodes,
		Server:       server,
		ListenAddrs:  []string{":8080"},
		PublisherID:  "replica-a",
	})
	return publisher, server
}

func TestPublisherPublishesOncePerContent(t *testing.T) {
	t.Parallel()

	source := &fakeSource{backends: []Backend{{Domain: "a.example.com", Upstream: "10.0.0.10:8080"}}}
	pubs := &fakePublications{}
	publisher, server := testPublisher(source, pubs)

	ctx := context.Background()
	if err := publisher.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	first := server.Status()
	if !first.HasSnapshot {
		t.Fatal("expected a published snapshot")
	}
	if pubs.writes != 1 {
		t.Fatalf("writes = %d, want 1", pubs.writes)
	}

	// Unchanged content republishes locally but does not rewrite the row.
	if err := publisher.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if pubs.writes != 1 {
		t.Fatalf("writes = %d, want 1 after unchanged sync", pubs.writes)
	}
	if got := server.Status().Version; got != first.Version {
		t.Fatalf("version = %s, want %s", got, first.Version)
	}

	// Changed content publishes a new version.
	source.backends = append(source.backends, Backend{Domain: "b.example.com", Upstream: "10.0.0.11:8080"})
	if err := publisher.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := server.Status().Version; got == first.Version {
		t.Fatal("expected a new version after the backend change")
	}
	if pubs.writes != 2 {
		t.Fatalf("writes = %d, want 2", pubs.writes)
	}
}

func TestPublisherAdoptsRacingWinner(t *testing.T) {
	t.Parallel()

	// Another owner already published this exact content (deterministic
	// bytes): adopt it without rewriting the row.
	backends := []Backend{{Domain: "a.example.com", Upstream: "10.0.0.10:8080"}}
	snap, err := Build(BuildInput{Backends: backends, ListenAddrs: []string{":8080"}})
	if err != nil {
		t.Fatal(err)
	}
	source := &fakeSource{backends: backends}
	pubs := &fakePublications{pub: Publication{Version: snap.Version, Hash: snap.Hash, Inputs: snap.Inputs, Publisher: "replica-b"}}
	publisher, server := testPublisher(source, pubs)

	if err := publisher.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pubs.writes != 0 {
		t.Fatalf("writes = %d, want 0 (adopt, not rewrite)", pubs.writes)
	}
	if got := server.Status().Version; got != snap.Version {
		t.Fatalf("served version = %s, want %s", got, snap.Version)
	}
}

func TestPublisherFencedAfterTakeover(t *testing.T) {
	t.Parallel()

	source := &fakeSource{
		backends: []Backend{{Domain: "a.example.com", Upstream: "10.0.0.10:8080"}},
		guardErr: errors.New("lease lost"),
	}
	pubs := &fakePublications{}
	publisher, server := testPublisher(source, pubs)

	if err := publisher.Sync(context.Background()); err == nil {
		t.Fatal("expected a lease-guard error from a stale owner")
	}
	if server.Status().HasSnapshot {
		t.Fatal("a fenced owner must not serve a snapshot")
	}
	if pubs.writes != 0 {
		t.Fatal("a fenced owner must not write the publication row")
	}
}

func TestPublisherRequestSyncCoalescesBurst(t *testing.T) {
	t.Parallel()

	source := &fakeSource{firstStarted: make(chan struct{}), blockFirst: make(chan struct{})}
	pubs := &fakePublications{}
	ctx, cancel := context.WithCancel(context.Background())
	server := NewServer(ctx)
	publisher := NewPublisher(PublisherConfig{
		Source:       source,
		Publications: pubs,
		Server:       server,
		ListenAddrs:  []string{":8080"},
		PublisherID:  "replica-a",
		MinSync:      time.Hour,
	})

	runCtx, cancelRun := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- publisher.Run(runCtx) }()
	t.Cleanup(func() {
		cancelRun()
		<-runDone
		cancel()
	})

	deadline := time.Now().Add(5 * time.Second)
	select {
	case <-source.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("initial sync never started")
	}
	// The initial sync is blocked, so the whole burst must coalesce into
	// the single queued request.
	publisher.RequestSync()
	publisher.RequestSync()
	publisher.RequestSync()
	close(source.blockFirst)
	for source.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := source.calls.Load(); got != 2 {
		t.Fatalf("backend reads = %d, want exactly 2 (initial plus one coalesced)", got)
	}
}

func TestPublisherFollowAdoptsDurablePublication(t *testing.T) {
	t.Parallel()

	// The live owner computed and published this snapshot; this replica
	// holds no live state at all (a fenced source would fail every Sync).
	backends := []Backend{{Domain: "a.example.com", Upstream: "10.0.0.10:8080"}}
	ownerSnap := mustBuild(t, BuildInput{Backends: backends, ListenAddrs: []string{":8080"}})
	pubs := &fakePublications{pub: Publication{
		Version: ownerSnap.Version, Hash: ownerSnap.Hash, Inputs: ownerSnap.Inputs, Publisher: "replica-b",
	}}
	source := &fakeSource{guardErr: errors.New("not the live owner")}
	publisher, server := testPublisherWithNodes(source, pubs, nil)

	if err := publisher.Replicate(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := server.Status()
	if !status.HasSnapshot || status.Version != ownerSnap.Version {
		t.Fatalf("follower served %+v, want version %s", status, ownerSnap.Version)
	}

	// The follower must refuse storage whose inputs do not match the row
	// hash instead of serving a tampered snapshot.
	pubs.mu.Lock()
	pubs.pub.Inputs = []byte(`{"tampered":true}`)
	pubs.mu.Unlock()
	server2 := NewServer(context.Background())
	follower := NewPublisher(PublisherConfig{Publications: pubs, Server: server2})
	if err := follower.Replicate(context.Background()); err == nil {
		t.Fatal("expected tampered publication inputs to be rejected")
	}
	if server2.Status().HasSnapshot {
		t.Fatal("tampered publication must not be served")
	}
}

func TestPublisherFollowFlushesNodeObservations(t *testing.T) {
	t.Parallel()

	backends := []Backend{{Domain: "a.example.com", Upstream: "10.0.0.10:8080"}}
	pubs := &fakePublications{}
	nodes := newFakeNodes()
	publisher, server := testPublisherWithNodes(&fakeSource{backends: backends}, pubs, nodes)
	if err := publisher.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	version := server.Status().Version

	// A disconnected Envoy that never fully applied keeps its stale hash
	// even after it disappears from the local server view.
	nodes.nodes["envoy-stale"] = NodeObservation{NodeID: "envoy-stale", AppliedHash: "older"}
	if err := publisher.flushNodeObservations(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A connected Envoy mid-apply (only EDS ACKed) must not report the new
	// hash: it may still route with the old listeners and endpoints.
	if err := publisher.Replicate(context.Background()); err != nil {
		t.Fatal(err)
	}
	server.observe(0, "envoy-partial", resourcev3.EndpointType, version, nil)
	if err := publisher.flushNodeObservations(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Once every required type is ACKed at the served version the node is
	// fully applied.
	for _, typeURL := range server.Status().RequiredTypes {
		server.observe(0, "envoy-partial", typeURL, version, nil)
	}
	if err := publisher.flushNodeObservations(context.Background()); err != nil {
		t.Fatal(err)
	}
	nodes.mu.Lock()
	defer nodes.mu.Unlock()
	if got := nodes.nodes["envoy-stale"].AppliedHash; got != "older" {
		t.Fatalf("stale node hash = %q, want %q", got, "older")
	}
	if got := nodes.nodes["envoy-partial"].AppliedHash; got != version {
		t.Fatalf("partial node hash = %q, want %q", got, version)
	}
}

func TestPublisherConvergedRequiresEveryKnownNode(t *testing.T) {
	t.Parallel()

	backends := []Backend{{Domain: "a.example.com", Upstream: "10.0.0.10:8080"}}
	pubs := &fakePublications{}
	nodes := newFakeNodes()
	publisher, server := testPublisherWithNodes(&fakeSource{backends: backends}, pubs, nodes)
	ctx := context.Background()

	// Nothing published and no nodes: nothing to converge on.
	if converged, err := publisher.Converged(ctx); err != nil || !converged {
		t.Fatalf("Converged = %v, %v; want true", converged, err)
	}
	if err := publisher.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	version := server.Status().Version

	// No observed nodes: nothing has ever subscribed, so nothing can route
	// to withdrawn endpoints.
	if converged, err := publisher.Converged(ctx); err != nil || !converged {
		t.Fatalf("Converged = %v, %v; want true with no nodes", converged, err)
	}

	// A NACKing or disconnected Envoy stuck on an older hash blocks drains.
	if err := nodes.UpsertNodeObservations(ctx, []NodeObservation{{NodeID: "envoy-1", AppliedHash: "older"}}); err != nil {
		t.Fatal(err)
	}
	if converged, err := publisher.Converged(ctx); err != nil || converged {
		t.Fatalf("Converged = %v, %v; want false while a node holds stale config", converged, err)
	}

	// An observed node that never fully applied also blocks: it is mid-apply
	// and may still route with whatever it holds.
	if err := nodes.UpsertNodeObservations(ctx, []NodeObservation{{NodeID: "envoy-2", AppliedHash: ""}}); err != nil {
		t.Fatal(err)
	}
	if converged, err := publisher.Converged(ctx); err != nil || converged {
		t.Fatalf("Converged = %v, %v; want false while a node is mid-apply", converged, err)
	}

	// Every known node at the current hash converges.
	if err := nodes.UpsertNodeObservations(ctx, []NodeObservation{
		{NodeID: "envoy-1", AppliedHash: version},
		{NodeID: "envoy-2", AppliedHash: version},
	}); err != nil {
		t.Fatal(err)
	}
	if converged, err := publisher.Converged(ctx); err != nil || !converged {
		t.Fatalf("Converged = %v, %v; want true once all nodes applied", converged, err)
	}
}

func TestPublisherRegistersSubscriberBeforeServing(t *testing.T) {
	t.Parallel()

	backends := []Backend{{Domain: "a.example.com", Upstream: "10.0.0.10:8080"}}
	pubs := &fakePublications{}
	nodes := newFakeNodes()
	publisher, server := testPublisherWithNodes(&fakeSource{backends: backends}, pubs, nodes)
	ctx := context.Background()
	if err := publisher.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	server.SetNodeStore(nodes)

	// First contact durably registers the subscriber before any config can
	// reach it — no flush cycle involved. Only then may it receive the
	// snapshot that routes to live allocations.
	req := &discoveryv3.DiscoveryRequest{Node: &corev3.Node{Id: "envoy-fresh"}, TypeUrl: resourcev3.EndpointType}
	if err := server.onFetchRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	nodes.mu.Lock()
	got, ok := nodes.nodes["envoy-fresh"]
	nodes.mu.Unlock()
	if !ok || got.AppliedHash != "" {
		t.Fatalf("first contact observation = %+v (present=%t), want durable registration with empty hash", got, ok)
	}

	// A subscriber that has applied nothing is mid-apply: it must block the
	// drain barrier rather than be ignored as unknown.
	if converged, err := publisher.Converged(ctx); err != nil || converged {
		t.Fatalf("Converged = %v, %v; want false while a fresh subscriber holds no applied version", converged, err)
	}

	// A node-less follow-up ACK (Envoy sends node only on the first
	// request of a stream) must still update apply state.
	if err := server.onStreamRequest(7, req); err != nil {
		t.Fatal(err)
	}
	status := server.Status()
	version := status.Version
	for _, typeURL := range status.RequiredTypes {
		if err := server.onStreamRequest(7, &discoveryv3.DiscoveryRequest{TypeUrl: typeURL, VersionInfo: version}); err != nil {
			t.Fatal(err)
		}
	}
	server.mu.Lock()
	node := server.nodes["envoy-fresh"]
	server.mu.Unlock()
	if node == nil || !((NodeStatus{Applied: node.applied}).FullyApplied(version, status.RequiredTypes)) {
		t.Fatalf("node-less ACKs did not update apply state: %+v", node)
	}
}

func TestServerRejectsUntrackableSubscribers(t *testing.T) {
	t.Parallel()

	// If a subscriber cannot be recorded durably, it must not be served:
	// the drain barrier would otherwise pass under config it cannot see.
	server := NewServer(context.Background())
	server.SetNodeStore(failingNodes{})
	err := server.onFetchRequest(context.Background(),
		&discoveryv3.DiscoveryRequest{Node: &corev3.Node{Id: "envoy-x"}, TypeUrl: resourcev3.EndpointType})
	if err == nil {
		t.Fatal("expected registration failure to fail the request closed")
	}
}

type failingNodes struct{}

func (failingNodes) UpsertNodeObservations(context.Context, []NodeObservation) error {
	return errors.New("node store unavailable")
}

func (failingNodes) ListNodeObservations(context.Context) ([]NodeObservation, error) {
	return nil, errors.New("node store unavailable")
}

// movingPublications returns one publication on the first load and another
// afterwards: the durable row moved while an adoption was in flight.
type movingPublications struct {
	first Publication
	moved Publication
	loads int
	mu    sync.Mutex
}

func (f *movingPublications) LoadPublication(context.Context) (Publication, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	if f.loads == 1 {
		return f.first, nil
	}
	return f.moved, nil
}

func (f *movingPublications) CompareAndSwapPublication(_ context.Context, _ string, _ Publication) (bool, error) {
	return false, nil
}

func TestPublisherRefreshNeverServesMovedPastPublication(t *testing.T) {
	t.Parallel()

	stale := mustBuild(t, BuildInput{
		Backends:    []Backend{{Domain: "a.example.com", Upstream: "10.0.0.10:8080"}},
		ListenAddrs: []string{":8080"},
	})
	current := mustBuild(t, BuildInput{
		Backends:    []Backend{{Domain: "b.example.com", Upstream: "10.0.0.11:8080"}},
		ListenAddrs: []string{":8080"},
	})
	pubs := &movingPublications{
		first: Publication{Version: stale.Version, Hash: stale.Hash, Inputs: stale.Inputs},
		moved: Publication{Version: current.Version, Hash: current.Hash, Inputs: current.Inputs},
	}
	server := NewServer(context.Background())
	publisher := NewPublisher(PublisherConfig{Publications: pubs, Server: server})

	// The adoption races a fresh publication. It must end on the moved
	// row's version and never serve the stale build in between: a lagging
	// replica serving withdrawn config would stall or fool the drain gate.
	if err := publisher.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := server.Status()
	if !status.HasSnapshot || status.Version != current.Version {
		t.Fatalf("served %+v, want moved publication %s", status, current.Version)
	}
}

type recordingNodeStore struct {
	*fakeNodes
	events *[]string
}

func (f *recordingNodeStore) UpsertNodeObservations(ctx context.Context, observations []NodeObservation) error {
	*f.events = append(*f.events, "register")
	return f.fakeNodes.UpsertNodeObservations(ctx, observations)
}

func TestPublisherFirstContactRegistersThenRefreshesBeforeServing(t *testing.T) {
	t.Parallel()

	// A lagging follower holds no snapshot yet while the durable row
	// already carries the current publication.
	published := mustBuild(t, BuildInput{
		Backends:    []Backend{{Domain: "a.example.com", Upstream: "10.0.0.10:8080"}},
		ListenAddrs: []string{":8080"},
	})
	pubs := &fakePublications{pub: Publication{
		Version: published.Version, Hash: published.Hash, Inputs: published.Inputs,
	}}
	server := NewServer(context.Background())
	publisher := NewPublisher(PublisherConfig{Publications: pubs, Server: server})

	var events []string
	server.SetNodeStore(&recordingNodeStore{fakeNodes: newFakeNodes(), events: &events})
	server.SetFirstContactHook(func(ctx context.Context) error {
		events = append(events, "refresh")
		return publisher.Refresh(ctx)
	})

	// First contact must register durably (so the drain barrier sees this
	// subscriber even if its check races this request) and then serve the
	// durable publication, never the stale local cache.
	req := &discoveryv3.DiscoveryRequest{Node: &corev3.Node{Id: "envoy-fresh"}, TypeUrl: resourcev3.EndpointType}
	if err := server.onFetchRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0] != "register" || events[1] != "refresh" {
		t.Fatalf("first-contact events = %v, want [register refresh]", events)
	}
	if status := server.Status(); !status.HasSnapshot || status.Version != published.Version {
		t.Fatalf("first contact served %+v, want durable publication %s", status, published.Version)
	}
}

// flakyNodes fails its first upserts and then delegates: a transient store
// outage must not leave a node served untracked.
type flakyNodes struct {
	*fakeNodes
	failures int
}

func (f *flakyNodes) UpsertNodeObservations(ctx context.Context, observations []NodeObservation) error {
	if f.failures > 0 {
		f.failures--
		return errors.New("node store unavailable")
	}
	return f.fakeNodes.UpsertNodeObservations(ctx, observations)
}

func TestServerRetriesRegistrationAfterFailure(t *testing.T) {
	t.Parallel()

	published := mustBuild(t, BuildInput{
		Backends:    []Backend{{Domain: "a.example.com", Upstream: "10.0.0.10:8080"}},
		ListenAddrs: []string{":8080"},
	})
	pubs := &fakePublications{pub: Publication{
		Version: published.Version, Hash: published.Hash, Inputs: published.Inputs,
	}}
	server := NewServer(context.Background())
	publisher := NewPublisher(PublisherConfig{Publications: pubs, Server: server})
	nodes := &flakyNodes{fakeNodes: newFakeNodes(), failures: 1}
	refreshes := 0
	server.SetNodeStore(nodes)
	server.SetFirstContactHook(func(ctx context.Context) error {
		refreshes++
		return publisher.Refresh(ctx)
	})

	req := &discoveryv3.DiscoveryRequest{Node: &corev3.Node{Id: "envoy-x"}, TypeUrl: resourcev3.EndpointType}
	// First attempt hits a store outage: fail closed, but the node must not
	// be considered handled — the next request has to run the full
	// register-then-refresh sequence again instead of trusting memory.
	if err := server.onFetchRequest(context.Background(), req); err == nil {
		t.Fatal("expected registration failure to fail the request closed")
	}
	if err := server.onFetchRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	nodes.mu.Lock()
	got, ok := nodes.nodes["envoy-x"]
	nodes.mu.Unlock()
	if !ok || got.AppliedHash != "" {
		t.Fatalf("retried registration = %+v (present=%t), want durable row", got, ok)
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1 (the failed attempt must not mark the node served)", refreshes)
	}

	// Once registered, later requests skip the sequence.
	if err := server.onFetchRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1 after registration succeeded", refreshes)
	}
}

func TestPublisherConvergesWithoutEDSSubscription(t *testing.T) {
	t.Parallel()

	// A snapshot without endpoints (the last service was removed) leaves
	// Envoy with no EDS cluster and therefore no EDS subscription to ACK.
	// The drain barrier must still converge once the standing types apply.
	pubs := &fakePublications{}
	nodes := newFakeNodes()
	publisher, server := testPublisherWithNodes(&fakeSource{}, pubs, nodes)
	if err := publisher.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	version := server.Status().Version
	server.observe(0, "envoy-neds", resourcev3.ListenerType, version, nil)
	server.observe(0, "envoy-neds", resourcev3.ClusterType, version, nil)
	server.observe(0, "envoy-neds", resourcev3.RouteType, version, nil)

	if err := publisher.flushNodeObservations(context.Background()); err != nil {
		t.Fatal(err)
	}
	nodes.mu.Lock()
	got := nodes.nodes["envoy-neds"]
	nodes.mu.Unlock()
	if got.AppliedHash != version {
		t.Fatalf("applied hash = %q, want %q without any EDS ACK", got.AppliedHash, version)
	}
}
