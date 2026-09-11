package controlplane

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// The control plane multiplexes gRPC over one http.Server through
// dualProtocolHandler. gRPC's serverHandlerTransport, created by ServeHTTP,
// panics in Drain, so shutting the shared server down must not call
// GracefulStop. A live streaming RPC keeps that transport registered.
func TestServerCloseAfterServeHTTPDoesNotPanic(t *testing.T) {
	grpcServer := grpc.NewServer()
	healthpb.RegisterHealthServer(grpcServer, health.NewServer())

	httpServer := httptest.NewUnstartedServer(dualProtocolHandler(grpcServer, http.NotFoundHandler()))
	httpServer.EnableHTTP2 = true
	httpServer.StartTLS()
	defer httpServer.Close()

	conn, err := grpc.NewClient(httpServer.Listener.Addr().String(),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := healthpb.NewHealthClient(conn).Watch(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("initial watch response: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- (&Server{
			internalGRPC: grpcServer,
			internalHTTP: httpServer.Config,
			internalLn:   httpServer.Listener,
		}).Close()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server Close did not return")
	}
}
