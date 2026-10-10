# ebpf-wg-mesh

A small PaaS. It runs a project's services as isolated workloads on an operator-managed fleet, connected by a WireGuard overlay with eBPF identity policy.

- `cmd/controlplane`: authoritative control plane (CockroachDB, mTLS gRPC, xDS for Envoy ingress, registry auth)
- `cmd/agent`: fleet agent that supervises allocations on one node
- `cmd/builder`: build worker that turns source snapshots into images
- `cmd/platformctl`: installation planner and encrypted, resumable SSH deployment controller
- `console`: TanStack Start web app; owns browser auth and calls the control plane
- `api/proto`: protobuf and generated gRPC bindings

## Docs

- [Production deployment](docs/production-deployment.md): typed installations, provider adapters, placement, availability and flat release cutovers.
- [Recovery](docs/recovery.md): complete independent recovery points, native backups, protected artifacts, monitoring and isolated restores.
- [INGRESS.md](INGRESS.md): authenticated xDS, node retirement, and shared wildcard deployment and renewal.
- [docs/map](docs/map/README.md): architecture map, one block per file. Render it with `go run ./cmd/archmap`.
- [CONTEXT.md](CONTEXT.md): domain vocabulary.
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
make test-integration      # Cockroach-backed
make test-e2e-local        # Playwright against an ephemeral local stack
make test-e2e-vm           # Hetzner VMs; needs HCLOUD_TOKEN
make test-smoke-local      # local QEMU/KVM fleet; run `make localvm-setup` once
```

## Develop

```bash
make dev-ephemeral
```

`localteststack` loads a repo-root `.env`, which can be a 1Password-mounted Environment. Shell variables take precedence. To enable GitHub sign-in and webhooks, set `LOCALTESTSTACK_ENABLE_PUBLIC_TUNNEL=1` and provide `CLOUDFLARE_TUNNEL_TOKEN`, `CLOUDFLARE_HOSTNAME`, and the `CONTROLPLANE_GITHUB_*` and `DASHBOARD_GITHUB_*` keys. Without them the stack runs in local-only mode with dev users.

## Agent restarts and network removal

Agent shutdown closes process handles while workloads keep their network. Startup adopts the existing WireGuard interface, reconciles peers and routes in place, and reopens eBPF maps and TCX links under `/sys/fs/bpf/ebpf-wg-mesh/<interface>`. Per-workload connection tracking and identity policy survive clean shutdown and process crashes. The host must mount bpffs at `/sys/fs/bpf`; pins last until explicit removal or a host reboot ([eBPF object lifecycle](https://ebpf-go.dev/concepts/object-lifecycle/)). Map layout or capacity mismatches fail startup while retaining existing state.

Stop the agent and remove its workloads before explicitly destroying its network:

```bash
agent remove-network -mesh-interface-name wg0
```

Workload exit removes that workload's attachments, policy and connection tracking. Agent session loss affects management availability and placement; existing workload observations remain scoped to their allocation generation across reconnects. Envoy checks backend TCP listeners every two seconds, including idle backends, and excludes failed listeners without panic routing. Before replacing a serving allocation on an unavailable agent, the control plane checks its last healthy listeners directly. A reachable listener, an explicit local mesh-access error, or the absence of a reported listener defers replacement. These control-plane probes require network access to the workload addresses; Envoy probes run independently from the actual ingress path.
