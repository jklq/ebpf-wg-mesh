package builder

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/containers"
	containerdseccomp "github.com/containerd/containerd/contrib/seccomp"
	"github.com/containerd/containerd/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// sandboxSeccompProfile returns the default syscall filter for build
// sandboxes. It must run after the capability-stripping lockdown:
// the default profile denies namespace manipulation only when the
// bounding set lacks CAP_SYS_ADMIN.
func sandboxSeccompProfile() oci.SpecOpts {
	return containerdseccomp.WithDefaultProfile()
}

// sandboxHostname is the UTS hostname inside every build sandbox.
const sandboxHostname = "build"

// sandboxRunSize caps /run, which carries only runtime state.
const sandboxRunSize = "16m"

// sandboxTmpfsSize derives the /tmp and /var/tmp cap from the
// execution disk limit so tmpfs writes are contained by the same disk
// budget as the workspace (post-hoc accounting) and the overlay
// filesystem (usage polling). Without this a hostile build could fill
// tmpfs beyond the execution's disk budget with none of the three
// accounting paths noticing.
func sandboxTmpfsSize(limits ResourceLimits) string {
	size := limits.MaxWorkspaceBytes / 8
	if size < 16<<20 {
		size = 16 << 20
	}
	if size > 256<<20 {
		size = 256 << 20
	}
	return fmt.Sprintf("%dm", size>>20)
}

// sandboxSpecInput carries everything needed to build one sandbox OCI
// spec. Image configuration (entrypoint, working dir) is deliberately
// not inherited: the sandbox runs an explicit argv with an explicit
// environment and working directory.
type sandboxSpecInput struct {
	Argv      []string
	Env       []string
	Dir       string
	NetNSPath string
	Limits    ResourceLimits
	Mounts    []SandboxMount
}

// buildSandboxSpecOpts returns the OCI spec options for one build
// step. The sandbox runs with:
//
//   - fresh mount, PID, IPC, UTS, and cgroup namespaces plus the
//     execution's private network namespace (never the host's);
//   - zero Linux capabilities, no-new-privileges, and a read-only
//     container root filesystem;
//   - only the execution workspace (read-only snapshot nested inside
//     a writable root), an optional content-keyed cache dir, and the
//     rendered resolver files from the host — no host sockets,
//     devices, or credentials;
//   - a hard resident-set cap and PID cap from the execution limits,
//     plus kernel-enforced CPU, file-size, and process-count rlimits;
//   - the containerd default seccomp profile.
//
// Capability stripping runs before the seccomp option on purpose: the
// default profile denies namespace manipulation (unshare, setns,
// namespaced clone, clone3) only when the bounding set lacks
// CAP_SYS_ADMIN.
func buildSandboxSpecOpts(input sandboxSpecInput) ([]oci.SpecOpts, error) {
	if len(input.Argv) == 0 {
		return nil, errors.New("sandbox step requires a command")
	}
	if strings.TrimSpace(input.NetNSPath) == "" {
		return nil, errors.New("sandbox requires a private network namespace: host networking is forbidden")
	}
	if err := input.Limits.Validate(); err != nil {
		return nil, fmt.Errorf("sandbox limits: %w", err)
	}
	if err := validateSandboxMounts(input.Mounts); err != nil {
		return nil, err
	}
	for _, entry := range input.Env {
		if !strings.Contains(entry, "=") {
			return nil, fmt.Errorf("sandbox env entry %q must be NAME=value", entry)
		}
	}
	mounts, err := sandboxOCIMounts(input.Mounts, input.Limits)
	if err != nil {
		return nil, err
	}
	dir := strings.TrimSpace(input.Dir)
	if dir == "" {
		dir = sandboxBuildRoot
	}
	return []oci.SpecOpts{
		oci.WithDefaultSpec(),
		oci.WithProcessArgs(input.Argv...),
		withEmptyEnv(),
		oci.WithEnv(input.Env),
		oci.WithProcessCwd(dir),
		oci.WithHostname(sandboxHostname),
		oci.WithMounts(mounts),
		oci.WithLinuxNamespace(specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: input.NetNSPath}),
		withBuildSandboxLimits(input.Limits),
		withBuildSandboxLockdown(),
		sandboxSeccompProfile(),
	}, nil
}

// withEmptyEnv clears the default environment so the step's explicit
// environment is the sandbox's whole environment. oci.WithEnv merges
// into whatever is already there; without this the image default
// PATH would survive alongside the step's variables.
func withEmptyEnv() oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, spec *specs.Spec) error {
		if spec.Process == nil {
			return errors.New("sandbox env requires a process")
		}
		spec.Process.Env = nil
		return nil
	}
}

// sandboxOCIMounts converts validated sandbox mounts to OCI mounts.
// The workspace root mount sorts first so nested read-only mounts
// (the snapshot) apply over it. Tmpfs mounts for /tmp, /var/tmp, and
// /run are appended with size caps.
func sandboxOCIMounts(mounts []SandboxMount, limits ResourceLimits) ([]specs.Mount, error) {
	out := make([]specs.Mount, 0, len(mounts)+3)
	// The build root must be mounted before anything nested beneath
	// it, so emit it first regardless of input order.
	for _, pass := range []bool{true, false} {
		for _, mount := range mounts {
			isRoot := filepath.Clean(mount.Dest) == sandboxBuildRoot
			if isRoot != pass {
				continue
			}
			out = append(out, specs.Mount{
				Source:      filepath.Clean(mount.Source),
				Destination: filepath.Clean(mount.Dest),
				Type:        "bind",
				Options:     sandboxBindOptions(mount.ReadOnly),
			})
		}
	}
	tmpSize := sandboxTmpfsSize(limits)
	out = append(out,
		specs.Mount{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=" + tmpSize}},
		specs.Mount{Destination: "/var/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=" + tmpSize}},
		specs.Mount{Destination: "/run", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "noexec", "nodev", "mode=0755", "size=" + sandboxRunSize}},
	)
	return out, nil
}

