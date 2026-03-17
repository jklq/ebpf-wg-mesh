# ebpf-wg-mesh

Minimal PaaS control plane and agent prototype with a retained WireGuard/eBPF private fabric.

## Current shape

- `cmd/controlplane`: authoritative control plane
- `cmd/agent`: node agent that opens an mTLS gRPC stream to the control plane
- `internal/controlplane`: CockroachDB store, scheduling, OIDC auth, public HTTP API, agent stream handling, Caddy sync
- `internal/agent`: desired-state loop, local reconcile runtime, containerd inspection, status reporting
- `internal/mesh`: retained mesh bootstrap that wraps the existing WireGuard and eBPF implementation
- `api/proto`: protobuf definitions and generated gRPC bindings
- `testbed`: devstack assets for one control plane, one Caddy sidecar, and two agents

## Architecture

- Single control plane only.
- CockroachDB stores users, projects, memberships, agents, services, revisions, volumes, domains, allocations, and status projections.
- Agents are intentionally dumb: they receive full per-node desired-state snapshots and reconcile local state.
- Public ingress, including the control-plane API, is centralized through one Caddy instance; the control plane replaces Caddy config through the admin API.
- User-facing HTTP is protected by OIDC JWT validation.
- Agent-facing gRPC is protected by mTLS.
- The existing WireGuard/eBPF code remains the private node-to-node transport/policy layer behind `internal/mesh`.

## Bootstrap

The binaries no longer require YAML config files.

- `controlplane` bootstraps from flags and environment, then owns node mesh/workload assignment in CockroachDB.
- `agent` bootstraps from flags and environment, discovers local host facts, persists its own WireGuard private key, enrolls, and waits for assigned node config from the control plane.

Common bootstrap inputs:

- control plane: listen addresses, agent bootstrap token(s), DB URL, state dir, OIDC settings, ingress admin URL
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

## Devstack

Start the stack:

```bash
docker compose -f testbed/docker-compose.yml up --build
```

The stack includes:

- `controlplane`
- `caddy`
- `agent-node1`
- `agent-node2`

## Internal mTLS

- The control plane auto-creates an internal CA and gRPC server certificate under `CONTROLPLANE_STATE_DIR/pki`.
- Agents no longer need pre-generated client certificates. Each agent generates its own key in `AGENT_DATA_DIR/tls`, enrolls with `AGENT_BOOTSTRAP_TOKEN`, receives a short-lived mTLS certificate, and renews it automatically before expiry.
- Agents still need the control-plane CA certificate for the initial TLS trust root. In the devstack this is shared from the control-plane data volume; on separate VPSes, copy the public `ca.crt` once.

## Notes

- The control plane, scheduler, auth, desired-state protocol, and ingress sync are implemented.
- The agent runtime currently materializes volume state, persists desired service state, inspects containerd, and reports status. Container launch orchestration is intentionally isolated behind the agent runtime boundary and can be deepened without changing the control-plane protocol.
