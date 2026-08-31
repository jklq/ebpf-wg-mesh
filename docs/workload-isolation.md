# Workload isolation

Every untrusted workload runs under a production OCI sandbox. Customers cannot
supply privileged, host-network, host-PID, sysctl, device, or bind-mount
configuration. The only customer-visible isolation control is an operator-defined
named sandbox profile.

## Production profile

The default `production` profile is not relaxable. The agent always applies:

- `no_new_privileges` and an empty capability set
- a non-root user when the image would otherwise run as UID 0
- a read-only root filesystem, plus explicit tmpfs at `/tmp`, `/var/tmp`, and `/run`
- writable volume mounts only at the platform volume path
- PID/process limits (`pids.max` and `RLIMIT_NPROC`)
- containerd’s maintained default seccomp profile
- AppArmor (`ebpf-wg-mesh-workload`) when the host supports it, otherwise the
  confined SELinux `container_t` label when SELinux is enabled
- isolated PID, IPC, UTS, mount, network, and cgroup namespaces
- masked `/proc` and `/sys` paths, including `/proc/kcore` and `/sys/fs/bpf`
- no host devices or sockets
- hard cgroup v2 CPU quota, memory max, swap equal to memory (no extra swap),
  `memory.oom.group=1`, and a raised workload OOM score

## Agent and runtime reservation

Each agent advertises capacity after subtracting reserved CPU and memory. The
agent process also:

- sets `oom_score_adj=-500` so tenant pressure is killed first
- writes `memory.min` / `memory.low` on its own cgroup
- places workloads under `ebpf-wg-mesh-workloads` with `memory.max` equal to
  advertised tenant memory

Install the agent unit with `Delegate=yes`, `MemoryMin`, `CPUWeight`, and
`OOMScoreAdjust=-500` so systemd cooperates with that reservation.

## Compatibility profiles

Operators may enable a small set of named profiles. Each profile must include a
visible risk statement and only these relaxations:

- `run-as-root`
- `writable-rootfs`

Configure them on the control plane:

```bash
CONTROLPLANE_SANDBOX_COMPATIBILITY_PROFILES_JSON='[
  {
    "name": "legacy-root",
    "risk": "The image entrypoint must run as UID 0.",
    "relaxations": ["run-as-root"]
  }
]'
```

Clients select a profile by name. The control plane replaces risk and
relaxations from operator configuration before persisting the spec. Selecting or
changing a relaxed profile writes an audit row that survives service deletion.

The console shows the resolved profile, the risk statement, and recent audit
history. It never accepts arbitrary OCI input.

## Local stack

`localteststack` uses Docker rather than containerd. The Docker runtime still
applies the production sandbox: read-only root, dropped capabilities,
`no-new-privileges`, PID limits, non-root user, tmpfs, and equal memory/swap
caps. It does not pass `--privileged`, `--network=host`, `--pid=host`, `--device`,
or `--sysctl`.
