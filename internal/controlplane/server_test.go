package controlplane

import (
	"context"
	"net"
	"testing"

	"ebof-wg-mesh/internal/testutil"

	"google.golang.org/grpc"
)

func TestServeGRPCReturnsNilOnGracefulStop(t *testing.T) {
	t.Parallel()

	server := grpc.NewServer()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- serveGRPC(server, ln)
	}()

	if err := testutil.Poll(context.Background(), testutil.PollConfig{}, func(ctx context.Context) (bool, error) {
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return false, nil
		}
		_ = conn.Close()
		return true, nil
	}); err != nil {
		t.Fatalf("wait for grpc listener: %v", err)
	}
	server.GracefulStop()

	if err := <-errCh; err != nil {
		t.Fatalf("serveGRPC: %v", err)
	}
}
