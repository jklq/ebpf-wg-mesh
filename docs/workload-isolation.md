# Workload isolation

Every untrusted workload runs under a production OCI sandbox. Customers cannot
supply privileged, host-network, host-PID, sysctl, device, or bind-mount
configuration or select an alternate sandbox profile.

## Production sandbox

The production sandbox is designed for ordinary Docker Hub and 12-factor
images. The agent:

- preserves the image `USER`, including UID 0
- leaves the ephemeral overlay root filesystem writable
- keeps only `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `FSETID`, `SETGID`, `SETUID`,
  `SETPCAP`, `NET_BIND_SERVICE`, and `KILL`; administrative, raw-network, BPF,
  tracing, module, time, and device capabilities remain absent
- applies `no_new_privileges`
- mounts convenience tmpfs filesystems at `/tmp`, `/var/tmp`, and `/run`
- permits writable bind mounts only at the platform volume path
- applies PID/process limits (`pids.max` and `RLIMIT_NPROC`)
- applies containerd's maintained default seccomp profile
- applies AppArmor (`ebpf-wg-mesh-workload`) when the host supports it, otherwise
  the confined SELinux `container_t` label when SELinux is enabled; production
  does not fail merely because AppArmor is absent
- isolates PID, IPC, UTS, mount, network, and cgroup namespaces
- masks `/proc/kcore`, `/sys/fs/bpf`, and `/sys/kernel/security`, and mounts
  sensitive proc control paths read-only
- exposes only the conventional private OCI pseudo-devices, never host sockets
  or devices
- enforces hard cgroup v2 CPU quota, memory max, swap equal to memory (no extra
  swap), `memory.oom.group=1`, and a raised workload OOM score

Overlay writes are ephemeral and disappear with the container. Persistent data
belongs on the platform volume mounted at `PLATFORM_VOLUME_DIR`. Each
allocation's ephemeral overlay is capped at 1 GiB once
[docs/todo/01-running-service.md](todo/01-running-service.md#111-bounded-ephemeral-disk)
lands; until then overlay growth can fill the host.

## Agent and runtime reservation

Each agent advertises capacity after subtracting reserved CPU and memory. The
agent process also:

- sets `oom_score_adj=-500` so tenant pressure is killed first
- writes `memory.min` / `memory.low` on its own cgroup
- places workloads under `ebpf-wg-mesh-workloads` with `memory.max` equal to
  advertised tenant memory

Production refuses `--disable-cgroups` and fails closed if the required cgroup
v2 hierarchy, controllers, or reservation cannot be configured. Install the
agent unit with `Delegate=yes`, `MemoryMin`, `CPUWeight`, and
`OOMScoreAdjust=-500` so systemd cooperates with that reservation.

## Local stack

`localteststack` uses Docker rather than containerd. Its default preserves the
image user, writable overlay root, bounded capability set, tmpfs convenience
mounts, `no-new-privileges`, PID limit, raised workload OOM score, and equal
memory/swap caps. It does not pass `--privileged`, `--network=host`,
`--pid=host`, `--device`, `--sysctl`, `--user`, or `--read-only`.
