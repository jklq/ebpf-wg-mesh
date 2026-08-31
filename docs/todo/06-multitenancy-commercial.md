# Stage 6 — Multi-Tenancy and Commercial Controls

This stage makes the platform safe to sell. VictoriaMetrics supplies billable resource measurements; CockroachDB holds the immutable commercial ledger.

## 6.1 Add organizations, invitations, and complete RBAC

Prompt:

```text
Introduce workspaces as the billing and organizational owner of projects while preserving personal ownership as a one-member workspace. Model invitations, membership lifecycle, and explicit workspace and project roles with capabilities rather than scattered role-name checks. At minimum distinguish administration, billing, project management, deployment, configuration/secrets management, log/metrics viewing, and read-only access; production environments must be restrictable separately. Centralize authorization in the control plane and apply it consistently to gRPC methods, streaming/event endpoints, console loaders/actions, future public API calls, webhooks, and support tooling. Membership changes must take effect promptly for live sessions, never expose secret values to viewers, and prevent removing the last owner. Add invitation expiry/revocation, project transfer, ownership transfer, and tests that enumerate every API capability across roles and environment restrictions.
```

## 6.2 Create tamper-evident audit history

Prompt:

```text
Record an audit event for every authenticated mutation and every security-relevant system action: membership, roles, secrets, service config, deploy actions, domains, volumes, API tokens, billing limits, key rotation, support access, backups, restores, policy overrides, and deletion. Store actor type and stable ID, effective role, workspace/project/environment/resource scope, request correlation ID, action, safe before/after summaries, source IP metadata where trusted, result, reason, and UTC timestamp. Never store plaintext secrets, tokens, source archives, or unrestricted request bodies. Make event creation atomic with the mutation through a transaction or durable outbox, append-only to ordinary callers, queryable with authorization and pagination, exportable, and subject to a documented retention policy. Add a hash-chain or external archival hook so operators can detect deletion or rewriting. Test redaction, failed attempts, system actors, retries, pagination, and tenant isolation.
```

## 6.3 Build scoped API credentials and session security controls

Prompt:

```text
Add user, workspace, project, and environment-scoped API credentials with named capabilities, creation metadata, optional expiry, last-used time, and explicit revocation. Store only a strong token hash plus a short lookup prefix, display the plaintext once, use constant-time verification, rate-limit failures, and make revocation effective across all control-plane replicas. Tokens must never gain more capability than their creator and must be rejected for browser-only or support-only actions. Add session inventory and revocation, secure cookie defaults, CSRF protection for browser mutations, configurable idle and absolute expiry, and optional mandatory MFA claims for sensitive production actions. Every use should carry an actor identity into audit events. Provide console management and tests for scope boundaries, expiry, rotation, revocation races, and token leakage through logs/errors.
```

## 6.4 Enforce resource quotas and admission control

Prompt:

```text
Create a hierarchical quota model with operator defaults and workspace/project overrides for projects, environments, services, replicas, requested CPU and memory, volume bytes, domains, concurrent builds, build minutes, source storage, image retention, log ingestion, metrics cardinality, network egress, API rate, and webhook delivery. Enforce quotas transactionally at resource creation or scaling, reserve capacity during in-progress changes, and release it reliably on cancellation or tombstoned deletion according to documented semantics. Distinguish product quota from temporary physical-capacity shortage and return machine-readable errors with current use, limit, and remediation. Do not silently overcommit resources whose isolation depends on hard limits. Provide usage views and operator override/audit flows. Test concurrent admission, failed rollout release, project transfer, quota reduction below current use, and enforcement consistency across console and API.
```

## 6.5 Meter billable resources through VictoriaMetrics

Prompt:

```text
Implement a billing meter whose raw resource time series live in VictoriaMetrics and whose closed-period usage records live immutably in CockroachDB. Define exact billable units and sampling rules for allocation CPU, reserved or measured memory according to the chosen pricing contract, persistent volume bytes, build resources, network egress, and retained storage. Use stable allocation/build/volume identities and lifecycle intervals so retries, replica replacement, counter resets, duplicate remote-write samples, delayed samples, and control-plane restarts cannot double bill or erase use. A periodic meter must query bounded VictoriaMetrics ranges, reconcile them with authoritative lifecycle state, write versioned per-hour or per-day usage buckets with source watermark and calculation version, and close buckets after a lateness window. Recalculation creates explicit superseding adjustments rather than mutating invoiced usage. Add synthetic golden tests, outage catch-up, clock-skew, and duplicate-allocation scenarios.
```

