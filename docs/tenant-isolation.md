# Tenant isolation

Tenant workloads and builds run untrusted code. They never run as host root
on the host kernel: every fleet agent and builder resolves one **isolation
class** (`internal/isolation`) that decides the kernel boundary tenant code
sits behind, verifies at startup that the host can actually provide it, and
fails closed — with the remedy in the error — when it cannot. There is no
configuration value for plain host-root runc, and a configured class never
degrades to a weaker one.

## Classes

| Class | Boundary | Container root is | Host needs |
|---|---|---|---|
| `runc-userns` | runc container in a per-environment ID-mapped user namespace | an unprivileged host UID unique to the environment | kernel user namespaces, overlayfs snapshotter with ID-mapped mounts (`remap-ids`) |
| `gvisor` | gVisor Sentry (userspace kernel) via `io.containerd.runsc.v1` | a Sentry-internal identity; syscalls never reach the host kernel directly | `runsc` + `containerd-shim-runsc-v1` on PATH; builders also `runsc-buildkit` (from this repo) |
| `kata` | Kata lightweight VM via `io.containerd.kata.v2` | guest-kernel root | `kata-runtime` + `containerd-shim-kata-v2`, `/dev/kvm` |

Defaults follow the profile, mirroring the builder executor default:
**production → `gvisor`, development → `runc-userns`**. Production builders
additionally reject `runc-userns`: Dockerfile `RUN` steps execute through the
per-execution BuildKit daemon's OCI worker, which user namespaces alone do
not separate from the host kernel.

On top of the class, the existing sandbox layers still apply in full:
seccomp default profile, AppArmor/SELinux, a stripped capability set,
`NoNewPrivileges`, deny-all device rules, masked `/proc`/`/sys` paths,
private namespaces, cgroup v2 CPU/memory/PID limits, and the eBPF mesh
policy. Isolation classes replace "root in the container is root on the
host"; they do not replace those layers.

## User namespaces (`runc-userns`)

Each environment deterministically owns a 64K-ID host range (a hash of the
environment ID over 4096 slots starting at UID 100000; no state to keep).
Container ID *n* in that environment is host ID *range+n*, for workloads'
processes, image layers, and files alike:

- The OCI spec carries a fresh user namespace with exactly the scope's
  mappings. Image-supplied mappings are replaced, never merged.
- The snapshotter presents shared image layers ID-mapped into the scope's
  range (`WithUserNSRemapperLabels`); layers stay shared, no per-tenant
  copies. Startup requires the daemon to advertise `remap-ids` for the
  configured snapshotter. A fresh sysfs mount is refused in a user
  namespace with a host-owned network namespace, so `/sys` is the host
  sysfs bind-mounted read-only (masked paths still apply on top), as in
  rootless runtimes.
- The volume directory itself is owned by the host identity the container
  user maps to, so ordinary bind mounts present correct ownership through
  the user namespace translation (per-mount ID mappings would shift twice
  and are deliberately not used). Volumes created before this change keep host-root ownership on nested
  files and must be re-owned once to the environment's range (the platform
  has no production volumes yet).
- Builds use one fixed scope (`builder`): build workspaces are ephemeral,
  so the remapped toolchain rootfs is shared across builds, and the
  workspace is handed to the mapped root before each sandbox step
  (`Lchown` walk; tenant symlinks are never followed).

Two environments hash-colliding onto one range would share a host identity
range; with 4096 slots this is a birthday-bound risk only at fleet sizes far
beyond a single host's environment count, and volumes/rootfs remain separate
directory trees regardless.

## gVisor (`gvisor`)

