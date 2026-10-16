package telemetry

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/trace"
)

var loggingOnce sync.Once

// installLogging replaces the default slog logger with a handler that emits
// structured records carrying service identity and, when a span is active,
// trace_id/span_id so a log line, its wide event and its trace in Tempo are
// one lookup apart. Existing call sites keep using package slog unchanged.
func installLogging(cfg Config) {
	if cfg.LogFormat == "" || cfg.LogFormat == "none" {
		return
	}
	loggingOnce.Do(func() {
		opts := &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}
		var base slog.Handler
		if cfg.LogFormat == "text" {
			base = slog.NewTextHandler(os.Stderr, opts)
		} else {
			base = slog.NewJSONHandler(os.Stderr, opts)
		}
		handler := &traceHandler{Handler: base}
		logger := slog.New(handler).With(
			slog.String("service", cfg.ServiceName),
			slog.String("service_version", cfg.ServiceVersion),
			slog.String("environment", cfg.Environment),
		)
		if cfg.InstanceID != "" {
			logger = logger.With(slog.String("instance", cfg.InstanceID))
		}
		slog.SetDefault(logger)
	})
}

func parseLevel(name string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// traceHandler injects trace correlation fields into every record that
// carries a context with a sampled (or any valid) span.
type traceHandler struct {
	slog.Handler
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx != nil {
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			r = r.Clone()
			r.AddAttrs(
				slog.String("trace_id", sc.TraceID().String()),
				slog.String("span_id", sc.SpanID().String()),
			)
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithGroup(name)}
}

// LoggerFrom returns a logger enriched with the operation fields already
// collected on ctx (see Operation), for the rare mid-operation log line
// that must exist before the wide event finalizes.
func LoggerFrom(ctx context.Context) *slog.Logger {
	l := slog.Default()
	if op := operationFrom(ctx); op != nil {
		return l.With(op.slogAttrs()...)
	}
	return l
}
