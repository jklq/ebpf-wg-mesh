# Billing

Billing prices the resource usage meters in [resource-usage.md](resource-usage.md)
per workspace, freezes the result into statements, and collects through
Stripe Invoicing. The design follows [usage-billing-plan.md](usage-billing-plan.md),
with the divergences recorded below. All financial authority lives in the Go
control plane (`internal/controlplane/billing/`); the console only ever calls
its RPCs.

## Modes and the launch gate

`--billing-mode` is `disabled`, `shadow` (default), or `enabled`.

- **Shadow** provisions accounts, rates usage, closes periods into frozen
  statements, and records restrictions as dry runs. It never creates Stripe
  objects, never charges, and delivery never enforces a dry-run restriction.
- **Enabled** adds Stripe collection and restriction enforcement. Startup
  fails in enabled mode without Stripe keys, and fails unless at least one
  full shadow period (28 days or longer) has already closed into a
  statement. That is the plan's gate: a complete shadow period, reconciled
  against independent measurements, before any charge. The check proves a
  shadow statement exists; the reconciliation itself is the operator's
  review of those statements.
- A persisted kill switch (`SetBillingCollectionPaused`, operator-only)
  pauses invoice finalization and collection while measurement, rating,
  statements, and reconciliation keep running. After a database restore,
  set it before resuming collection and reconcile statement identities
  against Stripe first.

## Payer: one Billing Account per Workspace

Each workspace has exactly one Billing Account (the plan proposed per-user
accounts; the workspace domain model in [CONTEXT.md](../CONTEXT.md) already
designates the workspace as the billing container, and membership roles map
cleanly: workspace **admins** change payment details, plan, and budget;
any member can read the account summary and statements). Managed platform
projects have no workspace and are exempt.

The payer of a resource is frozen when the control plane assigns it:
`usage_attributions.workspace_id` snapshots the project's workspace at
assignment or volume pinning. Moving a project to another workspace changes
the payer only for resources assigned afterwards. Financial rows have no
foreign keys into the catalog, so deleting projects or workspaces never
erases a bill.

New accounts start on the trial plan with a €5 credit grant expiring 30 days
after account creation. Plans (monthly minimum = included usage): trial
€0, hobby €5, pro €20. Plan changes take effect next period. Leaving trial
in enabled mode goes through paid activation: save a payment method
(Stripe Checkout, setup mode), the first minimum is invoiced, and the
account activates only when that invoice is paid. Unused trial credit keeps
its original expiry after the upgrade.

## Periods, rating, and statements

Periods are UTC calendar-month anniversaries of account creation, clamping
short months from the original anchor day (Jan 31 → Feb 28 → Mar 31). Each
period pins its plan minimum, included usage, currency (EUR), and rate
version at creation.

Rating aggregates `usage_hourly` through attribution, using the raw covered
integrals — never the usage query layer's coverage-adjusted display
estimates. Billable quantities: `cpu_usec`, `memory_byte_ms`,
`volume_byte_ms` minus retained (post-deletion, operator-funded) storage,
and public egress (direct + ingress). Intervals belong to the hour bucket
containing their start, as in usage queries. Rates are exact rationals
(rate version 1, provisional shadow prices from the plan's Railway
benchmarks, month = 2,592,000 s, GiB units): CPU €20/vCPU-month, memory
€10/GiB-month, volume €0.15/GiB-month, egress €0.05/GiB. Final commercial
prices are still owed after the shadow run (plan section 12).

```text
gross_usage = cpu + memory + volume + egress   (exact, then rounded once, half-up)
usage_after_allowance = max(0, gross - included_usage)
period_total = monthly_minimum + usage_after_allowance - credit_applied
amount_due   = period_total - prepaid_minimum + next_period_minimum + carried_balance
```

A €5 plan with €3 of usage costs €5; with €12 of usage it costs €12.
Credit applies only after the allowance, never to the minimum, earliest
expiry first, each grant and consumption recorded once. Cents are allocated
deterministically across statement lines (largest remainder). Lines are per
project × meter plus the account lines (minimum, allowance, credit,
prepaid, next minimum, carried balance), so every statement explains
itself; deleted projects keep their frozen names.

A period closes 24 hours after it ends. Closing is exactly-once under the
account lock. Usage arriving after finalization stays as retained evidence
and is handled by explicit credit adjustments, never by editing the frozen
statement. Renewal statements bill the previous period's total plus the
next period's minimum (each line dated); the first period's minimum is the
activation statement. Dues under Stripe's €0.50 minimum carry into the next
statement, or are waived when the account is terminal. Cancellation takes
effect at period end with no new minimum; retained volumes continue on
storage-only periods (no minimum, volume meter only) until deleted.

