# ebpf-wg-mesh

Minimal PaaS control plane and agent prototype with a retained WireGuard/eBPF private fabric.

## Current shape

- `cmd/controlplane`: authoritative control plane
- `cmd/agent`: node agent that opens an mTLS gRPC stream to the control plane
- `dashboard`: minimal TanStack Start dashboard app that owns browser auth/session state and calls the control plane over internal mTLS gRPC
- `internal/controlplane`: CockroachDB store, internal gRPC authz/authn, managed dashboard reconciliation, agent stream handling, Caddy sync
- `internal/agent`: desired-state loop, local reconcile runtime, containerd inspection, status reporting
- `internal/mesh`: retained mesh bootstrap that wraps the existing WireGuard and eBPF implementation
- `api/proto`: protobuf definitions and generated gRPC bindings

## Architecture

- Single control plane only.
- CockroachDB stores authz-side users, projects, memberships, agents, services, revisions, volumes, domains, allocations, and status projections.
- The dashboard app uses its own schema in the same CockroachDB cluster for app users, sessions, accounts, and onboarding metadata.
- Agents are intentionally dumb: they receive full per-node desired-state snapshots and reconcile local state.
- Public ingress is centralized through one Caddy instance; the control plane replaces Caddy config through the admin API.
- The dashboard is the only intended product-facing caller of `platform.v1.PlatformService`.
- Agent-facing and dashboard-facing internal gRPC are protected by mTLS with distinct caller identities.
- The existing WireGuard/eBPF code remains the private node-to-node transport/policy layer behind `internal/mesh`.

## Bootstrap

The binaries no longer require YAML config files.

- `controlplane` bootstraps from flags and environment, then owns node mesh/workload assignment in CockroachDB.
- `agent` bootstraps from flags and environment, discovers local host facts, persists its own WireGuard private key, enrolls, and waits for assigned node config from the control plane.

Common bootstrap inputs:

- control plane: listen addresses, agent bootstrap token(s), DB URL, state dir, ingress admin URL, managed dashboard service settings
- agent: control-plane address, control-plane CA, bootstrap token, data dir

## Build

Generate protobuf and BPF artifacts as needed:

```bash
go generate ./api/proto
go generate ./internal/firewall
```

Run tests:

```bash
go test ./...
```

## Internal mTLS

- The control plane auto-creates an internal CA and gRPC server certificate under `CONTROLPLANE_STATE_DIR/pki`.
- Agents no longer need pre-generated client certificates. Each agent generates its own key in `AGENT_DATA_DIR/tls`, enrolls with `AGENT_BOOTSTRAP_TOKEN`, receives a short-lived mTLS certificate, and renews it automatically before expiry.
- Agents still need the control-plane CA certificate for the initial TLS trust root. In the devstack this is shared from the control-plane data volume; on separate VPSes, copy the public `ca.crt` once.

## Notes

- The control plane, scheduler, auth, desired-state protocol, and ingress sync are implemented.
- The agent runtime currently materializes volume state, persists desired service state, inspects containerd, and reports status. Container launch orchestration is intentionally isolated behind the agent runtime boundary and can be deepened without changing the control-plane protocol.
