package xds

import (
	"context"
	"fmt"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client is a minimal ADS subscriber: enough to converge on the published
// snapshot, ACK/NACK it, and observe versions. Tests and the VM harness
// probe use it; Envoy itself speaks the same protocol.
type Client struct {
	nodeID string
	conn   *grpc.ClientConn
	stream discoveryv3.AggregatedDiscoveryService_StreamAggregatedResourcesClient
	// sent marks that the stream has carried its first request: node only
	// travels on the first request of a stream, like Envoy.
	sent bool
}

// Dial opens an ADS stream to addr as nodeID. Callers may pass dial options
// (for example a bufconn dialer in tests); without transport credentials the
// client defaults to plaintext.
func Dial(ctx context.Context, addr, nodeID string, opts ...grpc.DialOption) (*Client, error) {
	opts = append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial xds %s: %w", addr, err)
	}
	stream, err := discoveryv3.NewAggregatedDiscoveryServiceClient(conn).StreamAggregatedResources(ctx)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("open ADS stream: %w", err)
	}
	return &Client{nodeID: nodeID, conn: conn, stream: stream}, nil
}

// Request sends one DiscoveryRequest. A nil errDetail ACKs version; a
// non-nil one NACKs with Envoy semantics (version stays at the last ACKed).
// Like Envoy, node travels only on the first request of the stream: the
// harness must exercise the standard node-less ACK/NACK path.
func (c *Client) Request(typeURL, version, nonce string, errDetail *rpcstatus.Status) error {
	var node *corev3.Node
	if !c.sent {
		node = &corev3.Node{Id: c.nodeID}
	}
	if err := c.stream.Send(&discoveryv3.DiscoveryRequest{
		Node:          node,
		TypeUrl:       typeURL,
		VersionInfo:   version,
		ResponseNonce: nonce,
		ErrorDetail:   errDetail,
	}); err != nil {
		return err
	}
	c.sent = true
	return nil
}

// Subscribe requests typeURL from scratch and returns the first response.
func (c *Client) Subscribe(ctx context.Context, typeURL string) (*discoveryv3.DiscoveryResponse, error) {
	if err := c.Request(typeURL, "", "", nil); err != nil {
		return nil, err
	}
	return c.RecvContext(ctx)
}

// Recv blocks for the next response on the stream.
func (c *Client) Recv() (*discoveryv3.DiscoveryResponse, error) {
	return c.stream.Recv()
}

// RecvContext blocks for the next response until ctx ends.
func (c *Client) RecvContext(ctx context.Context) (*discoveryv3.DiscoveryResponse, error) {
	type result struct {
		resp *discoveryv3.DiscoveryResponse
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := c.stream.Recv()
		ch <- result{resp: resp, err: err}
	}()
	select {
	case r := <-ch:
		return r.resp, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// CloseSend half-closes the stream; Close tears the connection down.
func (c *Client) CloseSend() error { return c.stream.CloseSend() }

// Close releases the connection.
func (c *Client) Close() error { return c.conn.Close() }

// NodeID reports the subscribed identity.
func (c *Client) NodeID() string { return c.nodeID }