Workloads and build sandbox steps are created with runtime
`io.containerd.runsc.v1` explicitly — never the containerd default runtime.
Every allocation and build step is its own sandbox
(`io.kubernetes.cri.container-type: sandbox` annotation, which the shim
needs to wire container IO into `runsc create`), and the shim is configured
through containerd runtime options (a `runsc.toml` the agent/builder writes
into its data directory) with `network="host"`: the Sentry's netstack mode
does not complete link setup against CNI-provisioned network namespaces on
current kernels (frames arrive, ARP never answers — verified live), while
host-mode sockets still originate inside the workload's own network
namespace, so mesh policy and metering apply unchanged. Host seccomp and
AppArmor profiles are not applied to the Sentry process; it installs its
own syscall filtering, and the container profiles would deny platform
syscalls it requires. The pids cgroup cap carries headroom for Sentry and
gofer threads, which share the workload cgroup; `RLIMIT_NPROC` inside the
sandbox remains the exact app limit.
BuildKit `RUN` steps cannot use `runsc` directly (BuildKit's worker speaks
runc's CLI: `--log`, `--log-format`, `run --keep`), so builders set
`--oci-worker-binary runsc-buildkit`: a small binary from this repo
(`cmd/runsc-buildkit`) that drops the runc-only flags, injects runsc's
`--root` (per-execution, under the daemon root), `--ignore-cgroups`, and
`--network host` (the daemon already runs in the build's network namespace),
then execs `runsc`. Resource limits for `RUN` steps therefore come from the
per-execution daemon placement and BuildKit's own accounting, not from
runsc cgroups.

The Sentry shares the container's memory limit (the shim passes it as
runsc's `--total-memory`), so services under roughly 256 MiB can starve the
Sentry before the workload even starts; treat 256 MiB as the floor for
gVisor-class services (it is also the platform default).

Expect gVisor's usual compatibility surface: some syscalls are unimplemented
or slower (notably heavy `fsync`/network workloads on the default platform).
That is the trade operators accept by selecting the class; `runc-userns`
remains available for agents that need native syscall behavior with the
weaker (shared-kernel, de-privileged) boundary.

## Kata (`kata`)

Workloads and sandboxes use `io.containerd.kata.v2`; BuildKit uses
`kata-runtime` directly as its OCI worker binary (it is a runc-CLI
drop-in). Each container gets a guest kernel, so this is the strongest
boundary, at VM startup/memory cost. Hosts need KVM and a Kata installation
whose hypervisor configuration the operator owns; the platform only
verifies the shim, the runtime binary, and `/dev/kvm` exist. This class is
wired and fail-closed like the others but is not exercised by the repo's
live tests, which cover `runc-userns` and `gvisor`.

## Configuration

Agents:

```bash
--isolation gvisor            # AGENT_ISOLATION
```

Builders (hardened executor):

```bash
--sandbox-isolation gvisor    # BUILDER_SANDBOX_ISOLATION
```

Empty selects the profile default. Unknown values — including `runc` and
raw containerd runtime names — fail validation.

## Host provisioning checklist

Per agent/builder host, for the selected class:

- `runc-userns`: kernel ≥ 5.19 (ID-mapped overlay mounts),
  `/proc/sys/user/max_user_namespaces` > 0, containerd with overlayfs
  snapshotter advertising `remap-ids`
  (`ctr plugins ls` / snapshotter capabilities).
- `gvisor`: `runsc` and `containerd-shim-runsc-v1` from the same gVisor
  release on PATH (checksummed release artifacts; the shim and runsc must
  match), plus `runsc-buildkit` on builder hosts. No containerd
  configuration change is needed: shims are discovered on PATH and the
  runtime is chosen per container.
- `kata`: Kata Containers installed (`kata-runtime`,
  `containerd-shim-kata-v2`), KVM available to the shim.

Startup logs one `tenant isolation policy` / `build isolation policy` line
with the resolved class and runtime, so the effective boundary is visible
in the journal rather than inferred from configuration.

## Verification

- Unit: `internal/isolation` (mapping allocation, fail-closed
  normalization), spec tests in `internal/agent` and `internal/builder`
  (user namespace present, hostile mappings replaced, bind mounts
  ID-mapped), `cmd/runsc-buildkit` argv translation.
- Live (root, containerd host): the agent adversarial suite asserts
  workloads see a non-identity `uid_map`/`gid_map`;
  `TestGVisorWorkloadRunsBehindUserspaceKernel` serves traffic from a
  workload whose kernel release is the Sentry's; the builder adversarial
  suite's `host-root-identity` case does the same `uid_map` check inside a
  build sandbox.
