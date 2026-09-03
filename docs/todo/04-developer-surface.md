# 4 — Convenience on a working substrate

These make a reliable loop pleasant and programmable. They must not jump the queue ahead of a typical image reaching a truthful healthy endpoint, isolated builds, or dual-stack.

4.2 is the exception worth pulling forward if design partners cannot wire services together without copying hostnames.

## 4.1 Repository configuration as code

Was: 8.3
Status: open
Depends on: 1.6, 1.3

Prompt:

```text
Define a small versioned platform.toml or platform.json schema for build and runtime settings that appropriately belong with source: builder kind, Dockerfile/context or Railpack settings, build command, watch paths, start command, pre-deploy command, declared ports, health checks, restart policy, graceful drain, replicas within allowed bounds, resource requests/limits, mount path references, and public endpoint intent. Never allow plaintext secret values, memberships, billing, or operator policy in this file. Parse configuration from the immutable source snapshot, record its digest and field provenance on the deployment, validate it before building, and make repository values override or merge with dashboard settings according to one documented rule. Show effective values and provenance in the console and API. Protect production from unreviewed dangerous changes through existing staged-change and RBAC rules. Publish a JSON schema and test versioning, invalid config, monorepo paths, and rollback.
```

## 4.2 Reference variables

Was: 8.7
Status: open
Depends on: 1.2 so referenced secrets stay expressions, not plaintext copies.

Prompt:

```text
Introduce typed variable references so a service can consume another service’s private hostname/port, a public endpoint, a volume mount path, or selected platform metadata without copying a stale literal. Resolve references per environment at deployment assembly time, preserve the reference expression in configuration history, reject cross-environment and unauthorized references, and avoid revealing referenced secret plaintext in API or audit output. Provide stable generated values for project, environment, service, deployment, allocation/replica, region/failure domain, and internal addresses where useful. A referenced service rename or endpoint change must appear as a staged dependent change and participate in dependency-aware deploy ordering. Detect cycles and missing targets with actionable errors. Test environment duplication, rollback, deletion, rename, and partial access.
```

## 4.3 Monorepo watch paths

Was: 8.5
Status: open
Depends on: 1.6

Prompt:

```text
Let repository-backed services declare normalized include and exclude watch paths relative to their build context. On each verified source revision, compare changed paths from the GitHub API or immutable snapshots and queue a build only when the service is affected; a manual deploy must be able to override skipping. Persist a visible skipped deployment reason and the evaluated rule version. Coalesce repeated webhook work for the same service and commit, supersede queued older commits when safe, and reuse one source snapshot across affected services without sharing mutable build state. Path matching must reject traversal, have documented glob semantics, handle merge commits and unavailable diffs conservatively, and avoid skipping when correctness is uncertain. Add tests for root services, nested contexts, renames, deletes, large/unknown diffs, force-pushes, and concurrent deliveries.
```

## 4.4 Pre-deploy jobs and environment deploy DAG

Was: 8.6
Status: open
Depends on: 2.1, 4.2 if the DAG is derived from service references.

Prompt:

```text
Add an optional pre-deploy command that runs from the newly built image as a bounded one-shot allocation with the service’s target configuration and explicitly selected secrets, but without receiving public ingress. A successful job permits rollout; failure or timeout leaves the active generation untouched and records logs and exit cause. Ensure retries are explicit because database migrations may not be idempotent. For multi-service environment deploys, derive a dependency DAG only from declared service references, deploy dependencies before consumers, run independent branches concurrently, and reject or clearly break cycles according to a documented rule. Persist batch progress so a control-plane restart resumes safely. Do not create a general job/workflow engine. Test migrations, cancellation, timeout, dependency failure, cycles, and unchanged services.
```

## 4.5 Pull-request environments

Was: 8.4
Status: open
Depends on: 1.8, 3.3, 2.4. Do not copy production secrets onto forked PRs.

Prompt:

```text
Extend EnvironmentKind with ephemeral pull-request environments driven by verified GitHub App webhook state. A project may configure a persistent base environment, allowed repositories/target branches, resource/replica caps, secret-copy policy, and automatic expiry. On an eligible PR open or update, create or reconcile one isolated environment, copy only permitted configuration, deploy affected services from the exact head SHA, post or update one GitHub status/comment with URLs and state, and remove the environment after merge/close plus a grace period. Handle webhook replay, force-push, forked PR trust, bot PR policy, revoked installation access, and out-of-order events without deploying untrusted code with production secrets. Budget and quota ephemeral use separately, and test the complete lifecycle with fixture webhooks.
```
