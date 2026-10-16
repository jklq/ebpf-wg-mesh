package telemetry

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type recordingExporter struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (e *recordingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, spans...)
	return nil
}
func (e *recordingExporter) Shutdown(context.Context) error { return nil }
func (e *recordingExporter) all() []sdktrace.ReadOnlySpan {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sdktrace.ReadOnlySpan(nil), e.spans...)
}
func (e *recordingExporter) byName(name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range e.all() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

var (
	testProviderOnce sync.Once
	testProvider     *Provider
	testExporter     = &recordingExporter{}
	testSetupErr     error
)

func setupForTest(t *testing.T) *Provider {
	t.Helper()
	testProviderOnce.Do(func() {
		cfg := Config{
			ServiceName: "test-service", ServiceVersion: "test", Environment: "development",
			InstanceID: "test-0", SampleRatio: 1, ExportTimeout: time.Second,
			MetricsToken: "test-token", LogFormat: "none",
		}
		testProvider, testSetupErr = Setup(context.Background(), cfg, WithSpanExporter(testExporter))
	})
	if testSetupErr != nil {
		t.Fatalf("setup: %v", testSetupErr)
	}
	return testProvider
}

func scrape(t *testing.T, p *Provider, token string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Accept", "application/openmetrics-text")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	p.MetricsHandler().ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body)
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4317/")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	t.Setenv("TELEMETRY_TRACES_SAMPLE_RATIO", "0.25")
	t.Setenv("TELEMETRY_METRICS_LISTEN", "127.0.0.1:9095")
	cfg := ConfigFromEnv("controlplane")
	if cfg.ServiceName != "controlplane" || cfg.OTLPEndpoint != "collector:4317" || !cfg.OTLPInsecure || cfg.SampleRatio != 0.25 || cfg.MetricsListen != "127.0.0.1:9095" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestWideOperationSpanAndMetrics(t *testing.T) {
	p := setupForTest(t)
	ctx := context.Background()

	sctx, op := StartSpanOperation(ctx, Tracer("test"), "rpc", "test/CreateThing")
	Set(sctx, String("caller.class", "dashboard"), String("user.id", "user-1"), String("thing.id", "thing-9"))
	op.End(sctx, status.Error(codes.PermissionDenied, "nope"))
	op.End(sctx, nil) // idempotent: must not double-emit
	RecordBuild(ctx, "succeeded", "hardened", 2*time.Second)
	RecordBillingPeriodClosed(ctx, "shadow", "shadow", "usd", 2500)
	RecordBillingInvoiceWork(ctx, "settled")
	RecordBillingWebhook(ctx, "applied")
	RecordBillingRestrictionBlocked(ctx, "placement")
	if err := p.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}

	spans := testExporter.byName("test/CreateThing")
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	attrs := map[string]attribute.Value{}
	for _, a := range spans[0].Attributes() {
		attrs[string(a.Key)] = a.Value
	}
	for key, want := range map[string]string{
		"outcome": "error", "error.kind": "permission_denied",
		"caller.class": "dashboard", "user.id": "user-1", "thing.id": "thing-9",
	} {
		if got := attrs[key].AsString(); got != want {
			t.Fatalf("span attr %s = %q, want %q (all: %v)", key, got, want, attrs)
		}
	}
	if spans[0].Status().Code.String() != "Error" {
		t.Fatalf("span status = %v, want Error", spans[0].Status())
	}

	code, body := scrape(t, p, "")
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated scrape = %d, want 401", code)
	}
	code, body = scrape(t, p, "test-token")
	if code != http.StatusOK {
		t.Fatalf("scrape = %d", code)
	}
	for _, want := range []string{"platform_operations_total", "platform_builds_total", "platform_build_duration_seconds_bucket", "target_info",
		"platform_billing_periods_closed_total", "platform_billing_amount_due_total", "platform_billing_invoices_total",
		"platform_billing_webhooks_total", "platform_billing_restrictions_blocked_total"} {
		if !strings.Contains(body, want) {
			t.Fatalf("scrape missing %s", want)
		}
	}
	// IDs must never become metric labels.
	if strings.Contains(body, "thing-9") || strings.Contains(body, "user-1") {
		t.Fatal("high-cardinality ID leaked into metrics")
	}
}

func TestTracePropagationAcrossHops(t *testing.T) {
	p := setupForTest(t)
	ctx, parent := Tracer("test").Start(context.Background(), "parent-hop")
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	parent.End()

	extracted := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
	_, child := Tracer("test").Start(extracted, "child-hop")
	childTrace := child.SpanContext().TraceID()
	child.End()
	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if childTrace != parent.SpanContext().TraceID() {
		t.Fatal("trace ID did not survive propagation")
	}
	children := testExporter.byName("child-hop")
	if len(children) != 1 || children[0].Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Fatalf("child span parent mismatch: %+v", children)
	}
}

func TestHTTPHandlerInstrumentation(t *testing.T) {
	p := setupForTest(t)
	h := HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Set(r.Context(), String("route.kind", "test"))
		w.WriteHeader(http.StatusTeapot)
	}), "test-http")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/widgets", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d", rec.Code)
	}
	// Health paths are not operations.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, body := scrape(t, p, "test-token")
	if !strings.Contains(body, "http_server_request_duration_seconds") {
		t.Fatal("http server metrics missing")
	}
	count := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "platform_operations_total{") && strings.Contains(line, `kind="http"`) {
			count++
			if !strings.Contains(line, `outcome="client_error"`) {
				t.Fatalf("4xx must classify as client_error: %s", line)
			}
		}
	}
	if count != 1 {
		t.Fatalf("want exactly 1 http operation series (readyz excluded), lines:\n%s", body)
	}
}

func TestClassify(t *testing.T) {
	for err, want := range map[error]string{
		nil:                               "",
		status.Error(codes.NotFound, "x"): "not_found",
		status.Error(codes.PermissionDenied, "x"): "permission_denied",
		context.Canceled: "cancelled",
		fmt.Errorf("wrap: %w", context.DeadlineExceeded): "deadline_exceeded",
	} {
		_, kind := classify(err)
		if kind != want {
			t.Fatalf("classify(%v) kind = %q, want %q", err, kind, want)
		}
	}
	var _ = trace.SpanContext{}
}
