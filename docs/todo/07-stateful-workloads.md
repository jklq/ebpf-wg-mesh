# Stage 7 — Stateful Workloads

Do not advertise production databases before every task in this stage and the recovery drills in Stage 9 are complete.

## 7.1 Replace node-local directories with a volume provider contract

Prompt:

```text
Replace the production volume implementation based on agent-local directories with a VolumeProvider contract for operator-selected durable block or network storage. Keep local directories only as an explicitly non-durable development provider. A volume has provider identity, region/failure domain, requested and actual size, filesystem, mount options, lifecycle state, attachment generation, and exactly one writer unless a provider explicitly supports another mode. The control plane owns attachment intent and fences it with a monotonically increasing token; agents attach, format only once, mount at the configured absolute path, report observed identity, and refuse stale attachment generations. Scheduling must honor volume locality and provider availability. Never call RemoveAll on production volume data. Add provider conformance tests and simulate attach timeout, stale attachment, node loss, remount, provider outage, and accidental format attempts.
```

## 7.2 Add safe volume attachment, mount paths, and resize

Prompt:

```text
Make volume attachment a first-class service setting rather than a name that implies a fixed /data mount. Support an explicit absolute mount path, read/write mode, and one or more attachments only if the runtime and provider invariants allow it. Validate collisions with image paths and platform secret mounts, reject unsafe root or system paths, and show attachment effects as staged changes. Add grow-only online resize where the provider/filesystem supports it and a durable offline workflow otherwise; never report the new capacity until both provider and filesystem agree. Prevent detach or delete while an allocation may still write, drain the workload before ownership transfer, and use attachment fencing after node loss. Test mount persistence across deploys, rename, environment duplication policy, resize interruption, stale agent reports, and rollback to specs with different attachments.
```

## 7.3 Add snapshots, scheduled backups, and restores

Prompt:

```text
Implement manual and scheduled volume backups through the configured VolumeProvider or BackupProvider, with daily/weekly/monthly policies, retention, encryption, progress, failure state, and cost/size metadata. Define crash-consistent behavior honestly; optionally support pre/post hooks for application-coordinated backups without claiming consistency when hooks fail. A restore must create a new volume from the chosen backup, verify provider completion and size, stage an attachment change, and preserve the previous volume until the user explicitly deletes it after a grace period. Never restore destructively in place by default. Backup and restore actions must be authorized, audited, monitored for freshness, and safe under retry. Add restoration verification that mounts the recovered volume in isolation and checks a supplied sentinel, plus tests for partial snapshots, retention locks, schedule races, provider outage, and restore cancellation.
```

## 7.4 Make stateful failover explicit and fenced

Prompt:

```text
Implement controlled failover for a single-writer volume-backed service. When its agent is lost, mark the allocation unavailable and wait for the configured node-loss and storage-detach safety conditions. Prove or request provider-side fencing of the old attachment before assigning a new attachment generation and starting the service elsewhere; if fencing cannot be established, remain unavailable rather than risk two writers. Scheduling must respect region and failure-domain compatibility, and the console must distinguish waiting for node recovery, detaching, attaching, mounting, recovering, and healthy. Provide an operator override only when the data-loss/split-brain risk is explicit, typed, and audited. Test old-node return after failover, delayed detach, control-plane failover mid-operation, stale agent desired state, provider ambiguity, and recovery from each intermediate state.
```

## 7.5 Add storage monitoring and customer safety rails

Prompt:

```text
Collect volume provisioned bytes, used bytes, inode use where available, read/write bytes, IOPS, latency, attachment errors, backup age, and restore state into VictoriaMetrics using stable volume identity. Add warning and critical capacity monitors with projected exhaustion, and surface them on service and volume views. Prevent shrink operations, warn before configurations that place ephemeral database paths outside the mounted volume, and make common database image mount paths easy to choose without hard-coding database products into the control plane. Production volume deletion requires no attachment, a recent successful backup or an explicit high-risk override according to policy, typed confirmation, grace period, and audit event. Bill provisioned storage and backup storage from authoritative provider/lifecycle records reconciled with VictoriaMetrics, not from mutable console values.
```

## 7.6 Offer database templates only as transparent services

Prompt:

```text
After durable volumes are production-ready, add maintained templates for selected databases as ordinary container-image services plus volumes, generated credentials, health checks, resource defaults, and documented upgrade/backup paths. Keep the abstraction honest: users should see the image version, mount, variables, network endpoints, resource limits, backup status, and operational ownership. Pin images by digest, stage version upgrades, run compatibility prechecks where practical, and require a recent backup before major upgrades. Do not build a database control plane, automatic clustering system, or pretend a single container is highly available. Templates must be versioned, testable in the VM harness, removable without special scheduler branches, and accompanied by restore and upgrade exercises.
```
