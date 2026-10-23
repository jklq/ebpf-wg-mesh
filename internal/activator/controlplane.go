package activator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/xds"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// ControlPlaneConfig locates the control plane's internal listener and the
// activator's provisioned identity (same generation layout as ingress).
type ControlPlaneConfig struct {
	Addresses   []string
	IdentityDir string
	ServerName  string
}

// The identity is reloaded periodically so externally renewed credential
// generations (24-hour leaves) take effect without a restart.
const identityMaxAge = 10 * time.Minute

type controlPlane struct {
	cfg ControlPlaneConfig

	mu       sync.Mutex
	conn     *grpc.ClientConn
	loadedAt time.Time
	addrIdx  int
}

// NewControlPlaneWaker returns a Waker backed by the control plane's
// ActivatorService. Connections are (re)built lazily, rotate across the
// configured replica addresses on failure, and reload identity material
// when it ages out.
func NewControlPlaneWaker(cfg ControlPlaneConfig) (Waker, error) {
	var addrs []string
	for _, addr := range cfg.Addresses {
		if trimmed := strings.TrimSpace(addr); trimmed != "" {
			addrs = append(addrs, trimmed)
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("activator: at least one control-plane address is required")
	}
	cfg.Addresses = addrs
	return &controlPlane{cfg: cfg}, nil
}

func (c *controlPlane) client() (platformv1.ActivatorServiceClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil && time.Since(c.loadedAt) < identityMaxAge {
		return platformv1.NewActivatorServiceClient(c.conn), nil
	}
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	tlsConfig, err := xds.ClientTLS(c.cfg.IdentityDir, c.cfg.ServerName)
	if err != nil {
		return nil, fmt.Errorf("activator: load identity: %w", err)
	}
	addr := c.cfg.Addresses[c.addrIdx%len(c.cfg.Addresses)]
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		return nil, fmt.Errorf("activator: dial control plane: %w", err)
	}
	c.conn = conn
	c.loadedAt = time.Now()
	return platformv1.NewActivatorServiceClient(conn), nil
}

func (c *controlPlane) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.addrIdx++
}

func (c *controlPlane) Wake(ctx context.Context, hostname string) ([]Endpoint, bool, error) {
	client, err := c.client()
	if err != nil {
		return nil, false, err
	}
	response, err := client.WakeService(ctx, &platformv1.WakeServiceRequest{Hostname: hostname})
	if err != nil {
		switch status.Code(err) {
		case codes.NotFound:
			return nil, false, ErrNotFound
		case codes.Unavailable, codes.Unauthenticated:
			// The replica may be down or our leaf may have rotated; force
			// the next request onto a fresh connection and address.
			c.invalidate()
		}
		return nil, false, err
	}
	endpoints := make([]Endpoint, 0, len(response.GetEndpoints()))
	for _, endpoint := range response.GetEndpoints() {
		endpoints = append(endpoints, Endpoint{Address: endpoint.GetAddress(), Port: endpoint.GetPort()})
	}
	return endpoints, response.GetReady(), nil
}