## 6.6 Add plans, pricing, credits, and spend controls

Prompt:

```text
Model versioned plans and prices separately from measured usage so a price change never rewrites historic cost. Support subscription state, included credit, promotional credit with expiry, metered line items, tax/customer metadata delegated to the billing provider, and explicit manual adjustments with actor and reason. Integrate a narrow BillingProvider for checkout, payment status, invoices, and provider webhooks; verify webhook signatures and process them idempotently through an outbox/inbox model. Add soft alerts and hard spend limits. A hard limit must follow a documented state machine that prevents new billable work, drains or suspends eligible stateless services safely, never deletes data, continues minimum platform access, and can recover after payment or the next billing period. Console usage must reconcile meter buckets to invoice line items. Test pricing-version boundaries, late metrics, webhook replay, failed payment, credit expiry, hard-limit crossing, and reactivation.
```

## 6.7 Add fair-use and abuse safeguards

Prompt:

```text
Protect shared infrastructure from abusive or accidentally pathological tenants. Add per-actor API token buckets, expensive-query budgets, build and deployment churn limits, log and metrics cardinality limits, webhook destination controls, outbound connection and bandwidth policies, registry request limits, and configurable account-risk holds. Limits must be hierarchical, observable, machine-readable, and degrade the offending scope without destabilizing other tenants. Security-sensitive limits must not be user-increasable; commercial limits may have audited operator overrides. Avoid brittle content classification inside the platform: integrate external fraud or abuse review only through a narrow status provider and retain human override. Provide a customer-visible reason and appeal/support reference without revealing detection internals. Load-test noisy-neighbor isolation and verify that one workspace cannot exhaust control-plane workers, ClickHouse, VictoriaMetrics, builders, registry, or ingress.
```

## 6.8 Implement account export, retention, and closure

Prompt:

```text
Define customer data categories and retention for identity, configuration, source snapshots, images, secrets, logs, metrics, usage ledger, invoices, audit events, backups, and support artifacts. Add an authorized export job that produces a manifest and portable copies of customer-owned configuration and data that can be exported safely, with secret inclusion requiring a separate high-assurance flow. Account closure must disable new work, settle or preserve required billing records, revoke credentials, tombstone resources, honor recovery grace, and then delete external artifacts through idempotent jobs while retaining only legally required records. Expose progress and failures to operators and the customer. Make retention configurable by plan where appropriate but never shorter than safety or billing invariants. Test cancellation during grace, export while resources change, partial provider deletion, and proof that closed tenants disappear from product queries.
```

## 6.9 Add controlled operator and support access

Prompt:

```text
Separate ordinary operator health access from exceptional customer-resource support access. Operators should diagnose aggregate platform state without automatically reading customer logs, configuration, source metadata, or secrets. Any support access to a customer scope requires an eligible operator role, ticket/reason, bounded duration, least-privilege capability, prominent customer-visible audit event, and automatic expiry; secret plaintext remains unavailable unless a separate break-glass policy explicitly permits it. Break-glass actions require strong re-authentication, a second approver where configured, immediate security notification, and immutable audit export. Do not implement silent impersonation. Add support-session inventory and revocation, ensure all downstream API and query calls retain the true operator actor plus delegated customer scope, and test expiry, revocation, attempted scope expansion, and audit redaction.
```

## 6.10 Produce privacy, security, and compliance evidence from real controls

Prompt:

```text
Document the platform’s data flow, subprocessors/provider classes, encryption boundaries, regional storage behavior, retention, deletion, backup, incident response, access control, and shared-responsibility model directly from implemented configuration and controls. Generate evidence reports for access reviews, key rotation, backup/restore drills, vulnerability remediation, release provenance, availability, support access, audit retention, and incident exercises without claiming a certification the operator has not obtained. Add configurable data-region constraints that admission and provider selection can enforce when the installation supports them; reject unsupported combinations rather than presenting a cosmetic region field. Provide versioned customer-facing security and privacy documentation plus an operator evidence-export command whose output is sanitized and integrity-protected. Treat formal certifications, legal terms, DPA, and insurance as organizational work outside this repository, but make the technical evidence they require reproducible.
```
