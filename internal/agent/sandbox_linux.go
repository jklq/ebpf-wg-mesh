//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/isolation"

	containerdapparmor "github.com/containerd/containerd/v2/contrib/apparmor"
	containerdseccomp "github.com/containerd/containerd/v2/contrib/seccomp"
	"github.com/containerd/containerd/v2/core/containers"
	hostapparmor "github.com/containerd/containerd/v2/pkg/apparmor"
	"github.com/containerd/containerd/v2/pkg/oci"
	hostseccomp "github.com/containerd/containerd/v2/pkg/seccomp"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/selinux/go-selinux"
)

const (
	sandboxProcessLimit  = int64(256)
	sandboxOOMScoreAdj   = 500
	workloadAppArmorName = "ebpf-wg-mesh-workload"
)

var (
	sandboxMaskedPaths = []string{
		"/proc/kcore", "/sys/fs/bpf", "/sys/kernel/security",
	}
	sandboxReadonlyPaths = []string{
		"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger",
	}
	sandboxCapabilities = []string{
		"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_FSETID", "CAP_SETGID",
		"CAP_SETUID", "CAP_SETPCAP", "CAP_NET_BIND_SERVICE", "CAP_KILL",
	}
)

type sandboxBindMount struct {
	source   string
	writable bool
}

func workloadSandboxOpts(allowedBindMounts map[string]sandboxBindMount, cgroupPath string, policy isolation.Policy, uidMaps, gidMaps []specs.LinuxIDMapping) []oci.SpecOpts {
	pidsLimit := sandboxProcessLimit
	if policy.Class() == isolation.ClassGVisor {
		// The Sentry and gofer share the workload cgroup and spend host
		// threads per app thread; the in-sandbox RLIMIT_NPROC stays exact.
		pidsLimit += 128
	}
	opts := []oci.SpecOpts{
		withWorkloadSandbox(cgroupPath, allowedBindMounts, uidMaps, gidMaps, pidsLimit),
	}
	// Host seccomp and AppArmor confine the process the runtime starts.
	// Under gVisor that process is the Sentry, which installs its own,
	// far stricter syscall filtering and needs platform syscalls the
	// container profiles deny — applying them would weaken nothing but
	// break (or silently misconfine) the monitor. Every other class runs
	// tenant code more directly, so both profiles stay mandatory there.
	if policy.Class() == isolation.ClassGVisor {
		// Every allocation is its own gVisor sandbox. Without this
		// annotation the shim treats the container as joining an existing
		// sandbox, does not wire container IO into `runsc create`, and
		// its buffered-output wait then blocks until the workload exits.
		return append(opts, oci.WithAnnotations(map[string]string{
			"io.kubernetes.cri.container-type": "sandbox",
		}))
	}
	opts = append([]oci.SpecOpts{containerdseccomp.WithDefaultProfile()}, opts...)
	if hostapparmor.HostSupports() {
		opts = append(opts, containerdapparmor.WithDefaultProfile(workloadAppArmorName))
	} else if selinux.GetEnabled() {
		opts = append(opts, oci.WithSelinuxLabel("system_u:system_r:container_t:s0"), func(_ context.Context, _ oci.Client, _ *containers.Container, spec *specs.Spec) error {
			if spec.Linux != nil {
				spec.Linux.MountLabel = "system_u:object_r:container_file_t:s0"
			}
			return nil
		})
	}
	return opts
}

func withWorkloadSandbox(cgroupPath string, allowedBindMounts map[string]sandboxBindMount, uidMaps, gidMaps []specs.LinuxIDMapping, pidsLimit int64) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, spec *specs.Spec) error {
		if spec.Process == nil || spec.Root == nil || spec.Linux == nil {
			return errors.New("sandbox requires a complete Linux OCI spec")
		}
		spec.Process.NoNewPrivileges = true
		spec.Process.Capabilities = workloadCapabilities()
		spec.Process.User.AdditionalGids = nil
		spec.Root.Readonly = false
		spec.Process.OOMScoreAdj = intPtr(sandboxOOMScoreAdj)
		spec.Process.Rlimits = upsertRlimit(spec.Process.Rlimits, specs.POSIXRlimit{
			Type: "RLIMIT_NPROC", Soft: uint64(sandboxProcessLimit), Hard: uint64(sandboxProcessLimit),
		})

		if spec.Linux.Resources == nil {
			spec.Linux.Resources = &specs.LinuxResources{}
		}
		spec.Linux.Resources.Pids = &specs.LinuxPids{Limit: &pidsLimit}
		spec.Linux.Resources.Devices = sandboxDeviceRules()
		spec.Linux.Devices = nil
		spec.Linux.Sysctl = nil
		spec.Linux.RootfsPropagation = "rprivate"
		spec.Linux.MaskedPaths = append([]string(nil), sandboxMaskedPaths...)
		spec.Linux.ReadonlyPaths = append([]string(nil), sandboxReadonlyPaths...)
		// Identity mappings are replaced, never merged: an image-supplied
		// mapping must not widen or shift the scope's host range. Empty
		// mappings mean a kernel-separated class, where no user namespace
		// is created at all.
		spec.Linux.UIDMappings = append([]specs.LinuxIDMapping(nil), uidMaps...)
		spec.Linux.GIDMappings = append([]specs.LinuxIDMapping(nil), gidMaps...)
		spec.Linux.Namespaces = isolatedNamespaces(spec.Linux.Namespaces, len(uidMaps) > 0)
		if cgroupPath != "" {
			spec.Linux.CgroupsPath = cgroupPath
		}
		mounts, err := hardenedSandboxMounts(spec.Mounts, allowedBindMounts)
		if err != nil {
			return err
		}
		// Bind mounts get no per-mount ID mappings: the user namespace
		// translation already presents host-mapped inodes at their
		// container IDs, and a mount mapping would shift them twice.
		// Host inodes behind binds are owned by host-mapped IDs (volumes)
		// or world-readable (identity files).
		for i := range mounts {
			mounts[i].UIDMappings = nil
			mounts[i].GIDMappings = nil
			// A fresh sysfs mount is refused inside a user namespace whose
			// network namespace is owned by the host (ours is: CNI owns it).
			// Bind the host sysfs read-only instead, as rootless runtimes do;
			// masked paths still apply on top, and sysfs carries no ID
			// mapping (kernfs files are world-readable, never written).
			if len(uidMaps) > 0 && mounts[i].Type == "sysfs" && filepath.Clean(mounts[i].Destination) == "/sys" {
				mounts[i].Type = "bind"
				mounts[i].Source = "/sys"
				mounts[i].Options = []string{"rbind", "ro", "nosuid", "nodev", "noexec"}
			}
		}
		spec.Mounts = mounts
		return nil
	}
}

