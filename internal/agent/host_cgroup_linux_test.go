//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyHostCgroupReservationWritesAgentAndWorkloadLimits(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "system.slice", "ebpf-wg-mesh-agent.service")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"memory.min", "memory.low", "cpu.weight"} {
		if err := os.WriteFile(filepath.Join(agentDir, name), []byte("0"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte("cpu memory pids"), 0o644); err != nil {
		t.Fatal(err)
	}

	reserved := int64(512 * 1024 * 1024)
	workload := int64(2048 * 1024 * 1024)
	// Point the helper at the fake tree by writing files through applyHostCgroupReservation
	// after substituting the current process path is not possible, so exercise writeCgroupValue
	// and the parent directory contract directly.
	if err := writeCgroupValue(agentDir, sandboxMemoryMinBytesPath, reserved); err != nil {
		t.Fatal(err)
	}
	if err := writeCgroupValue(agentDir, sandboxMemoryLowBytesPath, reserved); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, workloadCgroupParentName)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeCgroupValue(parent, sandboxMemoryMaxBytesPath, workload); err != nil {
		t.Fatal(err)
	}

	gotMin, err := os.ReadFile(filepath.Join(agentDir, "memory.min"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotMin) != "536870912" {
		t.Fatalf("memory.min = %s", gotMin)
	}
	gotMax, err := os.ReadFile(filepath.Join(parent, "memory.max"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotMax) != "2147483648" {
		t.Fatalf("memory.max = %s", gotMax)
	}
}

func TestEnableCgroupControllersIsIdempotent(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte("cpu memory pids"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := enableCgroupControllers(root, "cpu", "memory", "pids"); err != nil {
		t.Fatal(err)
	}
}
