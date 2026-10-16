package telemetry

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Domain instruments. Labels are strictly bounded-cardinality: outcomes and
// kinds only, never entity IDs (IDs belong on spans and wide events). See
// docs/observability.md for the catalogue and the cardinality contract.
type domain struct {
	buildsTotal     metric.Int64Counter
	buildDuration   metric.Float64Histogram
	agentSessions   metric.Int64Counter
	operationsTotal metric.Int64Counter
	operationDur    metric.Float64Histogram
	logLinesTotal   metric.Int64Counter
	logDropsTotal   metric.Int64Counter
	reconcileTotal  metric.Int64Counter
	reconcileDur    metric.Float64Histogram
}

var (
	domainMu   sync.RWMutex
	domainInst *domain
)

// InitDomain creates the domain instruments on the given meter provider.
// Safe to call repeatedly; later calls replace earlier instruments.
func InitDomain(mp metric.MeterProvider) {
	if mp == nil {
		return
	}
	m := mp.Meter("ebof-wg-mesh/platform")
	d := &domain{}
	d.buildsTotal, _ = m.Int64Counter("platform.builds",
		metric.WithDescription("Build attempts completed, by outcome"),
		metric.WithUnit("{build}"))
	d.buildDuration, _ = m.Float64Histogram("platform.build.duration",
		metric.WithDescription("Build attempt duration"), metric.WithUnit("s"))
	d.agentSessions, _ = m.Int64Counter("platform.agent.sessions",
		metric.WithDescription("Agent control-plane sessions ended, by outcome"),
		metric.WithUnit("{session}"))
	d.operationsTotal, _ = m.Int64Counter("platform.operations",
		metric.WithDescription("Completed operations (wide events), by kind and outcome"),
		metric.WithUnit("{operation}"))
	d.operationDur, _ = m.Float64Histogram("platform.operation.duration",
		metric.WithDescription("Operation duration by kind"), metric.WithUnit("s"))
	d.logLinesTotal, _ = m.Int64Counter("platform.log.lines",
		metric.WithDescription("Workload log lines accepted by the pipeline"),
		metric.WithUnit("{line}"))
	d.logDropsTotal, _ = m.Int64Counter("platform.log.drops",
		metric.WithDescription("Workload log lines shed (gap rows record the loss)"),
		metric.WithUnit("{line}"))
	d.reconcileTotal, _ = m.Int64Counter("platform.reconcile.runs",
		metric.WithDescription("Reconciliation runs, by loop and outcome"),
		metric.WithUnit("{run}"))
	d.reconcileDur, _ = m.Float64Histogram("platform.reconcile.duration",
		metric.WithDescription("Reconciliation run duration"), metric.WithUnit("s"))
	domainMu.Lock()
	domainInst = d
	domainMu.Unlock()
}

func inst() *domain {
	domainMu.RLock()
	defer domainMu.RUnlock()
	return domainInst
}

// RecordBuild records one completed build attempt. Outcome is a stable
// bounded value: "succeeded", "failed", "cancelled" or a failure kind.
func RecordBuild(ctx context.Context, outcome, executor string, dur time.Duration) {
	d := inst()
	if d == nil || d.buildsTotal == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("outcome", bounded(outcome, "unknown")),
		attribute.String("executor", bounded(executor, "unknown")),
	)
	d.buildsTotal.Add(ctx, 1, attrs)
	if d.buildDuration != nil && dur > 0 {
		d.buildDuration.Record(ctx, dur.Seconds(), attrs)
	}
}

// RecordAgentSession records one ended agent session ("connected",
// "disconnected", "rotated", "redirected", "error").
func RecordAgentSession(ctx context.Context, outcome string) {
	d := inst()
	if d == nil || d.agentSessions == nil {
		return
	}
	d.agentSessions.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", bounded(outcome, "unknown"))))
}

// RecordLogLines records accepted/shed workload log lines for a source
// ("agent", "builder", "ingest").
func RecordLogLines(ctx context.Context, source string, accepted, dropped int64) {
	d := inst()
	if d == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("source", bounded(source, "unknown")))
	if accepted > 0 && d.logLinesTotal != nil {
		d.logLinesTotal.Add(ctx, accepted, attrs)
	}
	if dropped > 0 && d.logDropsTotal != nil {
		d.logDropsTotal.Add(ctx, dropped, attrs)
	}
}

// RecordReconcile records one reconciliation loop run.
func RecordReconcile(ctx context.Context, loop, outcome string, dur time.Duration) {
	d := inst()
	if d == nil || d.reconcileTotal == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("loop", bounded(loop, "unknown")),
		attribute.String("outcome", bounded(outcome, "unknown")),
	)
	d.reconcileTotal.Add(ctx, 1, attrs)
	if d.reconcileDur != nil && dur > 0 {
		d.reconcileDur.Record(ctx, dur.Seconds(), attrs)
	}
}

func recordOperation(ctx context.Context, kind, outcome string, dur time.Duration) {
	d := inst()
	if d == nil || d.operationsTotal == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("kind", bounded(kind, "unknown")),
		attribute.String("outcome", bounded(outcome, "unknown")),
	)
	d.operationsTotal.Add(ctx, 1, attrs)
	if d.operationDur != nil && dur > 0 {
		d.operationDur.Record(ctx, dur.Seconds(), attrs)
	}
}

// bounded keeps metric label values safe: short and non-empty. It is not a
// substitute for passing genuinely bounded values.
func bounded(v, fallback string) string {
	if v == "" {
		return fallback
	}
	if len(v) > 64 {
		return v[:64]
	}
	return v
}
