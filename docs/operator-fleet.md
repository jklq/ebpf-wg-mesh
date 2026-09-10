# Operator fleet lifecycle

Compute nodes are an operator-managed fleet. Customer workloads never supply
region, zone, failure-domain, reservation, or capability labels.

## Lifecycle

| State | Schedulable | Meaning |
| --- | --- | --- |
| `enrolling` | no | Operator created the node and issued a bootstrap token. Waiting for the first healthy hello. |
| `active` | yes, if the heartbeat is fresh | Accepts new placement. |
| `cordoned` | no | Existing allocations keep running. New placement is refused. |
| `draining` | no | Stateless allocations are replaced through the rolling reconciler: a new allocation is created on another active node, ingress switches, then the old process drains. Volume-backed allocations stay fenced until stateful storage handoff exists. Existing allocation identities are never rewritten onto another node. |
| `unavailable` | no | Heartbeat expired. Workloads fail over when capacity and policy allow. A later hello restores the previous maintenance state. |
| `retired` | no | Allocations and attachments must already be gone. Credentials are revoked and the WireGuard/mesh identity is cleared. |

Operators may move `active ↔ cordoned ↔ draining`, return a draining or cordoned node to `active`, and retire an empty `enrolling`, `cordoned`, `draining`, or `unavailable` node. Direct `active → retired` is refused.

## Enrollment

1. Create the node in the console **Fleet** page (or seed it with a bound bootstrap token).
2. Topology (`region`, `zone`, `failure-domain`) and host reservations are operator policy.
3. Copy the one-time bootstrap token onto the agent (`AGENT_BOOTSTRAP_TOKEN` / `-bootstrap-token`).
4. The agent reports observed CPU/memory, software version, and runtime capabilities (`containerd`, `wireguard`, `ebpf-policy`). Those facts are never taken from a service spec.

Configured control-plane tokens use `agent_id=token` and may carry operator policy:

```text
node-a=secret|region=us-east|zone=a|failure-domain=rack-1|reserved-cpu-millis=500|reserved-memory-mebibytes=512
```

Bootstrap users who may manage the fleet are marked `+operator`:

```text
-bootstrap-user ops:ops@example.com:platform+operator
```

## Placement

New placement uses only `active` nodes with a fresh heartbeat. It:

- skips cordoned, draining, unavailable, retired, and reserved (trusted dashboard) nodes
- spreads replicas across failure domains when capacity allows, then across nodes, then colocates
- honors `ServiceSpec.placement_region`
- subtracts operator CPU/memory reservations from schedulable headroom
- writes a concrete pending message on the service when it cannot place every replica (`N of M replicas placed; …`)

Interrupted drains retry when capacity returns, including after a node hello restores an `unavailable` node.

## Retirement

Retirement consumes remaining bootstrap tokens, sets `credential_revoked_at`, appends issued client-certificate serials to the control-plane revocation file, and clears WireGuard keys and workload subnets so peers drop the mesh identity. The retired node cannot hello, enroll, or receive desired state.

## Control-plane journal

The live owner periodically snapshots product state and truncates `cluster_journal` down to the latest 1024 commands. Restarts replay that snapshot plus the retained tail, not the cluster's entire history. Retried commands are looked up in `cluster_journal_receipts`, which compaction does not delete, so an idempotent retry still returns the original receipt after the log row is gone.

## Local stack

`make dev-ephemeral` enrolls `localteststack-agent` in region `local` / failure domain `localteststack` and marks the dev user as a platform operator so the Fleet page is usable.
