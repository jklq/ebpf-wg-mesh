package controlplane

import (
	"context"
	"net"
	"net/http"
	"testing"

	"ebof-wg-mesh/internal/testutil"

	"google.golang.org/grpc"
)

func TestServeHTTPReturnsNilOnClose(t *testing.T) {
	server := &http.Server{Handler: NewOpsHandler()}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- serveHTTP(server, ln)
	}()

	if err := testutil.Poll(context.Background(), testutil.PollConfig{}, func(ctx context.Context) (bool, error) {
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return false, nil
		}
		_ = conn.Close()
		return true, nil
	}); err != nil {
		t.Fatalf("wait for http listener: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("serveHTTP: %v", err)
	}
}

func TestServeGRPCReturnsNilOnGracefulStop(t *testing.T) {
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
