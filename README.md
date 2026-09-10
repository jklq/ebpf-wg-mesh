# ebpf-wg-mesh

Minimal PaaS control plane and agent prototype with a WireGuard/eBPF private fabric.

## Current shape

- `cmd/controlplane`: authoritative control plane
- `cmd/agent`: node agent that opens an mTLS gRPC stream to the control plane
- `cmd/builder`: build worker that claims jobs, materializes source snapshots, runs `buildctl`, and reports status
- `console`: TanStack Start app that owns browser auth/session state and calls the control plane over internal mTLS gRPC
- `internal/controlplane`: CockroachDB store, internal gRPC authz/authn, managed dashboard reconciliation, agent stream handling, Caddy admin sync
- `internal/agent`: desired-state loop, local reconcile runtime, containerd runtime, status reporting
- `internal/mesh`: mesh bootstrap that wraps the WireGuard and eBPF implementation
- `api/proto`: protobuf definitions and generated gRPC bindings

## Architecture

What the code does today. Target invariants live in [docs/todo/README.md](docs/todo/README.md); do not extend the current shapes called out as going away.

Today every agent still receives a cluster-wide identity catalog and a WireGuard peer for every other enrolled agent, the agent wire format is a full per-node desired-state snapshot (not start/update/stop diffs), and public ingress is a single Caddy driven through the admin API.

The stream contract, durable acknowledgements, removal scope and authority-expiration assumptions are defined in [Agent reconciliation](docs/agent-reconciliation.md).

- CockroachDB is the authoritative control-plane store for projects, memberships, repository grants, environments, agents, services, revisions, volumes, domains, allocations, and status projections. The console uses its own schema in the same cluster for users, sessions, and onboarding. Probe ticks, logs, and metrics samples do not belong in Cockroach.
- The control plane is authoritative for placement; an agent is authoritative for supervising the accepted work assigned to its node. Each agent durably commits its full node snapshot, authority epoch, reconciliation cursor, allocation generations, runtime identities, pending operations, drains, and observation sequence in `agent-state.db`. On restart it discovers stable `platform-<allocation-id>` runtime resources and reconciles them before connecting, so loss of a control-plane connection never ends workload supervision. Pull credentials are stored separately inside the mode-`0600` database and renewal does not manufacture an allocation change.
- A missing database with discovered managed resources, or any corrupt database, enters recovery. Corrupt data is quarantined, unknown resources are reported in the hello inventory, and destructive pruning remains disabled until an authenticated full snapshot establishes their ownership or authoritative absence. Without a prior cluster binding, every discovered resource must be claimed before the fence is lifted.
- Policy is fail-closed and identity-based: a workload-pool deny plus exact allows. Those allows are currently the whole-cluster catalog. The target is exact allows only for environments the node currently hosts, including remote allocations in those environments.
- WireGuard is overlay transport and eBPF is identity policy. Peering is currently a full mesh of enrolled agents; each peer's AllowedIPs are that peer's overlay IPv4 and IPv6 prefixes. The target is peers only between nodes that share an environment and between those nodes and the Envoy instances that publish their services.
- Public ingress is currently one Caddy instance. The control plane replaces Caddy config through the admin API. The target is an Envoy fleet: the control plane serves a versioned xDS snapshot and ACK/NACK is the apply protocol. Do not extend Caddy as the production data plane.
- Overlay dual-stack is live: every allocation gets IPv4 and IPv6, both are routed in AllowedIPs, and both are enforced by the same `network_identity`. That does not change the underlay; `advertise_addr` stays IPv6 until an explicit underlay prompt exists.
- The console is the current product-facing caller of `platform.v1.PlatformService`. A public API, when it exists, must use the same application services.
- Agent-facing and console-facing internal gRPC are protected by mTLS with distinct caller identities.
- The console owns OAuth, canonical user profiles, and browser sessions. For each product RPC it signs a 30-second user assertion with a control-plane audience; the control plane verifies the signature, issuer, audience, lifetime, and subject before applying project membership and role authorization.
- GitHub repositories are linked to a project only after the console confirms that the signed-in GitHub account can see them. Inspection, service mutation, and background source reconciliation use the project-specific repository grant. GitHub App grants produce immutable source snapshots; scheduled workloads already require a persisted image digest, but the full deploy-by-digest artifact contract is still open.
- Registry authorization is part of the control-plane process. It mints short-lived Distribution bearer tokens and exact-repository builder/agent capabilities; the registry verifies those tokens locally from the control-plane signing certificate.
- Control-plane replicas have no node-local authority as a production contract. Shared `CONTROLPLANE_STATE_DIR` / source-archive disks are a current constraint, not that contract.

