# Production Platform Backlog

This directory turns the current platform gaps into implementation prompts. The prompts describe the product and operational outcome in detail while leaving room for the implementing agent to choose a simple design that fits the repository.

This is not a feature-parity checklist for another platform. Preserve the architecture that already makes sense here:

- CockroachDB is the authoritative control-plane store.
- Agents remain deliberately dumb and reconcile control-plane desired state.
- containerd remains the workload runtime.
- WireGuard and eBPF remain the private transport and identity-policy layer.
- Caddy remains the initial ingress data plane unless a prompt explicitly changes the control-plane contract around it.
- ClickHouse remains the log store and may also support suitable operational analytics.
- VictoriaMetrics is the time-series store for billable resource metrics and the corresponding operational resource views. ClickHouse must not become the billing meter, and CockroachDB should hold immutable billing rollups and ledger state rather than raw high-frequency samples.
- GitHub App installation grants and immutable source snapshots remain the source trust model.
- Production integrations such as object storage, KMS, billing, durable block storage, email, and upstream edge protection should use narrow provider interfaces. Do not implement a new distributed database, workflow engine, storage engine, certificate authority product, or global edge network inside this repository.

Each prompt should be implemented end to end. That normally means protobuf and persisted model changes, authorization, control-plane behavior, agent/builder behavior where relevant, console UX, configuration and validation, focused tests, local-stack support, and operator documentation. Prefer a small coherent model over compatibility scaffolding. Do not preserve old persisted shapes or wire fields.

## Release gates

The stages are ordered by dependency and commercial risk, not by visual prominence.

### Gate A: controlled stateless private beta

Complete Stages 0 through 3, plus the build and networking items required by the selected beta workload. At this gate the platform may host stateless services for known design partners without accepting important persistent customer data or promising general availability.

### Gate B: paid stateless production

Complete Stages 4 through 6 and the applicable workflow and verification work in Stages 8 and 9. At this gate the platform should support paid, multi-tenant stateless workloads with explicit limits, recovery procedures, an operator on-call path, and a stable automation surface.

### Gate C: stateful production

Complete Stage 7 and its recovery exercises in Stage 9 before advertising databases or durable customer workloads. A local bind mount is not a production volume.

## Stages

1. [Stage 0 — Immediate safety and truthful state](00-immediate-safety.md)
2. [Stage 1 — Workload lifecycle and deployments](01-workload-lifecycle.md)
3. [Stage 2 — Control-plane durability and secrets](02-control-plane-durability.md)
4. [Stage 3 — Observability and operations](03-observability-operations.md)
5. [Stage 4 — Builds and software supply chain](04-builds-supply-chain.md)
6. [Stage 5 — Networking and ingress](05-networking-ingress.md)
7. [Stage 6 — Multi-tenancy and commercial controls](06-multitenancy-commercial.md)
8. [Stage 7 — Stateful workloads](07-stateful-workloads.md)
9. [Stage 8 — Developer workflows and product surface](08-developer-workflows.md)
10. [Stage 9 — Production verification and release discipline](09-production-verification.md)

Prompts within a stage can often be implemented in parallel, but do not bypass their stated dependencies. When a prompt discovers that an earlier invariant is missing, make that invariant explicit and fix it at the owning layer instead of adding a console-only workaround.
