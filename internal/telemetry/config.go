// Package telemetry owns the platform's OpenTelemetry setup: traces exported
// over OTLP, metrics scraped by Prometheus from the same instruments, and
// one wide (canonical) structured log event per completed operation.
//
// Telemetry is configured from the environment using the standard OTEL_*
// variables plus a small TELEMETRY_* set, so the same binaries work with the
// bundled Collector/Tempo stack or any external OTLP backend. Telemetry must
// never fail the operation it observes: setup errors disable export, and
// every recording helper is a no-op when telemetry is off.
package telemetry

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the telemetry configuration for one process.
type Config struct {
	// ServiceName is the OTel service.name, e.g. "controlplane".
	ServiceName string
	// ServiceVersion defaults to PLATFORM_RELEASE or "dev".
	ServiceVersion string
	// Environment is deployment.environment.name: "production" or "development".
	Environment string
	// InstanceID is service.instance.id, unique per replica.
	InstanceID string

	// Disabled turns off trace and metric export. Metrics stay scrapeable.
	Disabled bool
	// OTLPEndpoint is the OTLP/gRPC collector address (host:port). Empty
	// keeps spans in-process only: context, exemplars and log correlation
	// still work, nothing is exported.
	OTLPEndpoint string
	// OTLPInsecure disables TLS to the collector. The bundled collector
	// listens plaintext on the private host network; external backends
	// should use TLS.
	OTLPInsecure bool
	// SampleRatio is the head-sampling ratio for root spans. The bundled
	// collector tail-samples (errors and slow traces always survive), so
	// the default keeps everything and lets the collector decide.
	SampleRatio float64
	// ExportTimeout bounds a single OTLP batch export.
	ExportTimeout time.Duration

	// MetricsListen, when non-empty, serves Prometheus metrics (and pprof
	// is deliberately absent) on its own listener instead of relying on a
	// component health listener. Bind it to the private host address.
	MetricsListen string
	// MetricsToken, when non-empty, requires `Authorization: Bearer <token>`
	// on the metrics endpoint. Deployments set one shared scrape token.
	MetricsToken string
	// LogFormat is "json" (default), "text", or "" to leave slog alone.
	LogFormat string
	// LogLevel is the slog level name; default "info".
	LogLevel string
}

// ConfigFromEnv reads the standard OTEL_* variables and the platform's
// TELEMETRY_* variables. Component-specific prefixes (CONTROLPLANE_,
// AGENT_, BUILDER_) are intentionally not used: OTel configuration is
// already standardized and deployment sets the same names for every role.
func ConfigFromEnv(serviceName string) Config {
	cfg := Config{
		ServiceName:    strings.TrimSpace(envOr("OTEL_SERVICE_NAME", serviceName)),
		ServiceVersion: strings.TrimSpace(firstEnv("PLATFORM_RELEASE", "OTEL_SERVICE_VERSION")),
		Environment:    strings.TrimSpace(firstEnv("DEPLOYMENT_ENVIRONMENT", "OTEL_DEPLOYMENT_ENVIRONMENT")),
		InstanceID:     strings.TrimSpace(firstEnv("PLATFORM_INSTANCE", "HOSTNAME")),
		Disabled:       envBool("TELEMETRY_DISABLED", false),
		OTLPEndpoint:   strings.TrimSpace(firstEnv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT")),
		OTLPInsecure:   envBool("OTEL_EXPORTER_OTLP_INSECURE", false),
		SampleRatio:    envFloat("TELEMETRY_TRACES_SAMPLE_RATIO", 1),
		ExportTimeout:  10 * time.Second,
		MetricsListen:  strings.TrimSpace(os.Getenv("TELEMETRY_METRICS_LISTEN")),
		MetricsToken:   strings.TrimSpace(os.Getenv("TELEMETRY_METRICS_TOKEN")),
		LogFormat:      strings.ToLower(strings.TrimSpace(envOr("TELEMETRY_LOG_FORMAT", "json"))),
		LogLevel:       strings.ToLower(strings.TrimSpace(envOr("TELEMETRY_LOG_LEVEL", "info"))),
	}
	if cfg.OTLPEndpoint == "" && envBool("TELEMETRY_OTLP_LOCAL_COLLECTOR", false) {
		cfg.OTLPEndpoint = "127.0.0.1:4317"
		cfg.OTLPInsecure = true
	}
	// OTEL endpoint values often arrive as http://host:4317 URLs.
	cfg.OTLPEndpoint = strings.TrimPrefix(strings.TrimPrefix(cfg.OTLPEndpoint, "https://"), "http://")
	cfg.OTLPEndpoint = strings.TrimSuffix(cfg.OTLPEndpoint, "/")
	if cfg.ServiceVersion == "" {
		cfg.ServiceVersion = "dev"
	}
	if cfg.Environment == "" {
		cfg.Environment = "production"
	}
	if cfg.SampleRatio < 0 {
		cfg.SampleRatio = 0
	}
	if cfg.SampleRatio > 1 {
		cfg.SampleRatio = 1
	}
	return cfg
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func envBool(key string, fallback bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			return parsed
		}
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if parsed, err := strconv.ParseFloat(v, 64); err == nil {
			return parsed
		}
	}
	return fallback
}
