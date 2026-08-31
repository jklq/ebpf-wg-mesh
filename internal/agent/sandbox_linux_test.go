//go:build linux

package agent

import (
	"context"
	"slices"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"github.com/containerd/containerd/containers"
	containerdseccomp "github.com/containerd/containerd/contrib/seccomp"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestProductionSandboxEnforcesIsolationAfterImageConfiguration(t *testing.T) {
	spec := &specs.Spec{
		Root: &specs.Root{},
		Process: &specs.Process{
			User:         specs.User{UID: 0, GID: 0, AdditionalGids: []uint32{44}},
			Capabilities: &specs.LinuxCapabilities{Effective: []string{"CAP_SYS_ADMIN"}},
		},
		Linux: &specs.Linux{
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.NetworkNamespace, Path: "/run/netns/allocation"},
				{Type: specs.PIDNamespace, Path: "/proc/1/ns/pid"},
			},
			Resources: &specs.LinuxResources{Devices: []specs.LinuxDeviceCgroup{{Allow: true, Type: "c", Access: "rwm"}}},
			Sysctl:    map[string]string{"kernel.hostname": "unsafe"},
			Devices:   []specs.LinuxDevice{{Path: "/dev/kvm"}},
		},
		Mounts: []specs.Mount{
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: defaultVolumeMount, Type: "bind", Source: "/var/lib/platform/volume", Options: []string{"rw"}},
		},
	}
	if err := containerdseccomp.WithDefaultProfile()(context.Background(), nil, &containers.Container{}, spec); err != nil {
		t.Fatalf("default seccomp: %v", err)
	}
	allowedBindMounts := map[string]sandboxBindMount{
		defaultVolumeMount: {source: "/var/lib/platform/volume", writable: true},
	}
	if err := withWorkloadSandbox(sandboxPolicy{name: productionSandboxName}, allowedBindMounts)(context.Background(), nil, &containers.Container{}, spec); err != nil {
		t.Fatalf("sandbox: %v", err)
	}

	if !spec.Process.NoNewPrivileges || spec.Process.User.UID != sandboxUID || spec.Process.User.GID != sandboxGID {
		t.Fatalf("process identity was not hardened: %+v", spec.Process)
	}
	if len(spec.Process.User.AdditionalGids) != 0 || len(spec.Process.Capabilities.Effective) != 0 {
		t.Fatalf("supplementary identity or capabilities survived: %+v", spec.Process)
	}
	if !spec.Root.Readonly {
		t.Fatal("root filesystem is writable")
	}
	if spec.Linux.Seccomp == nil || spec.Linux.Seccomp.DefaultAction == "" {
		t.Fatal("maintained seccomp profile was not installed")
	}
	if spec.Linux.Resources.Pids == nil || spec.Linux.Resources.Pids.Limit != sandboxProcessLimit {
		t.Fatalf("PID limit = %+v", spec.Linux.Resources.Pids)
	}
	if len(spec.Linux.Devices) != 0 || len(spec.Linux.Sysctl) != 0 || spec.Linux.Resources.Devices[0].Allow {
		t.Fatalf("device/sysctl isolation not enforced: %+v", spec.Linux)
	}
	for _, rule := range spec.Linux.Resources.Devices[1:] {
		if rule.Major != nil && *rule.Major == 10 && rule.Minor != nil && *rule.Minor == 232 {
			t.Fatalf("injected /dev/kvm allow rule survived: %+v", spec.Linux.Resources.Devices)
		}
	}
	for _, namespaceType := range []specs.LinuxNamespaceType{specs.PIDNamespace, specs.IPCNamespace, specs.UTSNamespace, specs.MountNamespace, specs.NetworkNamespace, specs.CgroupNamespace} {
		if !hasNamespace(spec.Linux.Namespaces, namespaceType) {
			t.Fatalf("missing isolated %s namespace: %+v", namespaceType, spec.Linux.Namespaces)
		}
	}
	if network := namespaceFor(spec.Linux.Namespaces, specs.NetworkNamespace); network.Path != "/run/netns/allocation" {
		t.Fatalf("assigned network namespace path lost: %+v", network)
	}
	if pid := namespaceFor(spec.Linux.Namespaces, specs.PIDNamespace); pid.Path != "" {
		t.Fatalf("injected PID namespace path survived: %+v", pid)
	}
	if !slices.Contains(spec.Linux.MaskedPaths, "/sys/fs/bpf") || !slices.Contains(spec.Linux.MaskedPaths, "/proc/kcore") {
		t.Fatalf("sensitive proc/sys paths were not masked: %+v", spec.Linux.MaskedPaths)
	}
	for _, destination := range []string{"/tmp", "/var/tmp", "/run"} {
		mount := mountFor(spec.Mounts, destination)
		if mount.Type != "tmpfs" || !slices.Contains(mount.Options, "nodev") || !slices.Contains(mount.Options, "noexec") {
			t.Fatalf("%s is not a hardened tmpfs: %+v", destination, mount)
		}
	}
	volume := mountFor(spec.Mounts, defaultVolumeMount)
	if !slices.Contains(volume.Options, "rbind") || !slices.Contains(volume.Options, "nosuid") || !slices.Contains(volume.Options, "nodev") {
		t.Fatalf("volume mount options not hardened: %+v", volume)
	}
}

