package isolation

import (
	"strings"
	"testing"
)

func TestNormalizeClassDefaultsByProfile(t *testing.T) {
	if got, err := NormalizeClass("", true); err != nil || got != ClassGVisor {
		t.Fatalf("production default = %q, %v; want gvisor", got, err)
	}
	if got, err := NormalizeClass("", false); err != nil || got != ClassRuncUserNS {
		t.Fatalf("development default = %q, %v; want runc-userns", got, err)
	}
	for _, raw := range []string{"runc", "io.containerd.runc.v2", "none", "docker", "firecracker"} {
		if _, err := NormalizeClass(raw, true); err == nil {
			t.Fatalf("class %q accepted; host-root runtimes must not be selectable", raw)
		}
	}
}

func TestPolicyMappingsAreScopedAndDeterministic(t *testing.T) {
	policy, err := NewPolicy(ClassRuncUserNS)
	if err != nil {
		t.Fatal(err)
	}
	uids, gids, err := policy.Mappings("env-a")
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := policy.Mappings("env-a")
	if err != nil {
		t.Fatal(err)
	}
	if uids[0] != again[0] {
		t.Fatalf("mapping for a scope changed between calls: %+v vs %+v", uids, again)
	}
	if uids[0].ContainerID != 0 || uids[0].Size != usernsSize || uids[0].HostID < usernsBase {
		t.Fatalf("unexpected mapping: %+v", uids[0])
	}
	if uids[0].HostID == 0 {
		t.Fatal("container root maps to host root")
	}
	if gids[0] != uids[0] {
		t.Fatalf("gid mapping differs from uid mapping: %+v vs %+v", gids, uids)
	}
	other, _, err := policy.Mappings("env-b")
	if err != nil {
		t.Fatal(err)
	}
	if other[0].HostID == uids[0].HostID {
		t.Fatal("distinct scopes share a host identity range")
	}
	if _, _, err := policy.Mappings(""); err == nil {
		t.Fatal("empty scope accepted")
	}
}

func TestPolicyHostIDTranslation(t *testing.T) {
	policy, _ := NewPolicy(ClassRuncUserNS)
	uids, _, _ := policy.Mappings("env-a")
	host, err := policy.HostUID("env-a", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if host != uids[0].HostID+1000 {
		t.Fatalf("HostUID = %d, want %d", host, uids[0].HostID+1000)
	}
	if _, err := policy.HostUID("env-a", usernsSize); err == nil {
		t.Fatal("out-of-range container ID accepted")
	}

	gvisor, _ := NewPolicy(ClassGVisor)
	if uids, gids, err := gvisor.Mappings("env-a"); err != nil || uids != nil || gids != nil {
		t.Fatalf("kernel-separated mappings = %v,%v,%v; want nil", uids, gids, err)
	}
	if host, err := gvisor.HostUID("env-a", 1000); err != nil || host != 1000 {
		t.Fatalf("kernel-separated HostUID = %d,%v; want identity", host, err)
	}
}

func TestPolicyRuntimeNames(t *testing.T) {
	cases := []struct {
		class   Class
		runtime string
		shim    string
		worker  string
		kernel  bool
		userns  bool
	}{
		{ClassRuncUserNS, "io.containerd.runc.v2", "containerd-shim-runc-v2", "runc", false, true},
		{ClassGVisor, "io.containerd.runsc.v1", "containerd-shim-runsc-v1", "runsc", true, false},
		{ClassKata, "io.containerd.kata.v2", "containerd-shim-kata-v2", "kata-runtime", true, false},
	}
	for _, tc := range cases {
		policy, err := NewPolicy(tc.class)
		if err != nil {
			t.Fatal(err)
		}
		if policy.ContainerdRuntime() != tc.runtime || policy.ShimBinary() != tc.shim || policy.WorkerBinary() != tc.worker {
			t.Fatalf("%s: runtime=%s shim=%s worker=%s", tc.class, policy.ContainerdRuntime(), policy.ShimBinary(), policy.WorkerBinary())
		}
		if policy.KernelSeparated() != tc.kernel || policy.UsesUserNamespaces() != tc.userns {
			t.Fatalf("%s: kernel=%t userns=%t", tc.class, policy.KernelSeparated(), policy.UsesUserNamespaces())
		}
	}
}

func TestGVisorMemoryFloor(t *testing.T) {
	gvisor, _ := NewPolicy(ClassGVisor)
	for _, mebibytes := range []int64{0, 64, GVisorMinMemoryMebibytes - 1} {
		err := gvisor.ValidateMemoryMebibytes(mebibytes)
		if err == nil || !strings.Contains(err.Error(), "Sentry") {
			t.Fatalf("gvisor %d MiB accepted or unexplained: %v", mebibytes, err)
		}
	}
	if err := gvisor.ValidateMemoryMebibytes(GVisorMinMemoryMebibytes); err != nil {
		t.Fatalf("gvisor floor rejected: %v", err)
	}
	for _, class := range []Class{ClassRuncUserNS, ClassKata} {
		policy, _ := NewPolicy(class)
		if policy.MinMemoryMebibytes() != 0 || policy.ValidateMemoryMebibytes(64) != nil {
			t.Fatalf("%s gained a memory floor", class)
		}
	}
	if got := gvisor.RuntimeCapability(); got != "isolation:gvisor" || got != RuntimeCapability(ClassGVisor) {
		t.Fatalf("runtime capability = %q", got)
	}
}
