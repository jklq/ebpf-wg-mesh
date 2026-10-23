# Serverless services (scale-to-zero)

A service with serverless enabled stops all of its allocations after an idle
period and starts them again on the next request — public HTTP(S) or private
mesh traffic. The model follows
[Railway's serverless mode](https://docs.railway.com/deployments/serverless):
the setting is per service, inactivity is judged from network traffic, and the
first request after an idle period pays a cold start. Unlike Railway, the
first public request is held and then proxied once the service is ready
instead of failing with a gateway error.

## User contract

- **Opt-in per service.** `serverless_enabled` lives in the service spec, so
  it flows through the normal draft → revision → environment release path.
  Changing it takes effect on the next deployment, like every other spec
  field. It applies to every replica of the service.
- **Desired state is preserved.** Sleeping never changes
  `desired_replica_count`, domain bindings, volumes, variables, or the
  service's private addresses. Allocation assignments (and therefore their
  IPv4/IPv6 addresses) are retained across a sleep/wake cycle, so peers keep
  resolving the same internal hostname to the same addresses.
- **Idle means no network activity.** A service is eligible to sleep when no
  allocation has produced metered network activity (public direct, public
  ingress, or private classes) for the idle timeout (operator-configured,
  default 10 minutes). Platform and health-probe traffic never count.
  Anything that makes the workload emit traffic — database pools, telemetry,
  NTP, calls to sibling services — keeps it awake, exactly as on Railway.
- **Wake triggers.** (1) A public request to any of the service's domain
  bindings. (2) Private mesh traffic addressed to a sleeping allocation.
  (3) Any operation that wants the service running: a new deployment or
  environment release, a scale or restart action, or disabling serverless.
- **Cold start.** Wake restores the full desired replica count. The first
  public request is released as soon as one replica is ready. Images are
  pinned on the agent while a service sleeps, so a wake never re-pulls unless
  the agent lost its cache.
- **Billing.** A stopped allocation meters no CPU, memory, or egress, so a
  sleeping service accrues no compute usage. Attached volumes keep accruing
  volume usage, and the workspace plan minimum still applies. Sleeping
  services still count against desired-state quotas.
- **Status.** Services report a serverless state — `awake`, `asleep`, or
  `waking` — distinct from health: asleep is not crashed, and a sleeping
  service's deployment stays active.

## Caveats (documented behavior, not bugs)

- The first mesh packet(s) to a sleeping allocation are dropped while it
  wakes. TCP connections recover through SYN retransmission; connectionless
  first-flight payloads (UDP) are lost. Callers should use connect timeouts
  comfortably above the cold-start time.
- Ingress-only traffic that produces no outbound bytes at all (for example a
  pure UDP sink) is invisible to the egress-based activity signal and does
  not by itself keep a service awake.
- A public request that arrives while a wake is already in progress is held
  with the same deadline as the request that started the wake. If the wake
  deadline (operator-configured, default 90 seconds) passes first, the
  activator answers `503` with `Retry-After: 5` and the wake continues in
  the background.
- Long-lived connections (WebSockets, gRPC streams) keep a service awake
  while they carry traffic; once the service does sleep, all connections
  were already gone — sleep only happens after the full idle window.

## How it works

### Sleep

Allocation assignments carry a runtime intent. Serverless adds `sleep`
beside `run` and `drain`:

1. The control plane's idle evaluator (a singleton maintenance loop) finds
   serverless services whose last network activity is older than the idle
   timeout. Services with an in-progress rollout, a non-active deployment,
   or an enforced billing restriction are skipped.
2. The service is marked asleep and the ingress snapshot is republished:
   its hostnames now route to the shared **activator** cluster instead of
   allocation endpoints. Private host entries and mesh identities are
   retained.
3. Each assignment's intent flips to `sleep` with a drain deadline. The
   agent stops the workload gracefully (SIGTERM, then force-kill at the
   deadline), removes the container, and reports phase `Sleeping`. The
   image stays pinned against garbage collection.

Last activity is derived from sealed usage records (one-minute granularity),
so a service sleeps between the idle timeout and roughly two minutes past
it. A wake stamps activity at the wake instant, so a service always gets a
full idle window after starting before it can sleep again.

### Public wake

Envoy routes an asleep service's hostnames to a small **activator** process
co-located with each ingress instance. The activator:

1. Calls the control plane's internal `Wake` RPC with the request's
   hostname. The control plane resolves the hostname to a service
   server-side; the activator never asserts a service identity itself.
2. The control plane flips every assignment back to `run` (idempotently —
   all wake triggers converge on this one entry point), republishes the
   ingress snapshot with real endpoints, and the agent starts the
   containers from the pinned image.
3. The RPC returns when at least one endpoint is ready (or the deadline
   passes). The activator then proxies the held request — body, headers,
   and connection upgrades included — to that endpoint. Later requests go
   through the normal Envoy route again.

The activator runs under its own `activator` caller-class credentials,
separate from the ingress xDS identity, and can only call `Wake`.

### Private wake

Sleeping allocations keep their mesh identity seeds, so packets from peers
still route to the right host. The eBPF firewall counts packets whose
destination is a local identity with no live veth (both the same-host and
WireGuard ingress paths) in a per-CPU wake-counter map keyed by destination
address. The agent polls the map with its existing metering cadence and
reports wake demand per allocation on its sync stream; the control plane
treats it exactly like an activator wake. Packets that triggered the wake
are dropped (see caveats).

### Failure handling

- **Agent dies while a service sleeps on it.** The retained assignment goes
  through the normal lost-allocation path; the next wake re-places it
  (volume-backed services stay pinned to their volume's agent, as today).
- **Wake times out.** The activator fails the held request with `503`, but
  the wake itself is not cancelled; the next request either finds the
  service awake or holds again.
- **Control plane restarts.** Sleep state is durable (service delivery
  status), so a restart neither wakes everything nor forgets what is
  asleep. The idle evaluator resumes from sealed usage records.
- **Flapping.** Wake stamps activity, and eligibility requires a full idle
  window with zero activity, so sleep/wake cycles are at least one idle
  timeout apart.

## Operator configuration

| Setting | Default | Meaning |
| --- | --- | --- |
| `serverlessIdleTimeout` | `10m` | Inactivity after which a serverless service sleeps. |
| `serverlessWakeDeadline` | `90s` | How long the activator holds the first request. |

Metrics: `platform.serverless.sleeps`, `platform.serverless.wakes{trigger}`,
and `platform.serverless.wake.duration` are exported with the rest of the
control-plane telemetry; sleep and wake also appear in the service's
deployment history.

The activator is deployed per ingress host with the unit in
`infra/production/ingress/platform-activator.service` and its own
`activator` caller-class identity (see INGRESS.md). Rolling it into the
platformctl deployment plan as an Envoy sidecar — unit rendering, credential
provisioning, and probes — is tracked follow-up work; the manual unit is
the production path until then.