func workloadCapabilities() *specs.LinuxCapabilities {
	return &specs.LinuxCapabilities{
		Bounding:  append([]string(nil), sandboxCapabilities...),
		Effective: append([]string(nil), sandboxCapabilities...),
		Permitted: append([]string(nil), sandboxCapabilities...),
	}
}

func isolatedNamespaces(existing []specs.LinuxNamespace, userNamespace bool) []specs.LinuxNamespace {
	required := []specs.LinuxNamespaceType{
		specs.PIDNamespace, specs.IPCNamespace, specs.UTSNamespace, specs.MountNamespace,
		specs.NetworkNamespace, specs.CgroupNamespace,
	}
	if userNamespace {
		required = append(required, specs.UserNamespace)
	}
	out := make([]specs.LinuxNamespace, 0, len(required))
	for _, namespaceType := range required {
		next := specs.LinuxNamespace{Type: namespaceType}
		for _, namespace := range existing {
			if namespaceType == specs.NetworkNamespace && namespace.Type == namespaceType {
				next.Path = namespace.Path
				break
			}
		}
		out = append(out, next)
	}
	return out
}

func hardenedSandboxMounts(existing []specs.Mount, allowedBindMounts map[string]sandboxBindMount) ([]specs.Mount, error) {
	tmpfs := map[string]specs.Mount{
		"/tmp":     {Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=64m"}},
		"/var/tmp": {Destination: "/var/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=64m"}},
		"/run":     {Destination: "/run", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "noexec", "nodev", "mode=0755", "size=16m"}},
	}
	out := make([]specs.Mount, 0, len(existing)+len(tmpfs))
	for _, mount := range existing {
		destination := filepath.Clean(mount.Destination)
		if replacement, ok := tmpfs[destination]; ok {
			out = append(out, replacement)
			delete(tmpfs, destination)
			continue
		}
		if mount.Type == "bind" {
			allowed, ok := allowedBindMounts[destination]
			if !ok || filepath.Clean(mount.Source) != filepath.Clean(allowed.source) {
				return nil, fmt.Errorf("sandbox rejects bind mount %q from %q", destination, mount.Source)
			}
			mount.Source = filepath.Clean(allowed.source)
			mount.Options = hardenedBindMountOptions(mount.Options, allowed.writable)
		}
		out = append(out, mount)
	}
	for _, destination := range []string{"/tmp", "/var/tmp", "/run"} {
		if mount, ok := tmpfs[destination]; ok {
			out = append(out, mount)
		}
	}
	return out, nil
}

func hardenedBindMountOptions(options []string, writable bool) []string {
	out := make([]string, 0, len(options)+4)
	for _, option := range options {
		switch strings.ToLower(option) {
		case "bind", "rbind", "rw", "ro", "suid", "nosuid", "dev", "nodev":
			continue
		default:
			out = append(out, option)
		}
	}
	mode := "ro"
	if writable {
		mode = "rw"
	}
	return ensureMountOptions(out, "rbind", mode, "nosuid", "nodev")
}

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

func ensureMountOptions(options []string, required ...string) []string {
	out := append([]string(nil), options...)
	for _, option := range required {
		if !slices.Contains(out, option) {
			out = append(out, option)
		}
	}
	return out
}

func upsertRlimit(existing []specs.POSIXRlimit, limit specs.POSIXRlimit) []specs.POSIXRlimit {
	out := append([]specs.POSIXRlimit(nil), existing...)
	for i := range out {
		if out[i].Type == limit.Type {
			out[i] = limit
			return out
		}
	}
	return append(out, limit)
}

func intPtr(value int) *int { return &value }

func prepareWorkloadSandboxHost(cfg config.AgentConfig, policy isolation.Policy) (string, error) {
	if err := policy.VerifyHost(); err != nil {
		return "", err
	}
	if hostapparmor.HostSupports() {
		if err := containerdapparmor.LoadDefaultProfile(workloadAppArmorName); err != nil && cfg.Profile.IsProduction() {
			return "", fmt.Errorf("load production AppArmor profile: %w", err)
		}
	}
	parent, err := protectAgentRuntime(cfg)
	if err != nil {
		return "", err
	}
	if !cfg.Profile.IsProduction() {
		return parent, nil
	}
	if !hostseccomp.IsEnabled() {
		return "", errors.New("production sandbox requires kernel seccomp support")
	}
	controllers, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return "", fmt.Errorf("production sandbox requires cgroup v2: %w", err)
	}
	for _, required := range []string{"cpu", "memory", "pids"} {
		if !slices.Contains(strings.Fields(string(controllers)), required) {
			return "", fmt.Errorf("production sandbox requires cgroup v2 %s controller", required)
		}
	}
	return parent, nil
}
