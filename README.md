# ebpf-wg-mesh

Stateful, multi-tenant WireGuard mesh daemon with an eBPF TCX dataplane and containerd-driven container lifecycle orchestration.

## Architecture (current)

The project now follows a project-identity model:

- WireGuard (`wg0`) is provisioned as cryptographic transport only.
- Every peer is configured with `0.0.0.0/0` and `::/0` AllowedIPs.
- Tenant isolation and stateful policy are enforced in eBPF, not in WireGuard AllowedIPs.
- Per-container conntrack is isolated using a map-in-map (`HASH_OF_MAPS` -> per-container `LRU_HASH`).
- Cluster identity is modeled with an `LPM_TRIE` (`IPv6 -> {project_id, host_ip, veth_ifindex}`).
- Host-local container traffic is fast-pathed with `bpf_redirect_peer()` when source/destination are in the same project and on the same host.
- Container lifecycle is driven by containerd `TaskStart` / `TaskExit` events.

## Project layout

- `cmd/meshd`: daemon entrypoint
- `internal/config`: YAML / env config parsing
- `internal/wgmesh`: WireGuard lifecycle via Netlink + wgctrl
- `internal/firewall`: eBPF C program, generated bindings, TCX attach, containerd event orchestration
- `testbed`: docker lab assets

## Build notes

`bpf2go` generation requires Linux headers + clang + libbpf headers.

Generation directive:

```bash
go generate ./internal/firewall
```

If you are on macOS, generate inside a Linux container or Linux host.

## Configuration

Minimal YAML structure:

```yaml
nodeName: node1
host:
  ipv4: 172.30.0.11
containerd:
  socket: /run/containerd/containerd.sock
  namespace: default
  projectLabel: mesh.project_id
  ipv6Label: mesh.ipv6
wireguard:
  interfaceName: wg0
  privateKey: "<base64 private key>"
  listenPort: 51820
  addresses: ["10.44.0.1/24"]
  peers:
    - name: node2
      publicKey: "<peer pubkey>"
      endpoint: "172.30.0.12:51820"
      persistentKeepaliveSeconds: 15
firewall:
  conntrackInnerEntries: 10000
  maxContainers: 1024
  clusterIdentityEntries: 65536
```

### Container metadata contract

On `TaskStart`, the daemon resolves container metadata from labels:

- `mesh.project_id` (uint32)
- `mesh.ipv6` (IPv6 string)

(Labels are configurable under `containerd.*Label`.)

## Runtime behavior

1. Set up `wg0` with transport-only peers.
2. Load eBPF maps/programs and attach TCX ingress/egress on `wg0`.
3. Subscribe to containerd events.
4. On `TaskStart`:
   - resolve PID -> host veth ifindex via netns traversal,
   - create per-container inner conntrack map,
   - populate container policy + identity trie,
   - attach TCX ingress/egress to the veth.
5. On `TaskExit`: detach links and remove container-scoped state.

## Status

Implemented:

- TCX ingress/egress dataplane with IPv6 project enforcement, anti-spoofing, per-container internet conntrack, local fast-path redirection.
- WireGuard pure transport topology.
- containerd event-driven per-container hook lifecycle.

Not yet implemented in this repo version:

- managed ingress exposure for public internet traffic.