## Bootstrap

- `controlplane` bootstraps from flags and environment, then owns node mesh/workload assignment in CockroachDB.
- `agent` bootstraps from flags and environment, discovers local host facts, persists its own WireGuard private key, enrolls, and waits for assigned node config from the control plane.

### Multiple control-plane replicas

Control-plane replicas coordinate singleton reconcilers through a fenced CockroachDB lease. Agent streams and platform blocking reads observe durable database revisions, so a write handled by one replica wakes clients connected to another. Replica clocks are not used for lease, rollout, or failover decisions.

Agents accept a comma-separated `AGENT_CONTROLPLANE_ADDRESSES` (or `--controlplane-addresses`) seed list. Each agent start replaces the durable seed set with that config; successful enrollment, dashboard-certificate, and sync responses replace the durable replica view independently. Agents use the union only to find a reachable replica; a non-owner replica returns a live-owner redirect so the agent pins and reconnects to the leaseholder. A dead owner is quarantined briefly so a redirect cannot immediately re-pin it, and that cooldown is not extended by later redirects. Bootstrap enrollment is retry-safe for the same agent key after a lost response.

Every replica for one database must currently mount the same read-write `CONTROLPLANE_STATE_DIR` and `CONTROLPLANE_SOURCE_ARCHIVES_DIR`. These directories contain the shared internal PKI, registry identity, revocation data, and source objects. Startup binds both mounts to the database using persistent storage markers and fails if a replica is pointed at node-local or replacement storage. That shared-disk contract is scheduled to go away once source object storage and a key provider land. Every replica must currently point at the same Caddy admin target; when Envoy lands, that becomes a shared xDS snapshot identity.

Common bootstrap inputs:

- control plane: listen addresses, advertised replica addresses (`CONTROLPLANE_REPLICA_ADDRESSES`), this replica's dial address (`CONTROLPLANE_ADVERTISE_ADDR`, required when more than one replica address is set), single-use agent-bound bootstrap token(s) (`agent_id=token`), DB URL, state dir, ingress admin URL, managed console service settings
- agent: control-plane seed address(es), control-plane CA, bootstrap token, data dir

### Ingress admin security (current Caddy)

Until the Envoy fleet cutover, the rendered Caddy admin listener and the control-plane admin URL default to `127.0.0.1:2019`. Non-loopback admin listeners or URLs are rejected unless `CONTROLPLANE_INGRESS_ALLOW_NON_LOOPBACK_ADMIN=1` (or `--ingress-allow-non-loopback-admin`) is set explicitly. When opting in, set the rendered listener with `CONTROLPLANE_INGRESS_ADMIN_LISTEN` and protect the admin transport with network isolation and authenticated TLS; the control plane does not add Caddy admin credentials. The local Docker test stack opts in because its loopback-published port must bind inside the Caddy container.

### Deployment and health semantics

`UpdateService` stages a new service revision. `ReleaseEnvironment` publishes every staged revision in one transaction. Deployment-history operations use `ApplyDeploymentAction` with a selected `deployment_id` and a caller-generated `idempotency_key`; reusing a key for the same request is a no-op, while reusing it for different parameters is rejected.

The supported deployment actions are:

- `RESTART` restarts one selected allocation, or every replica when `allocation_id` is empty, without building an image or creating a rollout.
- `EXACT_REDEPLOY` creates a rollout from the selected deployment's persisted resolved spec and digest-pinned image. It does not inspect source or fetch a newer revision.
- `ROLLBACK` creates a new rollout from a non-current successful deployment, including the environment-variable versions captured with that deployment. Existing deployment rows remain history.
- `CANCEL` terminates current queued, building, or deploying work. Builders learn cancellation through their heartbeat, and late builder or agent reports cannot move the cancelled deployment out of its terminal state. The control plane restores the last successful digest-pinned deployment when one exists.
- `REMOVE` withdraws ingress, drains the current allocation set with the runtime's graceful-stop path, and retains the service and deployment history.
- `RETRY` retries failed, cancelled, or crashed work. Failed builds reuse their immutable source snapshot; runtime failures reuse the persisted resolved spec and image digest.

Owners and editors may apply actions; viewers cannot. The console exposes only actions valid for each history row and records each accepted action in that deployment's history. A `NotFound` response means the selected deployment or allocation no longer exists; `FailedPrecondition` means it exists but is stale or is in an incompatible state, so callers should refresh deployment history before deciding whether to issue a new action with a new idempotency key.

Readiness checks are currently rollout gates, not continuous monitors. With no health check configured, a deployment becomes ready as soon as its process is running. With an explicit HTTP readiness check, the agent retries the endpoint over both overlay families while the rollout is starting and marks the deployment ready after an HTTP `200` on IPv4, IPv6, or both. That successful readiness result is latched for the rollout and is not queried again during ordinary reconciliation; an optional HTTP liveness check can still run after ready and request a restart. The readiness latch is scheduled to go away (see [docs/todo/01-running-service.md](docs/todo/01-running-service.md#13-continuous-readiness-and-liveness)).

Deployments use a persisted rolling strategy with platform-managed replacement concurrency: healthy capacity is preserved and at most one extra allocation is created at a time. The user-configurable defaults are a 300-second healthcheck timeout and 30 seconds of draining time. The control plane creates replacement allocations alongside the serving generation, waits for readiness, publishes the healthy replacement and withdraws its predecessor from ingress, and only sends the predecessor a drain intent after that ingress update succeeds. Agents send `SIGTERM`, preserve the container and network namespace during the draining window, and use `SIGKILL` only after the absolute deadline. Control-plane and agent restarts resume from the CockroachDB allocation state and desired drain deadline. A readiness or scheduling timeout fails the rollout without removing healthy serving allocations. Services with a single-writer volume reject replacement rollouts until the stateful volume attachment handoff and fencing protocol exists (see [docs/todo/06-stateful.md](docs/todo/06-stateful.md)).

HTTP health-check paths must be absolute request paths beginning with a single `/`. Checks never follow redirects or use proxy environment variables. Production requests originate in the workload's persisted network namespace and target only the control-plane-assigned overlay IPv4 and IPv6 addresses and configured port (or the primary declared port when no check port is set). Ingress publishes one upstream per healthy allocation, preferring IPv4 when that family passed the probe. A missing or stale namespace keeps readiness pending; the agent does not fall back to host-network probing.

`runtime.cpu_millis` is enforced with Linux CFS quota using a 100 ms period (for example, `500` millicpu becomes a `50 ms / 100 ms` quota). It is not interpreted as a cpuset.

Customer workloads always run under one production OCI sandbox: the image USER is preserved, the ephemeral overlay root is writable, and a bounded Docker-like capability set supports ordinary images while excluding administrative/network capabilities. The agent also enforces `no_new_privileges`, seccomp, AppArmor or SELinux when the host supports them, isolated namespaces, masked dangerous proc/sys paths, PID limits, and hard cgroup v2 CPU/memory/swap/OOM controls. The customer API has no privileged, host-network, host-PID, sysctl, device, bind-mount, or sandbox-profile fields. See [docs/workload-isolation.md](docs/workload-isolation.md).

### Agent fleet

Operators enroll, cordon, drain, and retire compute nodes from the console **Fleet** page. Region, zone, failure domain, and host reservations are operator policy; agents only report observed capacity, capabilities, software version, and heartbeat. New placement skips cordoned/draining/unavailable/retired nodes, spreads replicas across failure domains when it can, honors `placement_region`, and leaves a pending explanation when it cannot. Drain moves stateless allocations through the existing failover path; volume-backed allocations stay fenced until stateful volume handoff exists. Retirement revokes credentials and removes mesh identity only after allocations are gone. See [docs/operator-fleet.md](docs/operator-fleet.md).

Each service can generate a stable platform hostname under `CONTROLPLANE_INGRESS_PUBLIC_ADDR`, such as `violet-7k3.platform.example`. The Domains panel exposes separate **Generate Domain** and **Custom Domain** actions. The custom flow creates the platform hostname when needed, then keeps the required record (`app.customer.com CNAME violet-7k3.platform.example`) visible until verification succeeds. The control plane resolves and verifies the CNAME itself before routing the custom hostname; no TXT challenge is required.

Services also receive an environment-private hostname derived from their unique service name, such as `accurate-reflection.mesh.internal`. Workloads in the same environment can use either the full hostname or the short `accurate-reflection` alias; these names resolve to the service's private IPv4 and IPv6 addresses and are not published externally.

## Build

Generate protobuf and BPF artifacts as needed:

```bash
go generate ./api/proto
go generate ./internal/firewall
```

Run tests:

```bash
make test-unit-go
make test-unit-console
make test-integration
```

Dashboard-local Bun commands:

```bash
bun --cwd=console install
bun --cwd=console run test:unit
bun --cwd=console run test:integration
bun --cwd=console run test:e2e:local
bun --cwd=console run build
bun --cwd=console run start
```

Smoke E2E against a local ephemeral stack (requires the public Cloudflare tunnel
credentials used by `LOCALTESTSTACK_ENABLE_PUBLIC_TUNNEL=1`; open dev logins stay
disabled on the public hostname, and Playwright authenticates with a per-run
session cookie written only to `console/artifacts/e2e-local/stack.json`):

```bash
make test-e2e-local
```

Interactive ephemeral local stack for manual exploration:

```bash
make dev-ephemeral
```

GitHub-enabled interactive devstack via 1Password Environments:

**Preferred for local dev:** mount the Environment as a repo-root `.env` in the 1Password desktop app (Developer → Environments → local `.env` destination). `localteststack` loads that file automatically, including 1Password’s FIFO mount. No `OP_*` vars are needed.

Public GitHub OAuth/webhook mode is deliberately opt-in because it exposes the local ingress through Cloudflare. Start it with strong per-run session secrets and GitHub auth (dev users are disabled in this mode):

```bash
LOCALTESTSTACK_ENABLE_PUBLIC_TUNNEL=1 make dev-ephemeral
```

**Headless / automation (SDK):** load the Environment remotely when the local contract is incomplete:

```bash
export OP_ENVIRONMENT_ID=envs/...
export OP_SERVICE_ACCOUNT_TOKEN=ops_... # preferred; full token from a service account
# or: export OP_ACCOUNT=my.1password.account
LOCALTESTSTACK_ENABLE_PUBLIC_TUNNEL=1 make dev-ephemeral
```

You can also put bootstrap or feature keys in a normal gitignored `.env` (process env still wins over file values):

```bash
# .env — either a 1Password-mounted Environment, or plain KEY=VALUE pairs
# CONTROLPLANE_GITHUB_APP_ID=...
# CLOUDFLARE_TUNNEL_TOKEN=...
# CLOUDFLARE_HOSTNAME=...
```

- When neither process/`.env` nor a remote Environment provides the GitHub contract, `make dev-ephemeral` keeps the existing local-only behavior.
- When secrets are already complete in process env / mounted `.env`, the 1Password SDK is skipped even if `OP_ENVIRONMENT_ID` is set (avoids bad/placeholder service-account tokens breaking local runs). They are not activated unless `LOCALTESTSTACK_ENABLE_PUBLIC_TUNNEL=1` is also set.
- When the remote Environment is readable but incomplete, the stack still starts and logs the missing key names while leaving GitHub disabled.
- Service-account auth takes precedence over desktop-app auth when both `OP_SERVICE_ACCOUNT_TOKEN` and `OP_ACCOUNT` are set.
- `localteststack` loads `.env` from the repo root before reading config. Already-exported shell variables are not overwritten.

Required 1Password Environment keys for GitHub-enabled `make dev-ephemeral`:

- Public endpoint: `CLOUDFLARE_TUNNEL_TOKEN` and `CLOUDFLARE_HOSTNAME` for a pre-provisioned Cloudflare Tunnel. `localteststack` starts `cloudflared` locally and uses `https://{CLOUDFLARE_HOSTNAME}` as the public base URL.
- Control plane GitHub: `CONTROLPLANE_GITHUB_APP_ID`, `CONTROLPLANE_GITHUB_WEBHOOK_SECRET`, `CONTROLPLANE_GITHUB_PRIVATE_KEY_PEM` (base64-encoded PEM is recommended if your secret store strips newlines).
- Optional control plane overrides: `CONTROLPLANE_GITHUB_API_BASE_URL`, `CONTROLPLANE_GITHUB_WEB_BASE_URL`, `CONTROLPLANE_GITHUB_WEBHOOK_PATH`.
- Optional dashboard install link: `CONTROLPLANE_DASHBOARD_GITHUB_INSTALL_URL`.
- Console GitHub auth: `DASHBOARD_GITHUB_APP_ID`, `DASHBOARD_GITHUB_CLIENT_ID`, `DASHBOARD_GITHUB_CLIENT_SECRET`.
- Console token storage: `DASHBOARD_GITHUB_TOKEN_ENCRYPTION_KEY` (required even when GitHub login is disabled; a base64url/base64 encoding of exactly 32 random bytes, with `_FILE` supported). Generate it independently with `openssl rand -base64 32`; do not reuse the JWT or user-assertion secret.
- Optional console overrides: `DASHBOARD_GITHUB_AUTH_BASE_URL`, `DASHBOARD_GITHUB_API_BASE_URL`.

### Embedded registry authorization

The registry ACL and token minter run inside `cmd/controlplane`; there is no credential-broker deployment. Build credentials expire after `CONTROLPLANE_REGISTRY_CREDENTIAL_TTL_SECONDS` (60–900 seconds, default 300) and contain only `pull,push` for the assigned build repository. Desired agent state carries a durable pull-only capability for that same exact repository so a node can cold-pull after a restart. The capability can mint only short-lived registry access tokens and cannot read a parent namespace or sibling project/build.

The token endpoint is `http://CONTROLPLANE_REGISTRY_AUTH_LISTEN/v1/registry/token`. Keep the listener private and publish it through HTTPS for non-local clients. The signing key and trust certificate are generated once under `CONTROLPLANE_STATE_DIR/registry-auth/`; protect the key and persist that directory across restarts.

Configure a CNCF Distribution registry to trust it:

```yaml
auth:
  token:
    realm: https://controlplane.example.test/v1/registry/token
    service: registry.example.test
    issuer: ebpf-wg-mesh
    rootcertbundle: /run/secrets/registry-auth-signing-cert.pem
```

`service` and `issuer` must match `CONTROLPLANE_REGISTRY_TOKEN_SERVICE` (defaults to `CONTROLPLANE_REGISTRY_HOST`) and `CONTROLPLANE_REGISTRY_TOKEN_ISSUER`. Mount `CONTROLPLANE_STATE_DIR/registry-auth/signing-cert.pem` at the registry's `rootcertbundle` path. The local harness performs this wiring automatically and exercises the same token challenge/ACL flow with a managed `registry:2` container.

Cloudflare Tunnel prerequisites:

- `cloudflared` is installed locally.
- The Cloudflare tunnel is created outside `localteststack`.
- The public hostname is already assigned/routed to that tunnel in Cloudflare, and the tunnel origin points at the local ingress origin `http://platform.localtest.me:8080` by default, or whatever `LOCALTESTSTACK_INGRESS_HOST` and `LOCALTESTSTACK_INGRESS_PORT` resolve to in your shell.
- The tunnel token and hostname are stored in the 1Password environment or exported in the shell as `CLOUDFLARE_TUNNEL_TOKEN` and `CLOUDFLARE_HOSTNAME`.

Hostname shape and free SSL:

- Prefer **not** putting the tunnel on the zone apex if the apex is a real site or protected by Cloudflare Access.
- Recommended split (free Universal SSL, no Advanced Certificate Manager):
  - `CLOUDFLARE_HOSTNAME=mesh.relay5.com` — dashboard / GitHub OAuth / webhooks (CNAME → tunnel).
  - `LOCALTESTSTACK_PLATFORM_DOMAIN_SUFFIX=relay5.com` — generated service hosts `*.relay5.com` (CNAME `*` → tunnel). Covered by free `*.relay5.com` certs.
  - Leave apex `relay5.com` as normal **A/AAAA** to production. Cloudflare’s “CNAME on apex / flattening” note only applies if you CNAME the apex; you do not need that for this setup.
- Nested hosts such as `*.mesh.relay5.com` are **not** on Universal SSL and need Advanced Certificate Manager (or a dedicated zone).
- Tunnel public hostnames: `mesh.relay5.com`, optionally `*.mesh.relay5.com`, and `*.relay5.com` → local ingress. Do **not** attach the apex if Access or production content lives there.
- Existing **named** subdomains with their own DNS records (for example `www`) keep those records and are not overridden by the wildcard.

One-time operator setup:

- Create the tunnel in Cloudflare.
- Assign `CLOUDFLARE_HOSTNAME` and the generated-host wildcard (`*.relay5.com` in the split above) to the tunnel. Do not attach the zone apex if Access or production content lives there.
- Set the tunnel origin to the local ingress URL exposed by `make dev-ephemeral`.
- Obtain the tunnel token.
- Store the token and hostname in 1Password or shell env.

Derived GitHub URLs from `https://{CLOUDFLARE_HOSTNAME}`:

- Callback URL: `{publicBaseURL}/auth/callback`
- Webhook URL: `{publicBaseURL}/webhooks/github` by default, or `{publicBaseURL}{CONTROLPLANE_GITHUB_WEBHOOK_PATH}` when that override is set.

Production-replica VM smoke on Hetzner:

```bash
# Put HCLOUD_TOKEN in repo-root .env (or export it); process env wins over .env
make test-e2e-vm
```

## Testing Pyramid

- `test-unit-go`: pure Go tests only. Cockroach-backed store coverage is excluded from this tier.
- `test-unit-console`: Vitest unit tests for console session logic, loaders, and React rendering.
- `test-integration`: Cockroach-backed Go tests behind the `integration` build tag, plus console Vitest tests that run against `cmd/testdb`.
- `test-e2e-local`: thin Playwright smoke against an ephemeral local Cockroach + control plane + console stack.
- `test-e2e-vm`: Hetzner-backed smoke that provisions disposable VMs, deploys pinned binaries, runs remote checks, collects artifacts, and destroys the environment.

The Cockroach-backed integration suite includes regressions for cluster-wide cross-node identity distribution/removal and direct-image `A` → `B` redeploys. Agent unit coverage checks failed probe phases/reasons, redirect refusal, and millicpu-to-CFS conversion.

## VM Harness

- Infrastructure lives under `infra/test-vm/`.
- `cmd/testvm` builds Linux binaries, provisions the Hetzner topology through OpenTofu, waits for server readiness with `hcloud-go`, deploys two independent control-plane processes and two agents over SSH, runs the gRPC scenarios, collects host artifacts, and tears everything down.
- The multi-replica scenario pins agent streams and blocking reads to one replica while writing through the other, rejects replica-local control-plane storage, stops the original singleton owner, and verifies fenced lease takeover plus ingress re-publication before continuing the rollout and agent-failover checks.
- Cockroach and the two control-plane processes are colocated on the control-plane VM; the processes use the same database and state directory while retaining independent in-memory state and lifecycles.
- Artifacts are written under `artifacts/e2e-vm/<run-id>/`.

## Internal mTLS

- The control plane auto-creates an internal CA and gRPC server certificate under `CONTROLPLANE_STATE_DIR/pki`.
- Agents no longer need pre-generated client certificates. Each agent generates its own key in `AGENT_DATA_DIR/tls`, enrolls once with its `AGENT_BOOTSTRAP_TOKEN`, receives a short-lived mTLS certificate, and renews it automatically before expiry. Configure the control plane with `CONTROLPLANE_AGENT_BOOTSTRAP_TOKENS=agent_id=token[,agent_id=token...]`; bindings are durable and consumed atomically.
- Agents still need the control-plane CA certificate for the initial TLS trust root. In the devstack this is shared from the control-plane data volume; on separate VPSes, copy the public `ca.crt` once.

### Revoke a compromised internal client

The control plane maintains a persistent leaf-certificate serial denylist at `CONTROLPLANE_STATE_DIR/pki/revoked-client-cert-serials.txt`. Override it with `CONTROLPLANE_INTERNAL_REVOKED_CLIENT_CERT_SERIALS_FILE` when that file must live on a separately managed persistent volume. The file accepts one hexadecimal serial per line, optional `:` separators, blank lines, and `#` comments. It is re-read at every authenticated handshake and RPC, so atomic file replacement takes effect without a control-plane restart. An unreadable or malformed denylist fails closed for authenticated clients.

For an agent compromise:

1. Stop the agent and record the leaf serial with `openssl x509 -in "$AGENT_DATA_DIR/tls/client.crt" -noout -serial`.
2. Add the serial value to the denylist. The revoked certificate can no longer call Sync or authenticated Enroll, so it cannot renew itself.
3. Replace that agent's entry in `CONTROLPLANE_AGENT_BOOTSTRAP_TOKENS` with a new random, agent-bound token and restart the control plane to persist it. Never reuse a consumed or exposed token.
4. Move the whole compromised TLS directory aside, for example `mv "$AGENT_DATA_DIR/tls" "$AGENT_DATA_DIR/tls.compromised"`, configure the matching fresh `AGENT_BOOTSTRAP_TOKEN`, and restart the agent. Removing the whole directory rotates the private key as well as the certificate. The agent can recover only through certificate-free Enroll, where the fresh token is checked and consumed once.
5. After verifying Sync with the replacement certificate, securely remove the quarantined TLS material.

Builder and dashboard leaf certificates use the same denylist and are rejected across their RPC surfaces. After revoking one, rotate its private key and certificate material before restarting that client; do not remove the serial from the denylist.

## Managed dashboard trust boundary

The managed dashboard must run on a dedicated, explicitly trusted agent. Set `CONTROLPLANE_DASHBOARD_TRUSTED_AGENT_ID` to that enrolled agent and configure the same node with `AGENT_MANAGED_DASHBOARD_SECRETS_DIR=/absolute/host/path`. The control plane reserves this agent from user workload placement and refuses dashboard placement if another workload is already there.

Create that directory and provision these files through the host secret manager, never through desired-state environment variables:

- `database-url` (use a dashboard-scoped database role, never the control-plane superuser URL)
- `jwt-secret` (a dashboard-only 32+ character secret for browser sessions)
- `user-assertion-secret` (a distinct 32+ byte HMAC secret shared only with the control plane)
- `github-token-encryption-key` (base64url/base64 encoding of exactly 32 random bytes, distinct from both secrets above)
- `github-client-secret` when dashboard GitHub OAuth is enabled

Configure the same assertion secret on the control-plane host with `CONTROLPLANE_USER_ASSERTION_SECRET` or `CONTROLPLANE_USER_ASSERTION_SECRET_FILE`. Do not reuse `jwt-secret`; keeping the keys separate prevents a control-plane credential from becoming a browser-session signing key.

When the managed dashboard is assigned, the trusted agent creates `controlplane-key.pem` locally and submits only a CSR over its authenticated agent connection. The control plane issues a short-lived certificate with the configured dashboard caller identity only to `CONTROLPLANE_DASHBOARD_TRUSTED_AGENT_ID`. The agent writes `controlplane-cert.pem` and `controlplane-ca.pem`, renews the certificate before expiry, and restarts the dashboard allocation after rotation. The private key never leaves the trusted node.

The agent bind-mounts the directory read-only at `/run/secrets/dashboard` only for the managed dashboard workload. The control plane sends file paths, not secret values, in desired state. The dashboard sends a fresh signed user assertion over its authenticated mTLS connection; the raw `x-platform-user-id` header is rejected. Project membership and role authorization remain in the control plane.
