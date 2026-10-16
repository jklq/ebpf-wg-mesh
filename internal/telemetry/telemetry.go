package telemetry

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Provider owns one process's telemetry pipeline. Shutdown flushes pending
// spans; it is safe to call once at process exit.
type Provider struct {
	cfg      Config
	tracer   *sdktrace.TracerProvider
	meter    *sdkmetric.MeterProvider
	registry *prometheus.Registry
	server   *http.Server
	ln       net.Listener

	shutdownOnce sync.Once
}

// Option customizes Setup for tests.
type Option func(*setupOptions)

type setupOptions struct {
	spanExporter sdktrace.SpanExporter
	metricReader sdkmetric.Reader
	skipGlobals  bool
}

// WithSpanExporter replaces the OTLP exporter (tests).
func WithSpanExporter(exp sdktrace.SpanExporter) Option {
	return func(o *setupOptions) { o.spanExporter = exp }
}

// WithMetricReader replaces the Prometheus reader (tests).
func WithMetricReader(r sdkmetric.Reader) Option {
	return func(o *setupOptions) { o.metricReader = r }
}

var (
	globalMu       sync.Mutex
	globalProvider *Provider
	globalHandler  http.Handler
)

// Setup installs the global tracer/meter providers, W3C propagation, the
// Go runtime instruments, and (when configured) the metrics listener and
// structured logging. It is idempotent per process: later calls return the
// existing provider, which keeps in-process component tests safe. A setup
// failure leaves telemetry disabled and reports the error; callers should
// log it and continue serving.
func Setup(ctx context.Context, cfg Config, opts ...Option) (*Provider, error) {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalProvider != nil {
		return globalProvider, nil
	}
	var so setupOptions
	for _, opt := range opts {
		opt(&so)
	}
	if cfg.Disabled && so.spanExporter == nil && so.metricReader == nil {
		installLogging(cfg)
		p := &Provider{cfg: cfg, registry: prometheus.NewRegistry()}
		globalProvider = p
		return p, nil
	}

	res, err := buildResource(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("telemetry resource: %w", err)
	}

	// One Prometheus registry holds OTel metrics plus the standard Go and
	// process collectors, so a single scrape gets RED, domain, runtime and
	// process series with consistent target labels.
	registry := prometheus.NewPedanticRegistry()
	if err := registry.Register(collectors.NewGoCollector()); err != nil {
		return nil, fmt.Errorf("register go collector: %w", err)
	}
	if err := registry.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
		return nil, fmt.Errorf("register process collector: %w", err)
	}
	if err := registry.Register(collectors.NewBuildInfoCollector()); err != nil {
		return nil, fmt.Errorf("register build info collector: %w", err)
	}

	promReader, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		return nil, fmt.Errorf("prometheus exporter: %w", err)
	}
	reader := sdkmetric.Reader(promReader)
	if so.metricReader != nil {
		reader = so.metricReader
	}
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(histogramViews()...),
	)

	tracerOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	}
	exporter := so.spanExporter
	if exporter == nil && cfg.OTLPEndpoint != "" {
		exporter, err = newTraceExporter(ctx, cfg)
		if err != nil {
			// Export is best-effort: keep local spans (exemplars, log
			// correlation) and report the failure to the caller.
			slog.Warn("telemetry: OTLP trace exporter unavailable; traces stay local", "error", err, "endpoint", cfg.OTLPEndpoint)
			exporter = nil
		}
	}
	if exporter != nil {
		tracerOpts = append(tracerOpts, sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(5*time.Second),
			sdktrace.WithExportTimeout(cfg.ExportTimeout),
			sdktrace.WithMaxExportBatchSize(512),
			sdktrace.WithMaxQueueSize(4096),
		))
	}
	tracerProvider := sdktrace.NewTracerProvider(tracerOpts...)

	p := &Provider{cfg: cfg, tracer: tracerProvider, meter: meterProvider, registry: registry}
	if !so.skipGlobals {
		otel.SetTracerProvider(tracerProvider)
		otel.SetMeterProvider(meterProvider)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{},
		))
		otel.SetErrorHandler(errorHandler{})
		globalProvider = p
		globalHandler = p.MetricsHandler()
	}
	if err := runtime.Start(runtime.WithMeterProvider(meterProvider), runtime.WithMinimumReadMemStatsInterval(15*time.Second)); err != nil {
		slog.Warn("telemetry: runtime metrics unavailable", "error", err)
	}
	installLogging(cfg)
	if cfg.MetricsListen != "" {
		if err := p.serveMetrics(cfg.MetricsListen); err != nil {
			return p, fmt.Errorf("metrics listener %s: %w", cfg.MetricsListen, err)
		}
	}
	InitDomain(meterProvider)
	return p, nil
}

