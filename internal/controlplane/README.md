# Control plane

`Server` assembles and runs the control plane. Transport handlers call the domain
modules; they do not implement scheduling or deployment transitions.
`Server` and `NewServer` are the package's exported entry points. Internal
constructors, transport handlers, and persistence plumbing stay private.

| Files | Responsibility |
| --- | --- |
| `server.go`, `connect.go` | Runtime assembly, lifecycle, and transport registration |
| `platform*.go`, `agent_service.go`, `ops_service.go` | Authenticated platform, fleet-agent, and operator requests |
| `builds.go` | Builder requests, credentials, and authorized source-snapshot streaming |
| `catalog.go`, `environments.go` | Projects, environments, volumes, and catalog operations |
| `deletions.go`, `deletion_gc.go` | Deletion previews, restoration, and eventual collection |
| `fleet.go`, `routing.go` | Fleet transport authority and routing persistence adapters |
| `database.go`, `schema.go`, `lease.go`, `events.go` | Persistence assembly, transactions, schema, ownership fencing, and event revisions |
| `presentation.go` | Product records rendered into protocol responses and deployment stages |
| `reconciliation.go`, `managed_dashboard.go` | Periodic delivery work and managed-dashboard setup |

Catalog, fleet, and routing persistence handles own their respective operations.
Delivery supplies the shared read model directly. Its environment snapshot reads
service drafts, volumes, and the event index in one transaction. Runtime observations
are added separately. Draft replacement requires the caller's expected spec revision.
Journal transactions apply durable changes to the same live view used by transport and routing readers.

## Delivery

`delivery.Delivery` owns the delivery lifecycle. Its constructor accepts the real
external dependencies; persistence helpers, allocation mutations, and scheduling
policies remain private. Files group commands with their closely related
persistence operations.

| Files in `delivery/` | Responsibility |
| --- | --- |
| `delivery.go`, `reads.go`, `environment_snapshot.go` | Construction, dependencies, and authorized or system reads |
| `services.go`, `service_changes.go`, `service_spec.go` | Service drafts, unapplied changes, and spec invariants |
| `environments.go`, `managed_service.go`, `env.go` | Environment releases and duplication, managed workloads, and env encryption at rest |
| `deployments.go`, `deployment_actions.go` | Deployment history, transitions, and user actions |
| `rollouts.go`, `rollout_plan.go` | Rollout reconciliation and replacement decisions |
| `allocations.go`, `placement.go` | Assignment mutations, replica reconciliation, and placement |
| `fleet.go`, `fleet_reconciliation.go` | Operator intent, maintenance drains, and failover |
| `agent_sessions.go`, `agent_state.go`, `agent_invalidation.go`, `allocation_sync.go` | Registration, status reports, desired state, and retained diffs |
| `build_requests.go`, `build_scheduler.go`, `build_results.go`, `artifacts.go` | Build submission, execution leases, completion, and immutable images |
| `live.go`, `live_sessions.go`, `live_view.go` | Live ownership, session fencing, observations, and indexed reads |
| `deletion.go`, `volumes.go` | Transactional deletion effects, volume reads, and retained-volume status |
| `model.go`, `convert.go` | Delivery records and build/source protocol rendering |

Delivery owns deletion quiesce, assignment withdrawal, and volume-destruction targets
inside catalog transactions. Desired-state invalidation lives beside agent-state
rendering and uses the same rendered peer fields. Database plumbing commits the
resulting desired-revision bumps.

Pure rollout decisions remain separate from their transactional execution. Live
ownership, agent sessions, and live indexes are also separate because they have
different synchronization responsibilities.

Unit tests exercise policies and transport behavior. The integration suites use a
real CockroachDB instance to exercise transactions, live ownership, builds,
deployments, and allocation observations through the existing entry points:

```sh
go test ./internal/controlplane/...
go test -tags=integration ./internal/controlplane
```
