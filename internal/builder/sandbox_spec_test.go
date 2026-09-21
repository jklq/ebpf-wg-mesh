package builder

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/namespaces"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func generateSandboxSpec(t *testing.T, input sandboxSpecInput) *specs.Spec {
	t.Helper()
	opts, err := buildSandboxSpecOpts(input)
	if err != nil {
		t.Fatalf("buildSandboxSpecOpts: %v", err)
	}
	var spec specs.Spec
	// WithDefaultSpec reads the container ID and namespace; the
	// client stays nil because no sandbox option dials the daemon.
	ctx := namespaces.WithNamespace(context.Background(), "sandbox-spec-test")
	container := &containers.Container{ID: "sandbox-spec-test"}
	for _, opt := range opts {
		if err := opt(ctx, nil, container, &spec); err != nil {
			t.Fatalf("spec opt: %v", err)
		}
	}
	return &spec
}

func sandboxSpecInputForTest(t *testing.T) (sandboxSpecInput, string) {
	t.Helper()
	root, mounts := sandboxMountsForTest(t)
	return sandboxSpecInput{
		Argv:      []string{"buildctl", "--addr", "unix:///build/s/bk.sock", "build"},
		Env:       []string{"PATH=/usr/bin:/bin", "HOME=/build/scratch", "TMPDIR=/build/tmp"},
		Dir:       "/build/scratch",
		NetNSPath: "/run/netns/build-test",
		Limits: ResourceLimits{
			Timeout:           time.Minute,
			MemoryBytes:       1 << 30,
			CPUSeconds:        60,
			MaxFileBytes:      1 << 30,
			MaxProcesses:      512,
			MaxWorkspaceBytes: 1 << 30,
		},
		Mounts: mounts,
	}, root
}

func TestSandboxSpecRunsExplicitCommandWithExplicitEnv(t *testing.T) {
	t.Parallel()

	input, _ := sandboxSpecInputForTest(t)
	spec := generateSandboxSpec(t, input)
	if len(spec.Process.Args) != 4 || spec.Process.Args[0] != "buildctl" {
		t.Fatalf("unexpected argv %#v", spec.Process.Args)
	}
	if len(spec.Process.Env) != len(input.Env) {
		t.Fatalf("sandbox env must be exactly the step env, got %#v", spec.Process.Env)
	}
	for i, entry := range input.Env {
		if spec.Process.Env[i] != entry {
			t.Fatalf("sandbox env = %#v, want %#v", spec.Process.Env, input.Env)
		}
	}
	if spec.Process.Cwd != "/build/scratch" {
		t.Fatalf("unexpected cwd %q", spec.Process.Cwd)
	}
	if spec.Hostname != "build" {
		t.Fatalf("unexpected hostname %q", spec.Hostname)
	}
}

func TestSandboxSpecIsolatesNamespaces(t *testing.T) {
	t.Parallel()

	input, _ := sandboxSpecInputForTest(t)
	spec := generateSandboxSpec(t, input)
	seen := map[specs.LinuxNamespaceType]string{}
	for _, namespace := range spec.Linux.Namespaces {
		seen[namespace.Type] = namespace.Path
	}
	for _, namespaceType := range []specs.LinuxNamespaceType{
		specs.PIDNamespace, specs.IPCNamespace, specs.UTSNamespace,
		specs.MountNamespace, specs.NetworkNamespace, specs.CgroupNamespace,
	} {
		path, ok := seen[namespaceType]
		if !ok {
			t.Fatalf("missing %s namespace", namespaceType)
		}
		if namespaceType == specs.NetworkNamespace {
			if path != input.NetNSPath {
				t.Fatalf("network namespace = %q, want %q", path, input.NetNSPath)
			}
			continue
		}
		if path != "" {
			t.Fatalf("%s namespace must be private, got path %q", namespaceType, path)
		}
	}
}

