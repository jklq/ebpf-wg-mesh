# Stage 5 — Networking and Ingress

The platform’s WireGuard/eBPF network is a differentiator. This stage completes that design instead of replacing it with a generic cluster network.

## 5.1 Add a dual-stack workload overlay

Prompt:

```text
Add IPv4 as a second overlay family next to the existing IPv6 mesh so every allocated workload gets both addresses, both are routed in WireGuard AllowedIPs, and both are enforced by the eBPF identity policy under the same network_identity. Allocate IPv4 from a real sequential pool and non-overlapping per-node prefixes—do not hash it the IPv6 way—and reject pool exhaustion or overlap transactionally. Leave underlay advertise_addr on IPv6. Extend desired state, allocation reports, container labels, CNI setup, internal host entries, service DNS, health probes, metrics labels, and Caddy backends to understand both addresses and choose a reachable healthy family without weakening environment isolation. A process binding only 0.0.0.0 must become healthy and publicly reachable, and a process binding only :: must continue to work. Isolation tests must prove same-environment allow, cross-environment deny, unknown-destination deny, identity removal, and node failover on both families.
```

## 5.2 Make ingress highly available and convergent

Prompt:

```text
Replace the single mutable Caddy target assumption with a small fleet of interchangeable ingress instances managed through an IngressProvider contract. Keep Caddy as the first implementation. Each instance must receive a versioned complete routing snapshot derived from CockroachDB, acknowledge the applied revision, retain the last known-good configuration on rejection, and converge after restart or network partition. Control-plane replicas may race to reconcile but must produce the same canonical configuration and must not partially publish a rollout. Readiness should require enough ingress instances at the current revision to satisfy the configured availability policy. Route only ready, non-draining allocations; support multiple replicas; and remove a backend before destructive shutdown. Provide per-instance status, config diff diagnostics without secrets, staged validation, rollback to last known good, and failure tests for one ingress down, invalid config, delayed apply, and split control-plane ownership.
```

## 5.3 Harden domain and certificate lifecycle

Prompt:

```text
Turn domain bindings into an explicit verification and certificate lifecycle. Persist requested, verification-pending, verified, certificate-pending, active, degraded, and removing states with safe reason codes and timestamps. Prevent hostname takeover by proving DNS ownership according to the domain type, recheck ownership periodically, and avoid serving a new customer’s workload on a hostname retained from a deleted project. Integrate certificate issuance through Caddy or a narrow ACME provider contract with rate-limit awareness, renewal monitoring, challenge cleanup, and last-known-good certificate behavior. Show exact DNS records, observed values, certificate expiry, and actionable errors in the console. Generated domains must be collision-resistant and reserved transactionally. Test conflicting claims, dangling CNAMEs, rebinding after deletion grace, issuance failure, renewal failure, wildcard restrictions, and certificate expiry alerts.
```

## 5.4 Support explicit HTTP and TCP exposure

Prompt:

```text
Model public endpoints separately from container ports. HTTP endpoints need hostname, target port, TLS policy, request-size and timeout limits, optional WebSocket support, and trusted proxy/header behavior. TCP endpoints need an allocated public port or hostname/SNI routing strategy supported by the configured ingress provider, target port, idle timeout, and connection limits. Reject exposure of undeclared or unhealthy ports and ensure private-only services remain unreachable publicly. Record enough ingress metrics in VictoriaMetrics to show request count, response class, latency, active connections, bytes, and rejected traffic without storing sensitive paths by default. Add console flows that explain the security and billing effect of exposure, plus end-to-end tests for HTTP, WebSocket, long-lived TCP, TLS, replica balancing, drain behavior, and cross-project isolation.
```

## 5.5 Add egress policy and network abuse controls

Prompt:

```text
Add per-environment outbound policy enforced close to workloads. The default product policy may allow internet egress, but operators and authorized project roles must be able to deny all external egress, allow selected CIDRs and ports, or require traffic through an operator-provided egress gateway. Private same-environment traffic continues to use workload identities and must not be accidentally governed as public egress. Protect platform metadata addresses, host networks, control-plane/agent administration, registry credentials, and other tenant overlays regardless of customer rules. Add connection and bandwidth accounting to VictoriaMetrics, configurable limits, SMTP and common abuse-sensitive port policy, and visible denial diagnostics that do not leak destination data across tenants. Verify IPv4 and IPv6 enforcement, DNS behavior, rule updates, fail-closed agent restart, and bypass attempts through mapped or tunneled addresses.
```

## 5.6 Integrate upstream edge protection without building an edge network

Prompt:

```text
Define an optional EdgeProvider integration for deployments that need CDN, WAF, DDoS protection, under-attack mode, or geographically distributed TLS entry. The platform should continue to own domains, origin routing, health, authorization, and product state while delegating global edge capabilities to an operator-selected provider such as Cloudflare. Provision and reconcile provider resources idempotently from domain settings, use scoped credentials, validate origin authentication, surface provider status and limits, and tear resources down only after the deletion grace period. Keep a direct-ingress mode for installations that do not configure an edge provider. Do not implement BGP, a CDN cache, or a WAF engine in this repository. Test provider outage, stale credentials, manual provider drift, origin bypass prevention, and safe disablement.
```
