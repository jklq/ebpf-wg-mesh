# ebpf-wg-mesh

Stateful WireGuard mesh daemon written in Go with an eBPF TCX firewall data plane.

## What it does

- Creates and configures a WireGuard interface (`wg0` by default) via Netlink + `wgctrl`.
- Loads TCX eBPF programs on `wg0` ingress + egress.
- Enforces policy:
  - trusted mesh CIDRs: allow (both directions)
  - external egress: allow and record 5-tuple in LRU conntrack map
  - external ingress: allow only if reverse 5-tuple exists, else drop (default deny)
- Streams new conntrack entries through a BPF ring buffer to user space and rebroadcasts over UDP for multi-node state sync.

## Project layout

- `cmd/meshd`: daemon entrypoint
- `internal/config`: YAML / env config parsing
- `internal/wgmesh`: WireGuard lifecycle via Netlink + wgctrl
- `internal/firewall`: eBPF C program, generated bindings, TCX attach, sync runtime
- `testbed`: Docker test lab (3 mesh nodes + external subnet)

## Build and run

`bpf2go` requires Linux headers/tooling. The recommended path is Docker.

1. Build and run the testbed:

```bash
./testbed/scripts/run-integration.sh
```

2. Run daemon directly in Linux:

```bash
meshd -config /path/to/node.yaml
```

## Configuration

Minimal YAML structure:

```yaml
nodeName: node1
wireguard:
  interfaceName: wg0
  privateKey: "<base64 private key>"
  listenPort: 51820
  addresses: ["10.44.0.1/24"]
  peers:
    - name: node2
      publicKey: "<peer pubkey>"
      endpoint: "172.30.0.12:51820"
      allowedIPs: ["10.44.0.2/32"]
      trustCIDRs: ["10.44.0.2/32"]
      persistentKeepaliveSeconds: 15
firewall:
  conntrackEntries: 131072
  trustEntries: 8192
sync:
  enabled: true
  listen: "0.0.0.0:7001"
  authKey: "<base64-or-hex-shared-secret>"
  replayWindowSeconds: 120
  peers: ["172.30.0.12:7002"]
```

Notes:

- `allowedIPs` controls WireGuard cryptokey routing.
- `trustCIDRs` controls firewall mesh trust lookups. Use this to avoid marking routed external subnets as mesh-trusted.

## Integration coverage (Docker testbed)

The test harness validates:

1. Trusted mesh traffic is allowed (`node1 -> node3` ping).
2. External outbound + return traffic is allowed (`node1 -> ext` HTTP).
3. State sync UDP listeners are up on all nodes.
4. Unsolicited external ingress is dropped (default deny).
