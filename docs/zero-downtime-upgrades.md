# Listener handover

Rolling upgrades ([production deployment](production-deployment.md)) decide the
order in which replicas change. Listener handover decides how one replica
changes. A role that declares `listeners` keeps its public sockets in a resident
owner process, so a new process generation takes over without closing them. A
single replica then upgrades, or loads renewed credentials, without an outage.

Only the control plane, console, registry and Envoy can declare listeners. The
release must pin the `continuity` tool.

```yaml
programs:
  controlplane:
    listeners:
      - address: ':9443'
        env: CONTROLPLANE_INTERNAL_LISTEN
```

## One handover

Each replica's systemd unit runs `continuity serve` and owns the declared
addresses. For each launch, the owner:

1. Starts the new process on private loopback ports, given through each
   listener's `env` (`portOnly` passes only the port), with a private copy of
   its configuration.
2. Waits for the release's `ready` command, and checks that the new process
   owns those private sockets.
3. Durably records the launch as its boot target, then sends new connections
   to it. Existing connections stay with the old process. Requests are never
   replayed.
4. On `finish`, runs the release's `stop` command against the old process's
   private addresses, then sends SIGTERM. Each role already drains on SIGTERM
   (see [graceful restarts](production-deployment.md#graceful-restarts)):
   control-plane streams reconnect to the new process, Envoy closes downstream
   connections gracefully, and the console finishes active requests.

If the new process exits before it is ready, the old one keeps serving. If it
exits after the handover while the old process is still draining, new
connections return to the old process.

Envoy keeps one xDS identity across generations. Each generation receives a
projection of the canonical snapshot that changes only listener ports, and an
ACK counts only when every live generation of that identity has applied it.
Hot restart is disabled. The admin and health listeners are projected the same
way. The health listener address must match the installation's
`ingressHealthListen`.

Control-plane certificate caches and ingest journals are per generation. File
logs are shared under an interprocess lock and one retention budget. A
surviving generation adopts an abandoned journal once its lock is free.

## In a rolling upgrade

A replica with listeners runs `configure`, `install`, `finish`. The canary and
wave limits still apply across replicas. Its configuration is written to
`{configDir}/releases/{release}`, so the serving process's inputs are never
rewritten.

The owner restarts instead of handing over when it is first adopted, when
listener addresses change, or when the `continuity` executable changes.
Executables are content-addressed, so a running owner is never replaced
underneath itself. The restart goes through the unit's normal `ExecStop`
drain. Replicated roles stay available through the rolling waves, and the plan
lists every sole replica that restarts in `upgrade.limitations`.

Replicas whose program, configuration, dependencies and replica count are
unchanged are not touched. An unchanged database stays running, so a
single-node installation can upgrade everything else. Agents and builders still
restart: an agent keeps workload forwarding but reconnects its management
session, and a builder drains admitted builds first. Both appear in
`upgrade.limitations`.

## Capacity

Only one handover per host overlaps at a time. The plan charges each host one
extra copy of its largest changed listener process, plus the resident owner and
staged artifacts. `upgrade-capacity` blocks the plan when the declared or
observed capacity cannot hold that peak. Before downloading, the
`upgrade-preflight` step checks free space and inodes on each host, using the
artifact `bytes` and `unpackedBytes` that `make package-production` records.
The reservation hook holds the same overlap on each host's agent until the
release is finalized. Credential renewal reserves it for its own handovers. The
owner refuses a launch when available memory cannot hold it, and enforces each
process's CPU and memory budget through its delegated cgroup.

## Rollback and repair

```sh
platformctl rollback                  # interrupted upgrade, schemas unchanged
platformctl repair --bundle r45.json  # new release, same schemas and changes
```

`rollback` builds a reverse rolling plan to the previous release. It is
available only while both schemas are unchanged, and it never restores data.
After a schema change, `repair` continues forward with a new release identity
that keeps the same configuration, protocol, schemas and `schemaChanges`. A
schema change that cannot go forward needs a restore. Restore is disaster
recovery with an explicit data-loss cutoff, never an automatic response to a
failed upgrade.

## Limits

- Listener handover covers software and credential changes. Host, kernel,
  container runtime and database binary changes follow the rolling rules,
  including at least three database members.
- A crash of the owner or the host interrupts that replica, like any process
  crash.
- Drains are bounded by the stop timeout, so very long requests and streams can
  still be cut when the old generation stops.
