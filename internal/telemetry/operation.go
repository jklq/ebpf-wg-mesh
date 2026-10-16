package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	grpc_codes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type operationKey struct{}

// Operation is one service-hop operation: facts are collected as work
// happens (middleware, auth, business logic via Set) and finalized exactly
// once at the true end of the operation. Finalization writes span
// attributes, operation metrics and one canonical wide-event log record:
//
//	event=operation kind=rpc name=/platform.../CreateProject outcome=error
//	error.kind=permission_denied caller.class=dashboard user.id=... duration_ms=12
//
// High-cardinality fields (IDs) live here and on the span, never as metric
// labels. The wide event is emitted at Info for successes and Warn/Error
// for failures, so log volume stays one record per operation per hop.
type Operation struct {
	name     string
	kind     string
	start    time.Time
	mu       sync.Mutex
	attrs    []attribute.KeyValue
	done     bool
	span     trace.Span
	ownsSpan bool
	emitLog  bool
}

// StartOperation begins an operation under the current span (it does not
// replace transport spans created by otelgrpc/otelhttp). Call End exactly
// once. kind is a bounded class such as "rpc", "http", "build", "reconcile".
func StartOperation(ctx context.Context, kind, name string) (context.Context, *Operation) {
	op := &Operation{name: name, kind: kind, start: time.Now(), span: trace.SpanFromContext(ctx), emitLog: true}
	return context.WithValue(ctx, operationKey{}, op), op
}

// StartSpanOperation starts a span and an operation together; used at
// boundaries with no transport instrumentation (jobs, reconcile loops).
func StartSpanOperation(ctx context.Context, tracer trace.Tracer, kind, name string, attrs ...attribute.KeyValue) (context.Context, *Operation) {
	ctx, span := tracer.Start(ctx, name, trace.WithAttributes(attrs...))
	op := &Operation{name: name, kind: kind, start: time.Now(), span: span, ownsSpan: true, emitLog: true}
	op.attrs = append(op.attrs, attrs...)
	return context.WithValue(ctx, operationKey{}, op), op
}

func operationFrom(ctx context.Context) *Operation {
	if ctx == nil {
		return nil
	}
	op, _ := ctx.Value(operationKey{}).(*Operation)
	return op
}

// Set enriches the operation on ctx. It is the deliberate-instrumentation
// seam: auth adds caller.*, handlers add entity IDs and branch decisions.
func Set(ctx context.Context, attrs ...attribute.KeyValue) {
	if op := operationFrom(ctx); op != nil {
		op.Set(attrs...)
	}
}

// Set adds attributes to this operation (bounded: values are truncated).
func (o *Operation) Set(attrs ...attribute.KeyValue) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return
	}
	for _, a := range attrs {
		// Replace same-key facts so the latest known value wins.
		replaced := false
		for i, existing := range o.attrs {
			if existing.Key == a.Key {
				o.attrs[i] = a
				replaced = true
				break
			}
		}
		if !replaced {
			o.attrs = append(o.attrs, a)
		}
	}
}

// End finalizes the operation. err classifies the outcome; an explicit
// outcome overrides the success/error default ("cancelled", "timeout").
// End is idempotent and never replaces the operation's result.
func (o *Operation) End(ctx context.Context, err error, outcomeOverride ...string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.done {
		o.mu.Unlock()
		return
	}
	o.done = true
	attrs := append([]attribute.KeyValue(nil), o.attrs...)
	o.mu.Unlock()

	dur := time.Since(o.start)
	outcome, errorKind := classify(err)
	if len(outcomeOverride) > 0 && outcomeOverride[0] != "" {
		outcome = outcomeOverride[0]
	}
	if ctx != nil && ctx.Err() != nil && outcome == "error" {
		if errors.Is(ctx.Err(), context.Canceled) {
			outcome, errorKind = "cancelled", "cancelled"
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			outcome, errorKind = "timeout", "deadline_exceeded"
		}
	}

	span := o.span
	if span != nil && span.IsRecording() {
		final := append(attrs,
			attribute.String("operation.kind", o.kind),
			attribute.String("operation.name", o.name),
			attribute.String("outcome", outcome),
			attribute.Float64("duration_ms", float64(dur.Microseconds())/1000),
		)
		if errorKind != "" {
			final = append(final, attribute.String("error.kind", errorKind))
		}
		span.SetAttributes(final...)
		if err != nil {
			span.RecordError(err)
			if outcome != "cancelled" {
				span.SetStatus(codes.Error, errorKind)
			}
		} else {
			span.SetStatus(codes.Ok, "")
		}
	}
	if span != nil && o.ownsSpan {
		// Only end spans this operation created: transport spans are owned
		// by their handlers; StartOperation reuses the parent span.
		span.End()
	}

	metricCtx := ctx
	if metricCtx == nil {
		metricCtx = context.Background()
	}
	recordOperation(metricCtx, o.kind, outcomeCategory(outcome, err), dur)

	if o.emitLog {
		level := slog.LevelInfo
		if err != nil && outcome != "cancelled" {
			level = slog.LevelWarn
			if errorKind == "internal" || errorKind == "unknown" || errorKind == "unavailable" {
				level = slog.LevelError
			}
		}
		logAttrs := []any{
			slog.String("event", "operation"),
			slog.String("kind", o.kind),
			slog.String("name", o.name),
			slog.String("outcome", outcome),
			slog.Float64("duration_ms", float64(dur.Microseconds())/1000),
		}
		if errorKind != "" {
			logAttrs = append(logAttrs, slog.String("error_kind", errorKind))
		}
		if err != nil {
			logAttrs = append(logAttrs, slog.String("error", truncate(err.Error(), 512)))
		}
		for _, a := range attrs {
			logAttrs = append(logAttrs, slog.Any(logKey(string(a.Key)), a.Value.AsInterface()))
		}
		logCtx := ctx
		if logCtx == nil {
			logCtx = context.Background()
		}
		slog.Log(logCtx, level, "operation", logAttrs...)
	}
}

func (o *Operation) slogAttrs() []any {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]any, 0, len(o.attrs)*1)
	for _, a := range o.attrs {
		out = append(out, slog.Any(logKey(string(a.Key)), a.Value.AsInterface()))
	}
	return out
}

func logKey(k string) string { return strings.ReplaceAll(k, ".", "_") }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// classify maps an error to (outcome, stable error kind). Kinds are gRPC
// code names in snake_case: they are bounded and safe for spans, logs and
// alert groupings, unlike raw error strings.
func classify(err error) (outcome, kind string) {
	if err == nil {
		return "success", ""
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled", "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", "deadline_exceeded"
	}
	code := status.Code(err)
	if code != grpc_codes.Unknown {
		return "error", codeName(code)
	}
	return "error", "unknown"
}

func outcomeCategory(outcome string, err error) string {
	switch outcome {
	case "cancelled", "timeout", "client_error":
		return outcome
	}
	if err == nil {
		return "success"
	}
	return "error"
}

func codeName(c grpc_codes.Code) string {
	s := c.String()
	// "PermissionDenied" -> "permission_denied"
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Tracer returns a named tracer from the global provider.
func Tracer(name string) trace.Tracer { return otel.Tracer(name) }

// Attr helpers keep call sites terse and consistently named.

func String(key, value string) attribute.KeyValue      { return attribute.String(key, truncate(value, 256)) }
func Int64(key string, value int64) attribute.KeyValue { return attribute.Int64(key, value) }
func Bool(key string, value bool) attribute.KeyValue   { return attribute.Bool(key, value) }