func TestSandboxSpecStripsPrivileges(t *testing.T) {
	t.Parallel()

	input, _ := sandboxSpecInputForTest(t)
	spec := generateSandboxSpec(t, input)
	if !spec.Process.NoNewPrivileges {
		t.Fatal("sandbox must set no-new-privileges")
	}
	caps := spec.Process.Capabilities
	if len(caps.Bounding) != 0 || len(caps.Effective) != 0 || len(caps.Permitted) != 0 || len(caps.Ambient) != 0 {
		t.Fatalf("sandbox must drop all capabilities, got %+v", caps)
	}
	if !spec.Root.Readonly {
		t.Fatal("sandbox root filesystem must be read-only")
	}
	if len(spec.Linux.Devices) != 0 {
		t.Fatalf("sandbox must allow no devices, got %+v", spec.Linux.Devices)
	}
	rules := spec.Linux.Resources.Devices
	if len(rules) == 0 || rules[0].Allow {
		t.Fatalf("sandbox device policy must start with deny-all, got %+v", rules)
	}
	if len(spec.Linux.MaskedPaths) == 0 || len(spec.Linux.ReadonlyPaths) == 0 {
		t.Fatal("sandbox must mask and read-only-protect system paths")
	}
	if spec.Linux.Sysctl != nil {
		t.Fatalf("sandbox must not set sysctls, got %+v", spec.Linux.Sysctl)
	}
}

func TestSandboxSpecEnforcesLimits(t *testing.T) {
	t.Parallel()

	input, _ := sandboxSpecInputForTest(t)
	spec := generateSandboxSpec(t, input)
	memory := spec.Linux.Resources.Memory
	if memory == nil || memory.Limit == nil || *memory.Limit != input.Limits.MemoryBytes {
		t.Fatalf("memory limit = %+v, want %d", memory, input.Limits.MemoryBytes)
	}
	if memory.Swap == nil || *memory.Swap != input.Limits.MemoryBytes {
		t.Fatal("swap must be capped to the memory limit so the cap cannot be escaped via swap")
	}
	if spec.Linux.Resources.Unified["memory.oom.group"] != "1" {
		t.Fatal("sandbox must kill as a group on OOM")
	}
	if spec.Linux.Resources.Pids == nil || spec.Linux.Resources.Pids.Limit != input.Limits.MaxProcesses {
		t.Fatalf("pids limit = %+v, want %d", spec.Linux.Resources.Pids, input.Limits.MaxProcesses)
	}
	rlimits := map[string]specs.POSIXRlimit{}
	for _, rlimit := range spec.Process.Rlimits {
		rlimits[rlimit.Type] = rlimit
	}
	for limitType, want := range map[string]int64{
		"RLIMIT_CPU":   input.Limits.CPUSeconds,
		"RLIMIT_FSIZE": input.Limits.MaxFileBytes,
		"RLIMIT_NPROC": input.Limits.MaxProcesses,
	} {
		got, ok := rlimits[limitType]
		if !ok || got.Soft != uint64(want) || got.Hard != uint64(want) {
			t.Fatalf("%s = %+v, want %d", limitType, got, want)
		}
	}
	if _, ok := rlimits["RLIMIT_AS"]; ok {
		t.Fatal("sandbox must not set RLIMIT_AS: the cgroup resident-set cap replaces the virtual-address bound")
	}
}

func TestSandboxSpecMountsWorkspaceOnly(t *testing.T) {
	t.Parallel()

	input, root := sandboxSpecInputForTest(t)
	spec := generateSandboxSpec(t, input)
	binds := map[string]specs.Mount{}
	for _, mount := range spec.Mounts {
		if mount.Type == "bind" {
			binds[mount.Destination] = mount
		}
	}
	workspace, ok := binds["/build"]
	if !ok {
		t.Fatalf("missing /build mount in %+v", spec.Mounts)
	}
	if workspace.Source != root || !hasMountOption(workspace.Options, "rw") {
		t.Fatalf("unexpected /build mount %+v", workspace)
	}
	repo, ok := binds["/build/repo"]
	if !ok {
		t.Fatal("missing read-only snapshot mount")
	}
	if !hasMountOption(repo.Options, "ro") || !hasMountOption(repo.Options, "noexec") {
		t.Fatalf("snapshot mount must be read-only and noexec, got %+v", repo.Options)
	}
	for _, dest := range []string{"/etc/resolv.conf", "/etc/hosts"} {
		mount, ok := binds[dest]
		if !ok || !hasMountOption(mount.Options, "ro") {
			t.Fatalf("missing read-only %s mount", dest)
		}
	}
	for dest, mount := range binds {
		for _, option := range []string{"rbind", "nosuid", "nodev"} {
			if !hasMountOption(mount.Options, option) {
				t.Fatalf("bind %s missing %s: %+v", dest, option, mount.Options)
			}
		}
		if strings.HasPrefix(mount.Source, "/run/") || strings.HasPrefix(mount.Source, "/var/run/") {
			t.Fatalf("bind %s exposes host runtime state %q", dest, mount.Source)
		}
	}
	// The writable root must sort before the nested read-only
	// snapshot so the nesting applies in order.
	var rootIndex, repoIndex = -1, -1
	for i, mount := range spec.Mounts {
		switch mount.Destination {
		case "/build":
			rootIndex = i
		case "/build/repo":
			repoIndex = i
		}
	}
	if rootIndex < 0 || repoIndex < 0 || rootIndex > repoIndex {
		t.Fatalf("mount order must nest repo inside root, got root=%d repo=%d", rootIndex, repoIndex)
	}
}

