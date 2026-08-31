# Stage 8 — Developer Workflows and Product Surface

These tasks make the reliable substrate pleasant and programmable without turning every convenience into a new infrastructure subsystem.

## 8.1 Publish a stable customer API

Prompt:

```text
Create a versioned public HTTP API backed by the same control-plane application services and authorization rules as the console; do not create a second source of business logic. Cover projects, environments, services, variables and secrets, source bindings, deployments and actions, domains/endpoints, volumes/backups, logs, metrics query links, usage, memberships, API tokens, monitors, and webhooks as each capability becomes available. Define consistent resource IDs, pagination, filtering, optimistic concurrency, idempotency keys, long-running operation resources, machine-readable errors, request IDs, rate-limit headers, and deprecation policy. Authenticate with the scoped credentials from Stage 6, emit audit events, redact secrets, and publish an OpenAPI document generated or verified in CI. The console may migrate incrementally but equivalent calls must produce identical authorization and state transitions. Add contract and cross-tenant tests.
```

## 8.2 Build a focused platform CLI

Prompt:

```text
Add a typed CLI for login/token configuration, context selection, project and environment inspection, service creation, source or image deployment, variables and secrets, logs, deployment actions, scaling, domains, volumes/backups, usage, and status. Design commands for both interactive humans and CI: stable exit codes, stdout for requested data, stderr for progress, JSON output, --yes for confirmed destructive operations, no color when non-interactive, cancellable waits, and explicit workspace/project/environment flags that override saved context. Never put tokens or secret values in command history, process arguments where avoidable, debug output, or shell completion. Upload source as a deterministic bounded archive only if a future non-GitHub source path is intentionally supported. Generate shell completions and focused command-contract tests against the public API.
```

## 8.3 Add repository configuration as code

Prompt:

```text
Define a small versioned platform.toml or platform.json schema for build and runtime settings that appropriately belong with source: builder kind, Dockerfile/context or Railpack settings, build command, watch paths, start command, pre-deploy command, declared ports, health checks, restart policy, graceful drain, replicas within allowed bounds, resource requests/limits, mount path references, and public endpoint intent. Never allow plaintext secret values, memberships, billing, or operator policy in this file. Parse configuration from the immutable source snapshot, record its digest and field provenance on the deployment, validate it before building, and make repository values override or merge with dashboard settings according to one documented rule. Show effective values and provenance in the console and API. Protect production from unreviewed dangerous changes through existing staged-change and RBAC rules. Publish a JSON schema and test versioning, invalid config, monorepo paths, and rollback.
```

## 8.4 Add branch and pull-request environments

Prompt:

```text
Extend EnvironmentKind with ephemeral pull-request environments driven by verified GitHub App webhook state. A project may configure a persistent base environment, allowed repositories/target branches, resource/replica caps, secret-copy policy, and automatic expiry. On an eligible PR open or update, create or reconcile one isolated environment, copy only permitted configuration, deploy affected services from the exact head SHA, post or update one GitHub status/comment with URLs and state, and remove the environment after merge/close plus a grace period. Handle webhook replay, force-push, forked PR trust, bot PR policy, revoked installation access, and out-of-order events without deploying untrusted code with production secrets. Budget and quota ephemeral use separately, and test the complete lifecycle with fixture webhooks.
```

## 8.5 Add monorepo watch paths and build deduplication

Prompt:

```text
Let repository-backed services declare normalized include and exclude watch paths relative to their build context. On each verified source revision, compare changed paths from the GitHub API or immutable snapshots and queue a build only when the service is affected; a manual deploy must be able to override skipping. Persist a visible skipped deployment reason and the evaluated rule version. Coalesce repeated webhook work for the same service and commit, supersede queued older commits when safe, and reuse one source snapshot across affected services without sharing mutable build state. Path matching must reject traversal, have documented glob semantics, handle merge commits and unavailable diffs conservatively, and avoid skipping when correctness is uncertain. Add tests for root services, nested contexts, renames, deletes, large/unknown diffs, force-pushes, and concurrent deliveries.
```

## 8.6 Add pre-deploy jobs and dependency-aware environment deploys

Prompt:

```text
Add an optional pre-deploy command that runs from the newly built image as a bounded one-shot allocation with the service’s target configuration and explicitly selected secrets, but without receiving public ingress. A successful job permits rollout; failure or timeout leaves the active generation untouched and records logs and exit cause. Ensure retries are explicit because database migrations may not be idempotent. For multi-service environment deploys, derive a dependency DAG only from declared service references, deploy dependencies before consumers, run independent branches concurrently, and reject or clearly break cycles according to a documented rule. Persist batch progress so a control-plane restart resumes safely. Do not create a general job/workflow engine. Test migrations, cancellation, timeout, dependency failure, cycles, and unchanged services.
```

## 8.7 Add reference variables and generated service values

Prompt:

```text
Introduce typed variable references so a service can consume another service’s private hostname/port, a public endpoint, a volume mount path, or selected platform metadata without copying a stale literal. Resolve references per environment at deployment assembly time, preserve the reference expression in configuration history, reject cross-environment and unauthorized references, and avoid revealing referenced secret plaintext in API or audit output. Provide stable generated values for project, environment, service, deployment, allocation/replica, region/failure domain, and internal addresses where useful. A referenced service rename or endpoint change must appear as a staged dependent change and participate in dependency-aware deploy ordering. Detect cycles and missing targets with actionable errors. Test environment duplication, rollback, deletion, rename, and partial access.
```

## 8.8 Add one-off commands and scheduled jobs conservatively

Prompt:

```text
Support authorized one-off commands against an immutable service image and configuration, plus cron-scheduled jobs for workloads that actually need them. Both use bounded one-shot allocations with explicit CPU/memory/time limits, log capture, exit status, cancellation, concurrency policy, and no public ingress. Cron schedules use a declared timezone, validated syntax, missed-run policy, overlap policy, and durable next-run calculation in CockroachDB; a lease-based reconciler may enqueue due runs without adding a workflow engine. Secrets and volumes require explicit opt-in, and volume-backed jobs must respect single-writer fencing. Meter runs through VictoriaMetrics and quota them separately. Provide history and manual trigger controls, and test daylight-saving transitions, control-plane failover, duplicate scheduling, long runs, cancellation, and quota exhaustion.
```

## 8.9 Improve onboarding, accessibility, and truthful UX

Prompt:

```text
Refine the console around the platform’s actual supported path: create or choose a project, connect an authorized GitHub repository or direct image, inspect detected build configuration, create a service, review staged changes, deploy, watch state, and reach a healthy endpoint. Add project/workspace navigation, useful empty states, capacity and quota explanations, and error recovery that preserves user input. Every icon button and field needs an accessible name, dialogs need focus management and keyboard behavior, status cannot depend only on color, timestamps and loading states must be truthful, and destructive actions must state scope and recovery. Provide responsive layouts without hiding required controls. Do not add decorative templates before the core path works. Add focused component tests and a Playwright journey covering keyboard-only onboarding, failed build recovery, deploy, rollback, and deletion grace.
```
