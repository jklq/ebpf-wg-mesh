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

type Client struct {
	nodeID string
	conn   *grpc.ClientConn
	stream discoveryv3.AggregatedDiscoveryService_StreamAggregatedResourcesClient
	sent   bool
}

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
// Envoy sends node only on the first request of a stream.
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

func (c *Client) Subscribe(ctx context.Context, typeURL string) (*discoveryv3.DiscoveryResponse, error) {
	if err := c.Request(typeURL, "", "", nil); err != nil {
		return nil, err
	}
	return c.RecvContext(ctx)
}

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

func (c *Client) CloseSend() error { return c.stream.CloseSend() }

func (c *Client) Close() error { return c.conn.Close() }