func TestSandboxSpecBoundsTmpfsFromLimits(t *testing.T) {
	t.Parallel()

	if got := sandboxTmpfsSize(ResourceLimits{MaxWorkspaceBytes: 32 << 20}); got != "16m" {
		t.Fatalf("small disk budget tmpfs = %q, want 16m floor", got)
	}
	if got := sandboxTmpfsSize(ResourceLimits{MaxWorkspaceBytes: 1 << 30}); got != "128m" {
		t.Fatalf("1GiB disk budget tmpfs = %q, want 128m", got)
	}
	if got := sandboxTmpfsSize(ResourceLimits{MaxWorkspaceBytes: 20 << 30}); got != "256m" {
		t.Fatalf("large disk budget tmpfs = %q, want 256m cap", got)
	}

	input, _ := sandboxSpecInputForTest(t)
	input.Limits.MaxWorkspaceBytes = 32 << 20
	spec := generateSandboxSpec(t, input)
	for _, mount := range spec.Mounts {
		if mount.Destination != "/tmp" || mount.Type != "tmpfs" {
			continue
		}
		if !hasMountOption(mount.Options, "size=16m") || !hasMountOption(mount.Options, "noexec") {
			t.Fatalf("unexpected /tmp mount %+v", mount.Options)
		}
	}
}

func TestSandboxSpecDeniesNamespaceManipulation(t *testing.T) {
	t.Parallel()

	input, _ := sandboxSpecInputForTest(t)
	spec := generateSandboxSpec(t, input)
	seccomp := spec.Linux.Seccomp
	if seccomp == nil {
		t.Fatal("sandbox requires a seccomp profile")
	}
	// The default profile denies unshare, setns, and namespaced
	// clone only when the bounding set lacks CAP_SYS_ADMIN, which is
	// why the lockdown runs before the seccomp option. If that
	// ordering ever regresses, unshare becomes allowed and this test
	// fails.
	for _, rule := range seccomp.Syscalls {
		if rule.Action != "SCMP_ACT_ALLOW" {
			continue
		}
		for _, name := range rule.Names {
			if name == "unshare" || name == "setns" || name == "clone3" {
				t.Fatalf("seccomp must not allow %s without CAP_SYS_ADMIN", name)
			}
		}
	}
}

func TestBuildSandboxSpecOptsRejectsHostNetworking(t *testing.T) {
	t.Parallel()

	input, _ := sandboxSpecInputForTest(t)
	input.NetNSPath = ""
	if _, err := buildSandboxSpecOpts(input); err == nil || !strings.Contains(err.Error(), "host networking is forbidden") {
		t.Fatalf("expected host-networking refusal, got %v", err)
	}
	input, _ = sandboxSpecInputForTest(t)
	input.Argv = nil
	if _, err := buildSandboxSpecOpts(input); err == nil {
		t.Fatal("expected empty argv to fail")
	}
	input, _ = sandboxSpecInputForTest(t)
	input.Env = []string{"NOEQUALS"}
	if _, err := buildSandboxSpecOpts(input); err == nil {
		t.Fatal("expected malformed env to fail")
	}
	input, _ = sandboxSpecInputForTest(t)
	input.Limits.MemoryBytes = 0
	if _, err := buildSandboxSpecOpts(input); err == nil {
		t.Fatal("expected bad limits to fail")
	}
}

func hasMountOption(options []string, want string) bool {
	for _, option := range options {
		if option == want {
			return true
		}
	}
	return false
}