type errorHandler struct{}

func (errorHandler) Handle(err error) {
	slog.Warn("telemetry: export error", "error", err)
}

func buildResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	host, _ := os.Hostname()
	attrs := []attribute.KeyValue{
		attribute.String("service.name", cfg.ServiceName),
		attribute.String("service.version", cfg.ServiceVersion),
		attribute.String("deployment.environment.name", cfg.Environment),
	}
	if cfg.InstanceID != "" {
		attrs = append(attrs, attribute.String("service.instance.id", cfg.InstanceID))
	}
	if v := strings.TrimSpace(os.Getenv("PLATFORM_HOST")); v != "" {
		attrs = append(attrs, attribute.String("host.id", v))
	}
	if v := strings.TrimSpace(os.Getenv("PLATFORM_INSTALLATION")); v != "" {
		attrs = append(attrs, attribute.String("platform.installation", v))
	}
	if host != "" {
		attrs = append(attrs, attribute.String("host.name", host))
	}
	return resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithHost(),
		resource.WithOS(),
		resource.WithProcessRuntimeVersion(),
		resource.WithAttributes(attrs...),
	)
}

func newTraceExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint),
		otlptracegrpc.WithTimeout(cfg.ExportTimeout),
		otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{
			Enabled:         true,
			InitialInterval: time.Second,
			MaxInterval:     10 * time.Second,
			MaxElapsedTime:  time.Minute,
		}),
	}
	if cfg.OTLPInsecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	return otlptracegrpc.New(ctx, opts...)
}

// histogramViews pins explicit-bucket histograms for server latencies so
// Prometheus aggregation is stable across SDK upgrades. Buckets cover
// sub-millisecond control-plane calls through multi-minute builds.
func histogramViews() []sdkmetric.View {
	latencySeconds := []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300, 1800}
	hist := func(name string) sdkmetric.View {
		return sdkmetric.NewView(
			sdkmetric.Instrument{Name: name},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: latencySeconds, NoMinMax: false,
			}},
		)
	}
	return []sdkmetric.View{
		hist("rpc.server.call.duration"),
		hist("rpc.client.call.duration"),
		hist("rpc.server.duration"),
		hist("rpc.client.duration"),
		hist("http.server.request.duration"),
		hist("http.client.request.duration"),
		hist("db.client.operation.duration"),
		hist("platform.build.duration"),
		hist("platform.operation.duration"),
		hist("platform.reconcile.duration"),
	}
}

// MetricsHandler serves the Prometheus registry in OpenMetrics text format
// (required for exemplars). It enforces the scrape token when configured.
func (p *Provider) MetricsHandler() http.Handler {
	if p == nil || p.registry == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "telemetry disabled", http.StatusServiceUnavailable)
		})
	}
	h := promhttp.HandlerFor(p.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
		Timeout:           10 * time.Second,
	})
	if p.cfg.MetricsToken == "" {
		return h
	}
	want := "Bearer " + p.cfg.MetricsToken
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Handler returns a handler that resolves the process-global metrics
// handler at request time, for components that mount /metrics on an
// existing listener before or after Setup. It 404s before Setup.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		globalMu.Lock()
		h := globalHandler
		globalMu.Unlock()
		if h == nil {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (p *Provider) serveMetrics(listen string) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", p.MetricsHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	p.ln = ln
	p.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute}
	go func() { _ = p.server.Serve(ln) }()
	slog.Info("telemetry: metrics listener", "service", p.cfg.ServiceName, "listen", ln.Addr().String(), "otlp_endpoint", p.cfg.OTLPEndpoint)
	return nil
}

// MetricsAddr reports the bound metrics address, for tests and startup logs.
func (p *Provider) MetricsAddr() string {
	if p == nil || p.ln == nil {
		return ""
	}
	return p.ln.Addr().String()
}

// ForceFlush exports pending spans now; used by tests and short-lived tools.
func (p *Provider) ForceFlush(ctx context.Context) error {
	if p == nil || p.tracer == nil {
		return nil
	}
	return p.tracer.ForceFlush(ctx)
}

// Shutdown flushes spans and stops the metrics listener. It never panics
// and is safe on a nil or disabled provider.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var err error
	p.shutdownOnce.Do(func() {
		if p.server != nil {
			_ = p.server.Shutdown(ctx)
		}
		if p.tracer != nil {
			err = p.tracer.Shutdown(ctx)
		}
		if p.meter != nil {
			if merr := p.meter.Shutdown(ctx); err == nil {
				err = merr
			}
		}
		globalMu.Lock()
		if globalProvider == p {
			globalProvider = nil
			globalHandler = nil
		}
		globalMu.Unlock()
	})
	return err
}
