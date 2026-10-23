# ebpf-wg-mesh

A small PaaS. It runs a project's services as isolated workloads on an operator-managed fleet, connected by a WireGuard overlay with eBPF identity policy.

- `cmd/controlplane`: authoritative control plane (PostgreSQL, mTLS gRPC, xDS for Envoy ingress, registry auth)
- `cmd/agent`: fleet agent that supervises allocations on one node
- `cmd/builder`: build worker that turns source snapshots into images
- `cmd/platformctl`: installation planner and encrypted, resumable SSH deployment controller
- `cmd/mcp`: MCP server (stdio + streamable HTTP) built on the public API
- `console`: TanStack Start web app; owns browser auth and calls the control plane
- `api/proto`: protobuf and generated gRPC bindings

## Docs

- [INGRESS.md](INGRESS.md): authenticated xDS, node retirement, and shared wildcard deployment and renewal.
- [CONTEXT.md](CONTEXT.md): domain vocabulary.
- [Storage operations](docs/storage-operations.md): snapshots, resident history, and scheduled registry collection.
- [Resource usage](docs/resource-usage.md): CPU, memory, volume, and public egress meters, delivery, retention, and gaps.
- [Observability](docs/observability.md): OpenTelemetry traces, Prometheus metrics, wide-event logs, and the optional bundled Grafana/Tempo/Collector deployment.
- [Public API](docs/public-api.md): token-authenticated REST/JSON API for CI, scripts, and agents.
- [MCP server](docs/mcp-server.md): AI-agent tools over the public API, editor setup, and the Railway mapping.
- [Billing](docs/billing.md): workspace billing accounts, exact rating, statements, spend restrictions, Stripe collection, and the shadow-mode launch gate.
- [Serverless services](docs/serverless.md): scale-to-zero on inactivity, wake on public or private traffic, cold starts, and billing behavior.
- [Tenant isolation](docs/tenant-isolation.md): ID-mapped user namespaces, gVisor, and Kata isolation classes for workloads and builds, host prerequisites, and fail-closed startup.
- [GitHub project](https://github.com/users/jklq/projects/6): backlog and priorities.

## Build

```bash
go generate ./api/proto
go generate ./internal/firewall
bun --cwd=console install   # Node.js is also required: Vite runs on Node
```

## Test

```bash
make test-unit-go
make test-unit-console
make test-integration      # PostgreSQL-backed
make test-e2e-local        # Playwright against an ephemeral local stack
make test-e2e-vm           # Hetzner VMs; needs HCLOUD_TOKEN
make test-smoke-local      # local QEMU/KVM fleet; run `make localvm-setup` once
```

## Develop

```bash
make dev-ephemeral
```

`localteststack` loads a repo-root `.env`, which can be a 1Password-mounted Environment. Shell variables take precedence. To enable GitHub sign-in and webhooks, set `LOCALTESTSTACK_ENABLE_PUBLIC_TUNNEL=1` and provide `CLOUDFLARE_TUNNEL_TOKEN`, `CLOUDFLARE_HOSTNAME`, and the `CONTROLPLANE_GITHUB_*` and `DASHBOARD_GITHUB_*` keys. Without them the stack runs in local-only mode with dev users.