Statements, credits, and producer receipts are kept indefinitely; usage
retention (raw 30 d, hourly 90 d) only needs to outlive period close
because statements freeze their quantities. Keep
`--usage-hourly-retention-days` above a period plus the close window — the
control plane warns at startup when it does not.

## Spend restrictions

Accounts may set a budget stop threshold against **gross** usage (before
allowance and credits). It is not an absolute invoice ceiling: the minimum
and retained storage still bill. Notices are recorded once per
account/period at 75%, 90%, and 100% (`billing_notices`); there is no email
infrastructure yet, so notice delivery to users is future work — operators
get an alarm webhook event when a restriction engages.

An enforced restriction (enabled mode only):

- stops compute: delivery drains every allocation of the workspace through
  the normal scale-down path and refuses new allocations at the single
  creation seam; desired replica counts and drafts are preserved, so
  clearing restores service;
- blocks growth: new builds (claim), environment releases, restarts and
  redeploys, scale increases, and volume creation/growth — from user
  requests and autonomous reconciliation alike;
- still allows inspection, payment, export, scale-down, and deletion.

Payment failure blocks growth immediately and suspends compute after a
7-day grace; restoration rechecks that no failed statement remains. Trial
expiry and cancellation also restrict. Restrictions are journal product
state (`billing_restrictions`), so they survive leader changes with the
rest of desired state. Detection runs with the billing maintenance pass
(default 60 s) on hourly rollups. A disconnected agent keeps running until
it receives new desired state — there is no finite enforcement bound during
a partition, and shadow mode should be used to measure real lag before
launch.

Trial resource limits (compute, replica, volume, and build caps from the
plan) are not implemented yet; trials are bounded by credit, expiry, and
budget only. Do not open trial signup to the public before those caps and
an abuse policy exist.

## Stripe

The adapter (`stripe.go`, official SDK `stripe-go/v82`, API version
2025-08-27.basil pinned by the dependency) is the only provider surface.
Rating stays internal; Stripe collects frozen statements:

- draft invoice with `auto_advance=false`,
  `collection_method=charge_automatically`, statement ID in metadata;
- every statement line attached to that exact draft, per-line idempotency
  keys and persisted item IDs, so retries never duplicate a line;
- draft verified (customer, currency, metadata, amount) before explicit
  finalization; authentication-required payments use the hosted invoice URL
  exposed on the statement;
- a lost create response is disambiguated by metadata search before any
  retry, because Stripe prunes idempotency keys after 24 hours; an
  unresolved mismatch blocks collection and alarms;
- webhooks arrive as raw bytes at `OpsService.IngestStripeWebhook`, are
  signature-verified in Go, persisted by Stripe event ID before success,
  and applied asynchronously; paid is terminal and never regresses;
- a reconciliation pass syncs unsettled statements against Stripe to cover
  missing webhooks.

The console's public `/webhooks/stripe` route is plumbing only: it bounds
the body to the same 256 KiB, forwards the raw bytes and the
`Stripe-Signature` header unchanged, and answers 200 only after
`IngestStripeWebhook` accepts. Checkout returns to
`/workspace/billing/checkout/success` and `/workspace/billing/checkout/cancel`
(the remembered workspace selects the account); a saved card alone never
activates paid service.

## Console

Billing lives under `/workspace/billing` in the workspace navigation. The
console renders what the RPCs return and never prices anything: amounts are
the control plane's cents, project totals add its allocated lines, and rates
are re-expressed per readable unit (30-day month, GiB) with exact integers.
Current-period figures refresh every minute and carry the watermark and gap
time beside them. Plan prices are not exposed by an RPC yet, so plan choices
show names only; the current period's pinned minimum and allowance are
shown. Cancellation asks what happens to volumes, because kept volumes
continue on storage-only periods, and shows the volume rate before
confirming. Blocked actions show the backend's restriction error with a
link to billing.

Tax collection is not configured. Stripe integration alone does not
establish tax obligations; configure Stripe Tax with finance review before
paid launch. Refunds and corrections are explicit credit entries locally;
provider credit notes are not automated yet.

## Alarms and recovery

Operator alarms (existing webhook) fire on restriction engagement and
provider mismatches. Billing tables live in the same PostgreSQL backup as
everything else. After a restore, statements may disagree with Stripe:
pause collection, run reconciliation, and only then resume. Producer
receipts that prevent usage replay live with the usage tables and share
their retention.
