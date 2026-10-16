package telemetry

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
)

type healthServer struct {
	healthv1.UnimplementedHealthServer
}

func (healthServer) Check(context.Context, *healthv1.HealthCheckRequest) (*healthv1.HealthCheckResponse, error) {
	return &healthv1.HealthCheckResponse{Status: healthv1.HealthCheckResponse_SERVING}, nil
}

// TestGRPCMetricNames guards the dashboard/alert PromQL: the real otelgrpc
// handler must emit rpc.server.call.duration in seconds, which the
// Prometheus exporter renders as rpc_server_call_duration_seconds.
func TestGRPCMetricNames(t *testing.T) {
	p := setupForTest(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(GRPCServerStatsHandler(), grpc.ChainUnaryInterceptor(GRPCUnaryInterceptor()))
	healthv1.RegisterHealthServer(srv, healthServer{})
	go func() { _ = srv.Serve(ln) }()
	defer srv.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(ln.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		GRPCClientStatsHandler())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := healthv1.NewHealthClient(conn).Check(ctx, &healthv1.HealthCheckRequest{Service: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	_, body := scrape(t, p, "test-token")
	for _, want := range []string{"rpc_server_call_duration_seconds", "rpc_client_call_duration_seconds", "grpc.health.v1.Health"} {
		found := false
		for _, line := range strings.Split(body, "\n") {
			if strings.Contains(line, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("scrape missing %q", want)
		}
	}
	// The server span for the health check must exist once.
	spans := 0
	for _, s := range testExporter.all() {
		if strings.Contains(s.Name(), "Health/Check") {
			spans++
		}
	}
	if spans == 0 {
		t.Fatal("no grpc health span recorded")
	}
}
