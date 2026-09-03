# 5 — Charge and govern

These items make the platform safe to sell. They are not required for a design-partner beta and must not delay the running-service or untrusted-code slices.

Metering needs operational metrics (3.5) first. Spend limits need quotas (3.3) and a drain path that already exists for stateless services.

## 5.1 VictoriaMetrics billing meter

Was: 6.5
Status: open
Depends on: 3.5, 2.1

Prompt:

```text
Implement a billing meter whose raw resource time series live in VictoriaMetrics and whose closed-period usage records live immutably in CockroachDB. Define exact billable units and sampling rules for allocation CPU, reserved or measured memory according to the chosen pricing contract, persistent volume bytes, build resources, network egress, and retained storage. Use stable allocation/build/volume identities and lifecycle intervals so retries, replica replacement, counter resets, duplicate remote-write samples, delayed samples, and control-plane restarts cannot double bill or erase use. A periodic meter must query bounded VictoriaMetrics ranges, reconcile them with authoritative lifecycle state, write versioned per-hour or per-day usage buckets with source watermark and calculation version, and close buckets after a lateness window. Recalculation creates explicit superseding adjustments rather than mutating invoiced usage. Add synthetic golden tests, outage catch-up, clock-skew, and duplicate-allocation scenarios.
```

## 5.2 Plans, credits, and spend controls

Was: 6.6
Status: open
Depends on: 5.1, 3.3

Prompt:

```text
Model versioned plans and prices separately from measured usage so a price change never rewrites historic cost. Support subscription state, included credit, promotional credit with expiry, metered line items, tax/customer metadata delegated to the billing provider, and explicit manual adjustments with actor and reason. Integrate a narrow BillingProvider for checkout, payment status, invoices, and provider webhooks; verify webhook signatures and process them idempotently through an outbox/inbox model. Add soft alerts and hard spend limits. A hard limit must follow a documented state machine that prevents new billable work, drains or suspends eligible stateless services safely, never deletes data, continues minimum platform access, and can recover after payment or the next billing period. Console usage must reconcile meter buckets to invoice line items. Test pricing-version boundaries, late metrics, webhook replay, failed payment, credit expiry, hard-limit crossing, and reactivation.
```

## 5.3 Fair-use and abuse safeguards

Was: 6.7
Status: open
Depends on: 3.3, 3.8, 3.18

Prompt:

```text
Protect shared infrastructure from abusive or accidentally pathological tenants. Add per-actor API token buckets, expensive-query budgets, build and deployment churn limits, log and metrics cardinality limits, webhook destination controls, outbound connection and bandwidth policies, registry request limits, and configurable account-risk holds. Limits must be hierarchical, observable, machine-readable, and degrade the offending scope without destabilizing other tenants. Security-sensitive limits must not be user-increasable; commercial limits may have audited operator overrides. Avoid brittle content classification inside the platform: integrate external fraud or abuse review only through a narrow status provider and retain human override. Provide a customer-visible reason and appeal/support reference without revealing detection internals. Load-test noisy-neighbor isolation and verify that one workspace cannot exhaust control-plane workers, ClickHouse, VictoriaMetrics, builders, registry, or ingress.
```

## 5.4 Account export, retention, and closure

Was: 6.8
Status: open
Depends on: 1.6, 3.4, 5.1

Prompt:

```text
Define customer data categories and retention for identity, configuration, source snapshots, images, secrets, logs, metrics, usage ledger, invoices, audit events, backups, and support artifacts. Add an authorized export job that produces a manifest and portable copies of customer-owned configuration and data that can be exported safely, with secret inclusion requiring a separate high-assurance flow. Account closure must disable new work, settle or preserve required billing records, revoke credentials, tombstone resources, honor recovery grace, and then delete external artifacts through idempotent jobs while retaining only legally required records. Expose progress and failures to operators and the customer. Make retention configurable by plan where appropriate but never shorter than safety or billing invariants. Test cancellation during grace, export while resources change, partial provider deletion, and proof that closed tenants disappear from product queries.
```

## 5.5 Operator and support access

Was: 6.9
Status: open
Depends on: 3.1, 3.4, 3.10

Prompt:

```text
Separate ordinary operator health access from exceptional customer-resource support access. Operators should diagnose aggregate platform state without automatically reading customer logs, configuration, source metadata, or secrets. Any support access to a customer scope requires an eligible operator role, ticket/reason, bounded duration, least-privilege capability, prominent customer-visible audit event, and automatic expiry; secret plaintext remains unavailable unless a separate break-glass policy explicitly permits it. Break-glass actions require strong re-authentication, a second approver where configured, immediate security notification, and immutable audit export. Do not implement silent impersonation. Add support-session inventory and revocation, ensure all downstream API and query calls retain the true operator actor plus delegated customer scope, and test expiry, revocation, attempted scope expansion, and audit redaction.
```