func TestNamedCompatibilityProfileOnlyRelaxesDeclaredControls(t *testing.T) {
	profile := &platformv1.SandboxProfile{
		Name: "legacy-runtime",
		Risk: "Legacy image executes as root and writes its image filesystem.",
		Relaxations: []platformv1.SandboxRelaxation{
			platformv1.SandboxRelaxation_SANDBOX_RELAXATION_RUN_AS_ROOT,
			platformv1.SandboxRelaxation_SANDBOX_RELAXATION_WRITABLE_ROOT_FILESYSTEM,
		},
	}
	policy, err := resolveSandboxPolicy(profile)
	if err != nil {
		t.Fatal(err)
	}
	spec := &specs.Spec{Root: &specs.Root{}, Process: &specs.Process{User: specs.User{}}, Linux: &specs.Linux{Resources: &specs.LinuxResources{}}}
	if err := withWorkloadSandbox(policy, nil)(context.Background(), nil, nil, spec); err != nil {
		t.Fatal(err)
	}
	if spec.Process.User.UID != 0 || spec.Root.Readonly {
		t.Fatalf("declared compatibility relaxations were not honored: %+v", spec)
	}
	if !spec.Process.NoNewPrivileges || spec.Linux.Resources.Pids == nil || len(spec.Process.Capabilities.Effective) != 0 {
		t.Fatalf("non-relaxable protections were lost: %+v", spec)
	}
}

func TestWorkloadSandboxPlacesContainerInReservedCgroup(t *testing.T) {
	spec := &specs.Spec{Root: &specs.Root{}, Process: &specs.Process{}, Linux: &specs.Linux{Resources: &specs.LinuxResources{}}}
	policy := sandboxPolicy{name: productionSandboxName, cgroupPath: "ebpf-wg-mesh-workloads/platform-alloc"}
	if err := withWorkloadSandbox(policy, nil)(context.Background(), nil, nil, spec); err != nil {
		t.Fatal(err)
	}
	if spec.Linux.CgroupsPath != "ebpf-wg-mesh-workloads/platform-alloc" {
		t.Fatalf("cgroups path = %q", spec.Linux.CgroupsPath)
	}
}

func TestProductionSandboxRejectsUnrecognizedHostBindMount(t *testing.T) {
	spec := &specs.Spec{
		Root:    &specs.Root{},
		Process: &specs.Process{},
		Linux:   &specs.Linux{Resources: &specs.LinuxResources{}},
		Mounts: []specs.Mount{{
			Destination: "/host", Type: "bind", Source: "/", Options: []string{"rbind", "rw"},
		}},
	}
	err := withWorkloadSandbox(sandboxPolicy{name: productionSandboxName}, nil)(context.Background(), nil, nil, spec)
	if err == nil {
		t.Fatal("unrecognized host bind mount was accepted")
	}
}

func TestHardMemoryIsolationDisablesSwapAndGroupsOOM(t *testing.T) {
	limit := int64(256 * 1024 * 1024)
	spec := &specs.Spec{Linux: &specs.Linux{Resources: &specs.LinuxResources{Memory: &specs.LinuxMemory{Limit: &limit}}}}
	if err := withHardMemoryIsolation()(context.Background(), nil, nil, spec); err != nil {
		t.Fatal(err)
	}
	if spec.Linux.Resources.Memory.Swap == nil || *spec.Linux.Resources.Memory.Swap != limit {
		t.Fatalf("memory+swap cap = %v, want memory cap %d", spec.Linux.Resources.Memory.Swap, limit)
	}
	if spec.Linux.Resources.Memory.DisableOOMKiller == nil || *spec.Linux.Resources.Memory.DisableOOMKiller {
		t.Fatal("OOM killer must remain enabled")
	}
	if spec.Linux.Resources.Unified["memory.oom.group"] != "1" {
		t.Fatalf("memory.oom.group = %q", spec.Linux.Resources.Unified["memory.oom.group"])
	}
}

func hasNamespace(namespaces []specs.LinuxNamespace, namespaceType specs.LinuxNamespaceType) bool {
	return namespaceFor(namespaces, namespaceType).Type == namespaceType
}

func namespaceFor(namespaces []specs.LinuxNamespace, namespaceType specs.LinuxNamespaceType) specs.LinuxNamespace {
	for _, namespace := range namespaces {
		if namespace.Type == namespaceType {
			return namespace
		}
	}
	return specs.LinuxNamespace{}
}

func mountFor(mounts []specs.Mount, destination string) specs.Mount {
	for _, mount := range mounts {
		if mount.Destination == destination {
			return mount
		}
	}
	return specs.Mount{}
}
