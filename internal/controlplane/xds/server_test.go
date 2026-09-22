package xds

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

type adsClient struct {
	t      *testing.T
	client *Client
	recvd  chan recvResult
}

type recvResult struct {
	resp *discoveryv3.DiscoveryResponse
	err  error
}

func testServer(t *testing.T) (*Server, *bufconn.Listener) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server := NewServer(ctx)
	listener := bufconn.Listen(1 << 20)
	t.Cleanup(func() { _ = listener.Close() })
	grpcServer := server.GRPCServer()
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	return server, listener
}

func dialADS(t *testing.T, listener *bufconn.Listener, nodeID string) *adsClient {
	t.Helper()
	client, err := Dial(context.Background(), "passthrough://bufnet", nodeID,
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	c := &adsClient{t: t, client: client, recvd: make(chan recvResult, 16)}
	go func() {
		for {
			resp, err := client.Recv()
			c.recvd <- recvResult{resp: resp, err: err}
			if err != nil {
				return
			}
		}
	}()
	return c
}

func (c *adsClient) send(typeURL, version, nonce string, errDetail *rpcstatus.Status) {
	c.t.Helper()
	if err := c.client.Request(typeURL, version, nonce, errDetail); err != nil {
		c.t.Fatal(err)
	}
}

func (c *adsClient) recv() *discoveryv3.DiscoveryResponse {
	c.t.Helper()
	select {
	case r := <-c.recvd:
		if r.err != nil {
			c.t.Fatal(r.err)
		}
		return r.resp
	case <-time.After(5 * time.Second):
		c.t.Fatal("timed out waiting for xDS response")
		return nil
	}
}

func (c *adsClient) expectSilence(d time.Duration) {
	c.t.Helper()
	select {
	case r := <-c.recvd:
		c.t.Fatalf("expected no response, got %+v (err=%v)", r.resp, r.err)
	case <-time.After(d):
	}
}

func (c *adsClient) close() {
	c.t.Helper()
	if err := c.client.CloseSend(); err != nil {
		c.t.Fatal(err)
	}
}

func waitForApplied(t *testing.T, server *Server, nodeID string, types []string, version string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status := server.Status()
		node, ok := status.Nodes[nodeID]
		complete := ok && status.HasSnapshot && status.Version != ""
		if complete {
			for _, typeURL := range types {
				if node.Applied[typeURL] != version {
					complete = false
					break
				}
			}
		}
		if complete {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("node %s never applied %s: %+v", nodeID, version, status.Nodes[nodeID])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func mustBuild(t *testing.T, input BuildInput) *Snapshot {
	t.Helper()
	snap, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestServerServesConsistentSnapshotOverADS(t *testing.T) {
	t.Parallel()

	server, listener := testServer(t)
	snap := mustBuild(t, testInput())
	server.Publish(context.Background(), snap)

	client := dialADS(t, listener, "envoy-1")
	defer client.close()

	// A rollout must never publish partially: every type arrives at the same
	// content version.
	for _, typeURL := range []string{
		resourcev3.ListenerType, resourcev3.ClusterType,
		resourcev3.RouteType, resourcev3.EndpointType,
	} {
		client.send(typeURL, "", "", nil)
		resp := client.recv()
		if resp.GetVersionInfo() != snap.Version {
			t.Fatalf("type %s: version %s, want %s", typeURL, resp.GetVersionInfo(), snap.Version)
		}
		if resp.GetTypeUrl() != typeURL {
			t.Fatalf("type %s: response type %s", typeURL, resp.GetTypeUrl())
		}
		client.send(typeURL, resp.GetVersionInfo(), resp.GetNonce(), nil)
	}

	waitForApplied(t, server, "envoy-1", []string{
		resourcev3.ListenerType, resourcev3.ClusterType,
		resourcev3.RouteType, resourcev3.EndpointType,
	}, snap.Version)
}

func TestServerNACKRetainsLastKnownGood(t *testing.T) {
	t.Parallel()

	server, listener := testServer(t)
	snap := mustBuild(t, testInput())
	server.Publish(context.Background(), snap)

	client := dialADS(t, listener, "envoy-1")
	defer client.close()
	client.send(resourcev3.ListenerType, "", "", nil)
	resp := client.recv()
	if resp.GetVersionInfo() != snap.Version {
		t.Fatalf("version = %s, want %s", resp.GetVersionInfo(), snap.Version)
	}
	// The client rejects the snapshot it cannot apply.
	client.send(resourcev3.ListenerType, "", resp.GetNonce(), &rpcstatus.Status{
		Code:    13,
		Message: "invalid listener: test NACK",
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		status := server.Status()
		if node, ok := status.Nodes["envoy-1"]; ok && node.NACKs == 1 {
			if node.LastNACK == "" {
				t.Fatal("NACK recorded without a message")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("NACK not recorded: %+v", server.Status().Nodes)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Last-known-good is retained: a fresh subscriber still converges on the
	// published snapshot.
	fresh := dialADS(t, listener, "envoy-2")
	defer fresh.close()
	fresh.send(resourcev3.ListenerType, "", "", nil)
	if got := fresh.recv().GetVersionInfo(); got != snap.Version {
		t.Fatalf("fresh subscriber version = %s, want %s", got, snap.Version)
	}
}

func TestServerReseedsNodeAfterRestart(t *testing.T) {
	t.Parallel()

	server, listener := testServer(t)
	v1 := mustBuild(t, testInput())
	server.Publish(context.Background(), v1)

	client := dialADS(t, listener, "envoy-1")
	client.send(resourcev3.ClusterType, "", "", nil)
	resp := client.recv()
	client.send(resourcev3.ClusterType, resp.GetVersionInfo(), resp.GetNonce(), nil)
	client.close()

	// Publish while the node is gone, then reconnect with the same node ID.
	changed := testInput()
	changed.Backends = append(changed.Backends, Backend{Domain: "c.example.com", Upstream: "10.0.0.20:8080"})
	v2 := mustBuild(t, changed)
	server.Publish(context.Background(), v2)

	reconnected := dialADS(t, listener, "envoy-1")
	defer reconnected.close()
	reconnected.send(resourcev3.ClusterType, "", "", nil)
	if got := reconnected.recv().GetVersionInfo(); got != v2.Version {
		t.Fatalf("reconnected version = %s, want %s", got, v2.Version)
	}
}

func TestServerDelayedApplyConverges(t *testing.T) {
	t.Parallel()

	server, listener := testServer(t)
	v1 := mustBuild(t, testInput())
	server.Publish(context.Background(), v1)

	client := dialADS(t, listener, "envoy-1")
	defer client.close()
	client.send(resourcev3.RouteType, "", "", nil)
	first := client.recv()
	if first.GetVersionInfo() != v1.Version {
		t.Fatalf("first version = %s, want %s", first.GetVersionInfo(), v1.Version)
	}

	// The client applies slowly: V2 publishes before the V1 ACK arrives.
	changed := testInput()
	changed.Backends = changed.Backends[:1]
	v2 := mustBuild(t, changed)
	server.Publish(context.Background(), v2)

	client.send(resourcev3.RouteType, first.GetVersionInfo(), first.GetNonce(), nil)
	second := client.recv()
	if second.GetVersionInfo() != v2.Version {
		t.Fatalf("delayed apply converged to %s, want %s", second.GetVersionInfo(), v2.Version)
	}
	client.send(resourcev3.RouteType, second.GetVersionInfo(), second.GetNonce(), nil)
	waitForApplied(t, server, "envoy-1", []string{resourcev3.RouteType}, v2.Version)
}

func TestServerHoldsStreamsUntilFirstPublish(t *testing.T) {
	t.Parallel()

	server, listener := testServer(t)
	client := dialADS(t, listener, "envoy-1")
	defer client.close()
	client.send(resourcev3.ListenerType, "", "", nil)
	client.expectSilence(100 * time.Millisecond)

	snap := mustBuild(t, testInput())
	server.Publish(context.Background(), snap)
	if got := client.recv().GetVersionInfo(); got != snap.Version {
		t.Fatalf("version = %s, want %s", got, snap.Version)
	}
}

// TestServerCancelsFirstContactWorkOnStreamClose pins the resource bound the
// finding asks for: durable registration and the pre-serve refresh run under
// the stream's context, so a client disconnecting mid-registration aborts the
// database work instead of leaving it running against a stalled backend.
func TestServerCancelsFirstContactWorkOnStreamClose(t *testing.T) {
	t.Parallel()

	nodes := &blockingNodes{started: make(chan struct{}), cancelled: make(chan error, 1)}
	server, listener := testServer(t)
	server.SetNodeStore(nodes)
	server.Publish(context.Background(), mustBuild(t, testInput()))

	client := dialADS(t, listener, "envoy-blocked")
	client.send(resourcev3.ListenerType, "", "", nil)
	select {
	case <-nodes.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first-contact registration never started")
	}

	// Drop the whole connection (CloseSend alone leaves the stream context
	// alive): the transport cancels the stream context and the blocked
	// registration must unwind with it.
	if err := client.client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-nodes.cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("registration ended with %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked first-contact work outlived its stream")
	}
}

// TestServerRejectsRequestsWithoutStreamContext: a stream request with no
// opened stream has no cancellable lifetime and must fail closed rather than
// run first-contact work that a disconnect could orphan.
func TestServerRejectsRequestsWithoutStreamContext(t *testing.T) {
	t.Parallel()

	server := NewServer(context.Background())
	server.SetNodeStore(failingNodes{})
	err := server.onStreamRequest(7, &discoveryv3.DiscoveryRequest{
		Node:    &corev3.Node{Id: "envoy-x"},
		TypeUrl: resourcev3.EndpointType,
	})
	if err == nil {
		t.Fatal("expected a request on an untracked stream to fail closed")
	}
}

type blockingNodes struct {
	started   chan struct{}
	cancelled chan error
	once      sync.Once
}

func (b *blockingNodes) UpsertNodeObservations(ctx context.Context, _ []NodeObservation) error {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	err := ctx.Err()
	b.cancelled <- err
	return err
}

func (b *blockingNodes) ListNodeObservations(context.Context) ([]NodeObservation, error) {
	return nil, nil
}
