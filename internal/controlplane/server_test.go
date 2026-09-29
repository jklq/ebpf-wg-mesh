package controlplane

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/health"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	grpchealth "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func TestControlPlaneReadinessOmitsPrivateTopology(t *testing.T) {
	t.Parallel()

	server := &Server{}
	rec := httptest.NewRecorder()
	health.ServeReadiness(server.readyReport)(rec, httptest.NewRequest(http.MethodGet, health.ReadinessPath, nil).WithContext(context.Background()))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
	var report health.Report
	if err := json.NewDecoder(rec.Body).Decode(&report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.Status != health.StatusNotReady {
		t.Fatalf("unexpected report %#v", report)
	}
	body := rec.Body.String()
	if strings.Contains(body, "postgresql://") || strings.Contains(body, "127.0.0.1") || strings.Contains(body, "password") {
		t.Fatalf("readiness leaked private topology: %s", body)
	}
	joined := strings.Join(report.Failed, ",")
	for _, name := range []string{"database", "migrations", "source_storage"} {
		if !strings.Contains(joined, name) {
			t.Fatalf("expected failed check %q in %q", name, joined)
		}
	}
}

// The control plane multiplexes gRPC over one http.Server through
// dualProtocolHandler. gRPC's serverHandlerTransport, created by ServeHTTP,
// panics in Drain, so shutting the shared server down must not call
// GracefulStop. A live streaming RPC keeps that transport registered.
func TestServerCloseAfterServeHTTPDoesNotPanic(t *testing.T) {
	grpcServer := grpc.NewServer()
	healthpb.RegisterHealthServer(grpcServer, grpchealth.NewServer())

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

func newDelivery(store *persistence, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, events *platformEvents, logEmitter *logs.LogEmitter) *deliverycore.Delivery {
	return newDeliveryWithScheduler(store, nil, notifier, ingress, events, logEmitter)
}

func TestBuildLeaseUsesItsOwnConfig(t *testing.T) {
	cfg := config.ControlPlaneBuilderConfig{HeartbeatTimeoutSeconds: 5, LeaseTTLSeconds: 120}
	if got := buildSchedulerConfigFromControlPlane(cfg).LeaseTTL; got != 120*time.Second {
		t.Fatalf("lease TTL = %s, want 120s", got)
	}
}
