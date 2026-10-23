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

	billingPeriodsClosed metric.Int64Counter
	billingAmountDue     metric.Int64Counter
	billingInvoices      metric.Int64Counter
	billingWebhooks      metric.Int64Counter
	billingRestrictions  metric.Int64Counter
	billingAccounts      metric.Int64Counter

	serverlessSleeps  metric.Int64Counter
	serverlessWakes   metric.Int64Counter
	serverlessWakeDur metric.Float64Histogram
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
	d.billingPeriodsClosed, _ = m.Int64Counter("platform.billing.periods_closed",
		metric.WithDescription("Billing periods frozen into statements, by mode/state"),
		metric.WithUnit("{period}"))
	d.billingAmountDue, _ = m.Int64Counter("platform.billing.amount_due",
		metric.WithDescription("Statement amount due at period close, in minor currency units"),
		metric.WithUnit("{cent}"))
	d.billingInvoices, _ = m.Int64Counter("platform.billing.invoices",
		metric.WithDescription("Invoice work items processed, by outcome"),
		metric.WithUnit("{invoice}"))
	d.billingWebhooks, _ = m.Int64Counter("platform.billing.webhooks",
		metric.WithDescription("Stripe webhook events, by outcome"),
		metric.WithUnit("{event}"))
	d.billingRestrictions, _ = m.Int64Counter("platform.billing.restrictions_blocked",
		metric.WithDescription("Growth attempts stopped by an enforced billing restriction, by scope"),
		metric.WithUnit("{attempt}"))
	d.billingAccounts, _ = m.Int64Counter("platform.billing.accounts_created",
		metric.WithDescription("Billing accounts provisioned"),
		metric.WithUnit("{account}"))
	d.serverlessSleeps, _ = m.Int64Counter("platform.serverless.sleeps",
		metric.WithDescription("Serverless services scaled to zero after inactivity"),
		metric.WithUnit("{sleep}"))
	d.serverlessWakes, _ = m.Int64Counter("platform.serverless.wakes",
		metric.WithDescription("Serverless services woken, by trigger"),
		metric.WithUnit("{wake}"))
	d.serverlessWakeDur, _ = m.Float64Histogram("platform.serverless.wake.duration",
		metric.WithDescription("Cold-start time from public wake to first ready endpoint"), metric.WithUnit("s"))
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

// RecordBillingPeriodClosed records one period frozen into a statement.
// Amounts aggregate across workspaces; per-workspace figures belong in the
// billing statements themselves, never in metric labels.
func RecordBillingPeriodClosed(ctx context.Context, mode, state, currency string, amountDueCents int64) {
	d := inst()
	if d == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("mode", bounded(mode, "unknown")),
		attribute.String("state", bounded(state, "unknown")),
		attribute.String("currency", bounded(currency, "unknown")),
	)
	if d.billingPeriodsClosed != nil {
		d.billingPeriodsClosed.Add(ctx, 1, attrs)
	}
	if d.billingAmountDue != nil && amountDueCents > 0 {
		d.billingAmountDue.Add(ctx, amountDueCents, attrs)
	}
}

// RecordBillingInvoiceWork records one settled/failed invoice work item
// ("settled", "paused", "mismatch", "failed").
func RecordBillingInvoiceWork(ctx context.Context, outcome string) {
	d := inst()
	if d == nil || d.billingInvoices == nil {
		return
	}
	d.billingInvoices.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", bounded(outcome, "unknown"))))
}

// RecordBillingWebhook records one Stripe webhook outcome
// ("applied", "failed", "ingest_rejected").
func RecordBillingWebhook(ctx context.Context, outcome string) {
	d := inst()
	if d == nil || d.billingWebhooks == nil {
		return
	}
	d.billingWebhooks.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", bounded(outcome, "unknown"))))
}

// RecordBillingRestrictionBlocked counts an enforcement encounter
// ("placement", "build", "growth"). Repeated reconciliation passes while a
// restriction holds each count once; rate signals an active restriction.
func RecordBillingRestrictionBlocked(ctx context.Context, scope string) {
	d := inst()
	if d == nil || d.billingRestrictions == nil {
		return
	}
	d.billingRestrictions.Add(ctx, 1, metric.WithAttributes(attribute.String("scope", bounded(scope, "unknown"))))
}

// RecordBillingAccountsCreated records provisioned billing accounts.
func RecordBillingAccountsCreated(ctx context.Context, n int) {
	d := inst()
	if d == nil || d.billingAccounts == nil || n <= 0 {
		return
	}
	d.billingAccounts.Add(ctx, int64(n))
}

// RecordServerlessSleep counts one service scaled to zero for inactivity.
func RecordServerlessSleep(ctx context.Context) {
	d := inst()
	if d == nil || d.serverlessSleeps == nil {
		return
	}
	d.serverlessSleeps.Add(ctx, 1)
}

// RecordServerlessWake counts one wake transition. Trigger is a bounded
// value: "public", "private", "operator", or "system".
func RecordServerlessWake(ctx context.Context, trigger string) {
	d := inst()
	if d == nil || d.serverlessWakes == nil {
		return
	}
	d.serverlessWakes.Add(ctx, 1, metric.WithAttributes(attribute.String("trigger", bounded(trigger, "unknown"))))
}

// RecordServerlessWakeDuration records the cold-start latency of one public
// wake, from the activator's request to the first ready endpoint.
func RecordServerlessWakeDuration(ctx context.Context, dur time.Duration) {
	d := inst()
	if d == nil || d.serverlessWakeDur == nil || dur < 0 {
		return
	}
	d.serverlessWakeDur.Record(ctx, dur.Seconds())
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
