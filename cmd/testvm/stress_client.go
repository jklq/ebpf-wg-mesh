package main

import (
	"context"

	"ebof-wg-mesh/internal/controlplane/delivery"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Follow explicit owner redirects only. A transport error on a mutation is
// ambiguous and must reach the oracle rather than silently replaying a write.
type stressConnection struct {
	grpc.ClientConnInterface
	peers map[string]grpc.ClientConnInterface
}

func (c stressConnection) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	conn := c.ClientConnInterface
	var lastErr error
	for attempt := 0; attempt <= len(c.peers); attempt++ {
		err := conn.Invoke(ctx, method, args, reply, opts...)
		lastErr = err
		if status.Code(err) != codes.FailedPrecondition {
			return err
		}
		addr, ok := delivery.ParseLiveOwnerRedirect(status.Convert(err).Message())
		if !ok {
			return err
		}
		next, ok := c.peers[addr]
		if !ok {
			return status.Errorf(codes.FailedPrecondition, "live owner redirected outside the fixture: %q", addr)
		}
		conn = next
	}
	return lastErr
}
