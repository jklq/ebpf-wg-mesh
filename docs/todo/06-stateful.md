# 6 — Volumes

Railway-level volumes, nothing fancier to start. A volume is a directory on one node that survives redeploys, restarts, and agent upgrades. It is **not** replicated and **not** backed up. If the node is lost, the volume is lost, and the console says so. Backups (6.3), a durable network/block provider (6.4), and storage monitoring (6.5) are parked in [freeze.md](freeze.md).

Volume-backed services reject overlapping replacement: a redeploy stops the current allocation before starting the next, even with a healthcheck. Replicas cannot be used with volumes.

What exists today: volume rows with a stored size, node-pinned placement, a directory under the agent's volumes dir, and a fixed `/data` mount. The size is not enforced, and the agent `RemoveAll`s any volume directory missing from its desired state. That prune is the one unacceptable behavior: a bad checkpoint or a placement bug deletes customer data.

## 6.1 Basic volumes

Was: 7.1 and 7.2, cut down
Status: open
Size: M
Depends on: none

Prompt:

```text
Make the existing node-local volumes a complete basic feature, at Railway's level and no further. A service may attach at most one volume, at an explicit absolute mount path chosen by the user (default /data), with unsafe root and system paths rejected; attaching, detaching, or changing the path is a staged change like any other configuration. The volume is pinned to the node that hosts it, and a volume-backed service is only ever placed there. Enforce the volume's size on the node (reuse the filesystem-quota or cgroup mechanism from 1.11); a full volume produces ENOSPC for the workload and a visible "volume full" status, not a full host disk. Size can grow and never shrink; the new size takes effect on the next start without data movement. Report used bytes from the agent and show used/size on the service and volume views. Agents must never delete volume data because a volume is missing from desired state: delete data only on an explicit, fenced delete instruction from the control plane after the 1.8 deletion grace. Unknown volume directories are reported to the operator, not removed. If the pinned node is lost or retired, the service shows that its volume is unavailable and why, and does not silently start on another node with an empty volume. The console states plainly that volumes are not backed up. Test data persistence across redeploy, agent restart, and agent upgrade; that a missing desired-state entry does not delete data; size enforcement; grow; mount-path change; and the node-lost status.
```