// sandboxBindOptions forces the containment options for a workspace
// bind: recursive bind, read-only or read-write as directed, never
// setuid, never devices. Execution is allowed on writable workspace
// mounts (build tools stage executable helpers there) but the
// read-only snapshot additionally forbids it.
func sandboxBindOptions(readOnly bool) []string {
	options := []string{"rbind", "nosuid", "nodev"}
	if readOnly {
		options = append(options, "ro", "noexec")
	} else {
		options = append(options, "rw")
	}
	return options
}

// withBuildSandboxLimits applies the execution resource limits: a hard
// cgroup resident-set cap (replacing 2.4a's virtual-address bound),
// swap capped to the same value with the OOM killer grouping the
// whole sandbox, a PID cap, and kernel rlimits for CPU time, file
// size, and process count. RLIMIT_AS is deliberately unset: virtual
// reservation headroom is not a containment boundary, resident set is.
func withBuildSandboxLimits(limits ResourceLimits) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, spec *specs.Spec) error {
		if spec.Process == nil || spec.Linux == nil {
			return errors.New("sandbox limits require a complete Linux OCI spec")
		}
		if spec.Linux.Resources == nil {
			spec.Linux.Resources = &specs.LinuxResources{}
		}
		memory := limits.MemoryBytes
		swap := limits.MemoryBytes
		disableOOMKiller := false
		spec.Linux.Resources.Memory = &specs.LinuxMemory{
			Limit:            &memory,
			Swap:             &swap,
			DisableOOMKiller: &disableOOMKiller,
		}
		if spec.Linux.Resources.Unified == nil {
			spec.Linux.Resources.Unified = make(map[string]string)
		}
		spec.Linux.Resources.Unified["memory.oom.group"] = "1"
		spec.Linux.Resources.Pids = &specs.LinuxPids{Limit: limits.MaxProcesses}
		spec.Process.Rlimits = []specs.POSIXRlimit{
			{Type: "RLIMIT_CPU", Soft: uint64(limits.CPUSeconds), Hard: uint64(limits.CPUSeconds)},
			{Type: "RLIMIT_FSIZE", Soft: uint64(limits.MaxFileBytes), Hard: uint64(limits.MaxFileBytes)},
			{Type: "RLIMIT_NPROC", Soft: uint64(limits.MaxProcesses), Hard: uint64(limits.MaxProcesses)},
		}
		return nil
	}
}

// withBuildSandboxLockdown strips the sandbox to the minimum a build
// client needs: zero capabilities, no-new-privileges, a read-only
// root filesystem, private namespaces, masked and read-only system
// paths, and a deny-all device policy with only the standard
// character devices allowed.
func withBuildSandboxLockdown() oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, spec *specs.Spec) error {
		if spec.Process == nil || spec.Root == nil || spec.Linux == nil {
			return errors.New("sandbox lockdown requires a complete Linux OCI spec")
		}
		spec.Process.NoNewPrivileges = true
		spec.Process.Capabilities = &specs.LinuxCapabilities{}
		spec.Process.User.AdditionalGids = nil
		spec.Root.Readonly = true
		spec.Linux.Resources.Devices = sandboxDeviceRules()
		spec.Linux.Devices = nil
		spec.Linux.Sysctl = nil
		spec.Linux.RootfsPropagation = "rprivate"
		spec.Linux.MaskedPaths = []string{
			"/proc/kcore", "/sys/fs/bpf", "/sys/kernel/security",
		}
		spec.Linux.ReadonlyPaths = []string{
			"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger",
		}
		spec.Linux.Namespaces = sandboxNamespaces(spec.Linux.Namespaces)
		return nil
	}
}

// sandboxNamespaces keeps the private network namespace path selected
// by the executor and forces every other namespace to a fresh private
// instance. A sandbox that shares the host mount, PID, IPC, UTS, or
// cgroup namespace is a sandbox in name only.
func sandboxNamespaces(existing []specs.LinuxNamespace) []specs.LinuxNamespace {
	var netPath string
	for _, namespace := range existing {
		if namespace.Type == specs.NetworkNamespace {
			netPath = namespace.Path
		}
	}
	return []specs.LinuxNamespace{
		{Type: specs.PIDNamespace},
		{Type: specs.IPCNamespace},
		{Type: specs.UTSNamespace},
		{Type: specs.MountNamespace},
		{Type: specs.NetworkNamespace, Path: netPath},
		{Type: specs.CgroupNamespace},
	}
}

// sandboxDeviceRules denies every device except the standard
// character devices a build client needs (null, zero, full, random,
// urandom, tty, ptmx, pts). There are no host block devices, no host
// disks, and no /dev/kmsg.
func sandboxDeviceRules() []specs.LinuxDeviceCgroup {
	device := func(deviceType string, major int64, minor *int64) specs.LinuxDeviceCgroup {
		return specs.LinuxDeviceCgroup{Allow: true, Type: deviceType, Major: &major, Minor: minor, Access: "rwm"}
	}
	minor := func(value int64) *int64 { return &value }
	return []specs.LinuxDeviceCgroup{
		{Allow: false, Access: "rwm"},
		device("c", 1, minor(3)),
		device("c", 1, minor(5)),
		device("c", 1, minor(7)),
		device("c", 1, minor(8)),
		device("c", 1, minor(9)),
		device("c", 5, minor(0)),
		device("c", 5, minor(2)),
		device("c", 136, nil),
	}
}
