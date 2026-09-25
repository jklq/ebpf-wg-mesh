# 3 — More than one team on one installation

These items make the beta loop shareable. They are not a reason to delay dual-stack, secrets, Railpack, or build isolation.

3.13 may start as soon as 3.2 exists, even if observability is unfinished. It talks to the existing Connect services; the versioned public HTTP API (3.12) is parked. 3.5 and 3.6 are user-facing features (resource graphs next to deployments) and do not need to wait for the rest of this file.

## 3.1 Workspaces and members

Was: 6.1
Status: open
Depends on: none

Prompt:

```text
Introduce workspaces as the owner of projects; every existing user gets a personal workspace. A workspace has members with one of two roles: Admin (everything, including members and billing) and Member (everything except members, billing, and deleting projects). Invite by email with expiry and revocation; the last admin cannot be removed. Put authorization behind one capability check in the control plane that is used by every RPC, stream, and webhook, and make membership changes take effect on live sessions. Test every RPC against both roles and a non-member. Do not add project-level roles, a Deployer or Viewer role, or project/ownership transfer.
```

## 3.2 API tokens

Was: 6.3
Status: open
Depends on: 3.1

Prompt:

```text
Add API tokens scoped to a workspace or a single project, with a name, optional expiry, last-used time, and revocation. Store only a hash plus a lookup prefix, show the plaintext once, and make revocation effective on every control-plane replica. A token never has more access than its creator. Tokens are rejected for browser-only actions such as membership and billing. Manage tokens in the console. Test scope boundaries, expiry, revocation, and that tokens never appear in logs or errors. Do not add per-capability scopes or environment-scoped tokens.
```

## 3.3 Workspace limits

Was: 6.4
Status: open
Depends on: 3.1. Volume bytes count from 6.1 basic volumes.

Limit only what the control plane already counts. Metered dimensions (build minutes, egress, log volume) wait for 5.1 and a real need.

Prompt:

```text
Enforce per-workspace limits on services, replicas, total requested CPU and memory, volume bytes, and concurrent builds, with an operator default and a per-workspace operator override. Check limits transactionally at creation and scaling, and return an error naming the limit and current use. Show usage against limits in the workspace settings. Test concurrent admission racing to the limit and lowering a limit below current use (existing resources keep running; new ones are refused). Do not add a quota hierarchy, project-level overrides, or reservation accounting.
```

## 3.4 Audit history

Was: 6.2
Status: open
Depends on: 3.1. Deployment actions already exist and must emit events once this lands.

Prompt:

```text
Record an audit event for every authenticated mutation, in the same transaction: actor, workspace/project scope, action, target resource, a safe summary, result, and timestamp. Never store secret values or tokens. Workspace admins can list and filter events in the console with pagination. Test redaction, system actors, and tenant isolation. Do not add export, trusted source-IP capture, or configurable retention.
```

## 3.5 Workload and platform metrics

Was: 3.1
Status: open
Depends on: none. Billable rollups wait for 5.1; this item is the operational series.

Prompt:

```text
Collect per-allocation CPU, memory, network in/out bytes, and volume usage on each agent from cgroup and runtime counters, labelled with project/environment/service/allocation IDs, and remote-write them to VictoriaMetrics through a bounded buffer that drops (and counts drops) rather than growing when VictoriaMetrics is down. Make the query side aggregate replicas without double-counting across allocation replacement. Test the buffer bound during an outage and replica aggregation. Do not add build metrics, platform-component metrics, or dashboards here.
```

## 3.6 Service and environment observability views

Was: 3.2
Status: open
Depends on: 3.5 and 2.9

Prompt:

```text
Add a metrics tab on the service page: CPU, memory, and network graphs over the last hour, 6 hours, day, and week, summed across replicas, with deployment markers on the time axis. Add environment-wide and deployment-scoped log search over the 2.9 pipeline, showing an explicit gap where 2.9 reports dropped lines. Enforce authorization server-side and bound the query range. Test that one project cannot query another's series or logs. Do not add custom time ranges, anomaly linking, or environment-level metric aggregation.
```

## 3.7 Public HTTP and TCP exposure

Was: 5.4
Status: open
Depends on: 2.7a so exposure is published through the xDS path.

Prompt:

```text
Let a user expose a service publicly: pick a port and get a generated domain (custom domains come from 2.8). HTTP and WebSocket work through Envoy without extra configuration. Services with no exposure are reachable only on the private network. Also support a public TCP proxy (platform host plus an assigned port) to a declared port, so users can reach a database from their laptop. Test HTTP, WebSocket, TCP proxy, an unexposed service staying private, and cross-project isolation. Do not add per-endpoint request-size, timeout, or trusted-proxy settings.
```

## 3.9 Deploy notifications and webhooks

Was: 3.4
Status: open
Depends on: 2.1 for webhook delivery.

Prompt:

```text
Notify users when a deployment fails, a build fails, or a service crash-loops: in-app plus email through a narrow email provider, sent once per event. Add project webhooks for deployment status changes, delivered through the 2.1 durable-work queue with retry, signed with a per-webhook secret, and SSRF-safe URL validation. Show recent webhook delivery attempts. Test delivery, retry, deduplication, and SSRF rejection. Do not add metric threshold monitors, cooldown or resolve rules, or a replay UI.
```

## 3.11 Platform backup and restore

Was: 2.5
Status: open
Depends on: 2.1, 2.2, 2.3a, 2.3b. Volume restore is out of scope until 6.3.

Prompt:

```text
Take scheduled CockroachDB BACKUPs of the platform database to the configured object storage, encrypted, with a documented retention and a freshness check that fails loudly when a backup is missing. Document restore, including the keyring and the object-storage bucket the platform depends on. Exercise one backup-mutate-restore in the VM harness. Do not build an isolated recovery-environment orchestrator or pre-enable validation suite.
```

## 3.13 Platform CLI

Was: 8.2
Status: open
Depends on: 3.2 for scoped tokens. Uses the existing Connect services with the same authorization as the console; the versioned public API (3.12) is parked.

Prompt:

```text
Add a CLI for the everyday loop: login (browser or token), link a directory to a project/environment/service, deploy, logs (follow), variables and secrets (set/list/delete, never printing secret values), status, and open. Stable exit codes, JSON output with --json, --yes for destructive actions, no color when not a TTY, and explicit flags that override the linked context. Never put tokens in process arguments or debug output. Test the command contract against the control-plane API. Do not add scaling, domains, volumes, usage, or source-upload commands yet.
```

## 3.14 Per-project build cache

Was: 4.4
Status: open
Depends on: 2.4b

Prompt:

```text
Give each project its own BuildKit cache so repeat builds are fast. Never read another project's cache. Bound cache size per project with LRU eviction, and delete a project's cache with the project. Build secrets use BuildKit secret mounts and never land in layers or cache keys as plaintext. Test that a cache is reused across deploys, that cross-project reads fail, and that eviction works. Do not add cross-project base-layer sharing or hit/miss analytics.
```
