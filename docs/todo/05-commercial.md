# 5 — Charge and govern

These items make the platform safe to sell. They are not required for a design-partner beta and must not delay the running-service or untrusted-code slices.

Metering needs operational metrics (3.5) first. Spend limits need quotas (3.3) and a drain path that already exists for stateless services.

## 5.1 Usage meter

Was: 6.5
Status: open
Depends on: 3.5, 2.1

Prompt:

```text
Meter billable usage per workspace: allocation CPU and memory, volume bytes, and network egress from VictoriaMetrics, reconciled against allocation and volume lifecycle intervals in CockroachDB so replaced replicas, restarts, and duplicate samples do not double-bill. Write hourly usage buckets to CockroachDB and close them after a lateness window; a closed bucket is never mutated. Test golden synthetic usage, a meter outage with catch-up, and duplicate samples. Do not add superseding adjustments, calculation-version recomputation, or build/log metering.
```

## 5.2 Plans and spend limit

Was: 6.6
Status: open
Depends on: 5.1, 3.3

Prompt:

```text
Integrate Stripe (behind a narrow provider interface) for a small set of plans: a subscription with included usage, and metered overage from the 5.1 buckets reported as invoice line items. Verify and deduplicate Stripe webhooks. Add a per-workspace hard spend limit: when crossed, stop new deploys and scale stateless services to zero without deleting any data, and restore them when the limit is raised or paid. Show current-period usage and cost in the console. Test webhook replay, failed payment, limit crossing, and reactivation. Do not add promotional credits, versioned price books, or manual ledger adjustments.
```
