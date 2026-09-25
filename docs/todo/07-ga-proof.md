# 7 — Prove paid and stateful GA

Cheap, continuous security hygiene. The rest of the GA-proof work (topology harness 2.13, fault suite 3.17, load limits 3.18, SLOs 7.2, version skew 7.3, DR drills 7.5) is parked in [freeze.md](freeze.md) until paid GA is actually in sight.

Do not treat this file as a reason to delay 1.x or 2.x. Isolation, secret, domain, and sandbox tests belong on those items as they land.

## 7.1 Security checks in CI

Was: 9.5
Status: open
Depends on: none. Not a substitute for adversarial tests on 1.8, 1.2, 2.4b, 2.15, and 2.8.

Prompt:

```text
Add dependency, container-image, Go, TypeScript, protobuf, IaC, and eBPF scanning in CI with a documented triage policy. Paid GA additionally requires an independent penetration test and remediation review; scanners do not replace it. Do not write a fourteen-boundary threat-model encyclopedia in this item.
```
