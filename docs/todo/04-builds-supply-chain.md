# Stage 4 — Builds and Software Supply Chain

This stage retains the existing source-snapshot and BuildKit direction while making untrusted builds safe and commercially operable.

## 4.1 Make Railpack the default source builder

Prompt:

```text
Make Railpack a first-class field on BuildRecipe and the default for every new inspect, create, and onboarding path, while Dockerfile remains an explicit selectable recipe with dockerfile_path and context_dir. Do it as a clean cutover: builder is required, omitted or unspecified values are rejected, and all newly created repository services choose Railpack unless the user selects Dockerfile. Thread the builder choice through protobufs, JSON recipe encoding, source inspection, staged-change descriptions, console create/update/settings, claimed BuildJob, deployment details, and config-as-code once available. Dispatch in internal/builder so both paths use the same immutable snapshot workspace, exact registry capability, log reporting, cancellation, resource limits, and final digest verification. Railpack analysis failure must produce actionable detected-language and missing-start-command details rather than silently falling back to Dockerfile. Add representative Node, Python, Go, static-site, monorepo, Dockerfile, and no-buildable-source tests.
```

## 4.2 Isolate every untrusted build

Prompt:

```text
Run each customer build inside a disposable isolation boundary stronger than a shared host process. Define a BuildExecutor interface and provide a local executor for development plus a production executor using an operator-selected microVM or hardened sandbox technology. Each execution receives a read-only verified source snapshot, an isolated writable workspace and BuildKit endpoint, scoped push credentials, explicit CPU/memory/disk/PID/time limits, and a restricted network policy. It must not access host sockets, builder credentials, sibling caches, control-plane credentials, or another project’s files. On completion or cancellation, destroy the execution environment and verify cleanup; persistent cache data must be content-addressed and tenant-safe. Do not build a VM orchestration platform in this repository—integrate a narrow executor backend. Add adversarial tests for filesystem escape, host socket access, fork bomb, disk exhaustion, network denial, credential scope, timeout, and cleanup after worker death.
```

## 4.3 Add durable build leases, cancellation, fairness, and quotas

Prompt:

```text
Evolve the CockroachDB build queue into a durable, fair lease-based scheduler without introducing Temporal or another general workflow system. Builders claim work transactionally with a lease epoch and expiry, heartbeat that lease, and may complete only with the current fencing token. User cancellation and supersession must prevent late completion from publishing an image or starting a rollout. Retry transient worker loss with a bounded attempt count while treating deterministic source/build failures as terminal. Enforce per-workspace and global concurrent-build quotas, weighted fairness between tenants, maximum queue age, build timeout, and admission rejection when account limits are exhausted. Expose queue position approximately, attempt history, cancellation progress, and operator drain controls. Test worker death, split ownership, late completion, cancellation races, starvation resistance, and quota release.
```

## 4.4 Make build cache behavior explicit and tenant-safe

Prompt:

```text
Add a documented build-cache model for Railpack and Dockerfile builds. Cache keys must include the source inputs, builder kind and version, architecture, relevant build configuration, and hashes—not plaintext—of build-time variables that legitimately affect the result. Permit sharing across environments of the same project where safe, prohibit cross-project private cache reads, and make public base-layer reuse explicit. Bound cache storage per workspace, track hits/misses and bytes, provide eviction and manual clear operations, and ensure revoked source or deleted projects eventually lose private cache material. Build secrets must use BuildKit secret mounts and must not affect final image layers, metadata, provenance, or logs except through an irreversible cache-input hash where required. Test cache poisoning, secret rotation, branch builds, environment reuse, eviction, and access isolation.
```

## 4.5 Record image provenance and enforce deploy-by-digest

Prompt:

```text
Create an immutable artifact record for every successful build containing source snapshot digest, commit SHA, build recipe and builder version, dependency/build plan where available, image manifest digest, target architecture, build actor, timestamps, and isolation executor identity. Generate an SPDX or CycloneDX SBOM, attach provenance using a standard attestations format, and sign the resulting image digest with a configured signing provider. Deployments must resolve and persist a digest before scheduling; mutable tags are accepted only as user input and never as the runtime identity. Add optional policy gates for unsigned images, failed vulnerability scans, forbidden severity, and stale scans, with clear project-level overrides for authorized roles. Preserve exact artifacts needed for rollback according to retention policy and test tag mutation, signature verification failure, multi-arch selection, and policy enforcement.
```

## 4.6 Add image and artifact lifecycle management

Prompt:

```text
Track references from active deployments, rollback retention, source builds, and legal deletion grace periods to registry manifests, layers, SBOMs, provenance, and source archives. Implement a mark-and-sweep lifecycle that never removes an artifact still reachable from an active or retained deployment, tolerates registry/object-store partial failure, and records tombstoned and physically deleted states. Define per-plan retention for failed builds, successful historic builds, logs, source archives, and images. Show users when an old deployment is no longer redeployable and why before its artifacts expire. Provide operator dry-run and reconciliation commands plus metrics for reclaimable bytes, failed deletes, and orphan discovery. Test shared layers, concurrent rollback, project restoration during grace, external objects missing early, and repeated collection.
```

## 4.7 Establish a production registry contract

Prompt:

```text
Define the production requirements and integration contract for the external OCI registry while preserving embedded control-plane token minting and exact-repository authorization. The configured registry must use durable replicated object storage or another operator-proven durable backend, TLS, authenticated internal administration, manifest deletion compatible with artifact lifecycle, retention-safe garbage collection, health/readiness reporting, and backup or reconstruction procedures for metadata and trust configuration. Verify that short-lived builder push and agent pull capabilities cannot list or access sibling repositories, that revoked or deleted project scopes stop minting new tokens, and that active agents can cold-pull retained digests after restart. Add reconciliation for manifests expected by CockroachDB but missing from the registry and vice versa, VictoriaMetrics operational metrics, capacity alerts, and VM tests for registry restart, backend outage, partial push, garbage collection, signing-key rotation, and cold-node recovery. Do not implement an OCI registry server in this repository.
```
