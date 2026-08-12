# Tasks

## 1. Dual-Stack Overlay

Prompt:

```
Add IPv4 as a second overlay family next to the existing IPv6 mesh so every allocated workload gets both addresses, both are routed in WireGuard AllowedIPs, and both are enforced by the eBPF identity policy under the same network_identity. Allocate IPv4 from a real sequential pool and per-node prefix — do not hash it the IPv6 way — leave underlay advertise_addr on IPv6, and make health probes and Caddy backends dual-stack so a process that only binds 0.0.0.0 can still become healthy and publicly reachable. Treat names and reported allocation IPs as consumers of those two addresses, and make isolation tests prove same-env allow, cross-env deny, and unknown-dest deny on both families.
```
