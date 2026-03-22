# ebpf-wg-mesh

Minimal PaaS control plane and agent prototype with a retained WireGuard/eBPF private fabric.

## Current shape

- `cmd/controlplane`: authoritative control plane
- `cmd/agent`: node agent that opens an mTLS gRPC stream to the control plane
- `console`: minimal TanStack Start app that owns browser auth/session state and calls the control plane over internal mTLS gRPC
- `internal/controlplane`: CockroachDB store, internal gRPC authz/authn, managed dashboard reconciliation, agent stream handling, Caddy sync
- `internal/agent`: desired-state loop, local reconcile runtime, containerd inspection, status reporting
- `internal/mesh`: retained mesh bootstrap that wraps the existing WireGuard and eBPF implementation
- `api/proto`: protobuf definitions and generated gRPC bindings

## Architecture

- Single control plane only.
- CockroachDB stores authz-side users, projects, memberships, agents, services, revisions, volumes, domains, allocations, and status projections.
- The console app uses its own schema in the same CockroachDB cluster for app users, sessions, accounts, and onboarding metadata.
- Agents are intentionally dumb: they receive full per-node desired-state snapshots and reconcile local state.
- Public ingress is centralized through one Caddy instance; the control plane replaces Caddy config through the admin API.
- The console is the only intended product-facing caller of `platform.v1.PlatformService`.
- Agent-facing and console-facing internal gRPC are protected by mTLS with distinct caller identities.
- The existing WireGuard/eBPF code remains the private node-to-node transport/policy layer behind `internal/mesh`.

Architecture decisions:

- Source integration target shape: [docs/adr/0002-source-integration.md](docs/adr/0002-source-integration.md)

## Bootstrap

The binaries no longer require YAML config files.

- `controlplane` bootstraps from flags and environment, then owns node mesh/workload assignment in CockroachDB.
- `agent` bootstraps from flags and environment, discovers local host facts, persists its own WireGuard private key, enrolls, and waits for assigned node config from the control plane.

Common bootstrap inputs:

- control plane: listen addresses, agent bootstrap token(s), DB URL, state dir, ingress admin URL, managed console service settings
- agent: control-plane address, control-plane CA, bootstrap token, data dir

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

Smoke E2E against a local ephemeral stack:

```bash
make test-e2e-local
```

Interactive ephemeral local stack for manual exploration:

```bash
make dev-ephemeral
```

GitHub-enabled interactive devstack via 1Password Environments:

```bash
export OP_ENVIRONMENT_ID=envs/...
export OP_SERVICE_ACCOUNT_TOKEN=ops_... # preferred
# or: export OP_ACCOUNT=my.1password.account
make dev-ephemeral
```

- When `OP_ENVIRONMENT_ID` is unset, `make dev-ephemeral` keeps the existing local-only behavior.
- When the 1Password Environment is readable but incomplete, the stack still starts and logs the missing key names while leaving GitHub disabled.
- Service-account auth takes precedence over desktop-app auth when both `OP_SERVICE_ACCOUNT_TOKEN` and `OP_ACCOUNT` are set.

Required 1Password Environment keys for GitHub-enabled `make dev-ephemeral`:

- Tunnel and hostname: `NGROK_AUTHTOKEN`, `NGROK_DOMAIN`.
- Control plane GitHub and registry: `CONTROLPLANE_GITHUB_APP_ID`, `CONTROLPLANE_GITHUB_WEBHOOK_SECRET`, `CONTROLPLANE_GITHUB_PRIVATE_KEY_PEM` (base64-encoded PEM is recommended if your secret store strips newlines), `CONTROLPLANE_REGISTRY_HOST`, `CONTROLPLANE_REGISTRY_USERNAME`, `CONTROLPLANE_REGISTRY_PASSWORD`.
- Optional control plane overrides: `CONTROLPLANE_GITHUB_API_BASE_URL`, `CONTROLPLANE_GITHUB_WEB_BASE_URL`, `CONTROLPLANE_GITHUB_WEBHOOK_PATH`, `CONTROLPLANE_REGISTRY_NAMESPACE_PREFIX`.
- Optional dashboard install link: `CONTROLPLANE_DASHBOARD_GITHUB_INSTALL_URL`.
- Console GitHub auth: `DASHBOARD_GITHUB_APP_ID`, `DASHBOARD_GITHUB_CLIENT_ID`, `DASHBOARD_GITHUB_CLIENT_SECRET`.
- Optional console overrides: `DASHBOARD_GITHUB_AUTH_BASE_URL`, `DASHBOARD_GITHUB_API_BASE_URL`.

Derived GitHub URLs:

- Callback URL: `{publicBaseURL}/auth/callback`
- Webhook URL: `{publicBaseURL}/webhooks/github` by default, or `{publicBaseURL}{CONTROLPLANE_GITHUB_WEBHOOK_PATH}` when that override is set.

Hostname strategy:

- Default: use the reserved ngrok hostname set in `NGROK_DOMAIN`.
- The stack starts ngrok automatically from `NGROK_AUTHTOKEN` and binds that exact hostname.
- There is no separate `LOCALTESTSTACK_PUBLIC_URL` override path.

Production-replica VM smoke on Hetzner:

```bash
export HCLOUD_TOKEN=...
make test-e2e-vm
```

## Testing Pyramid

- `test-unit-go`: pure Go tests only. Cockroach-backed store coverage is excluded from this tier.
- `test-unit-console`: Vitest unit tests for console session logic, loaders, and React rendering.
- `test-integration`: Cockroach-backed Go tests behind the `integration` build tag plus the console local-stack smoke.
- `test-e2e-local`: thin Playwright smoke against an ephemeral local Cockroach + control plane + console stack.
- `test-e2e-vm`: Hetzner-backed smoke that provisions disposable VMs, deploys pinned binaries, runs remote checks, collects artifacts, and destroys the environment.

## VM Harness

- Infrastructure lives under `infra/test-vm/`.
- `cmd/testvm` builds Linux binaries, provisions the Hetzner topology through OpenTofu, waits for server readiness with `hcloud-go`, deploys the control plane and agents over SSH, runs a thin gRPC smoke scenario, collects host artifacts, and tears everything down.
- The first cut keeps Cockroach colocated with the control plane and provisions two agent VMs.
- Artifacts are written under `artifacts/e2e-vm/<run-id>/`.

## Internal mTLS

- The control plane auto-creates an internal CA and gRPC server certificate under `CONTROLPLANE_STATE_DIR/pki`.
- Agents no longer need pre-generated client certificates. Each agent generates its own key in `AGENT_DATA_DIR/tls`, enrolls with `AGENT_BOOTSTRAP_TOKEN`, receives a short-lived mTLS certificate, and renews it automatically before expiry.
- Agents still need the control-plane CA certificate for the initial TLS trust root. In the devstack this is shared from the control-plane data volume; on separate VPSes, copy the public `ca.crt` once.

## Notes

- The control plane, scheduler, auth, desired-state protocol, and ingress sync are implemented.
- The agent runtime currently materializes volume state, persists desired service state, inspects containerd, and reports status. Container launch orchestration is intentionally isolated behind the agent runtime boundary and can be deepened without changing the control-plane protocol.
