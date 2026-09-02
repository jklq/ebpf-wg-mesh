//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ebof-wg-mesh/internal/config"
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
	parentName, err := applyHostCgroupReservationAt(root, "/system.slice/ebpf-wg-mesh-agent.service", reserved, workload)
	if err != nil {
		t.Fatal(err)
	}
	if parentName != workloadCgroupParentName {
		t.Fatalf("workload cgroup parent = %q", parentName)
	}
	parent := filepath.Join(root, parentName)

	gotMin, err := os.ReadFile(filepath.Join(agentDir, "memory.min"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotMin) != "536870912" {
		t.Fatalf("memory.min = %s", gotMin)
	}
	gotLow, err := os.ReadFile(filepath.Join(agentDir, "memory.low"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotLow) != "536870912" {
		t.Fatalf("memory.low = %s", gotLow)
	}
	gotWeight, err := os.ReadFile(filepath.Join(agentDir, "cpu.weight"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotWeight) != "10000" {
		t.Fatalf("cpu.weight = %s", gotWeight)
	}
	gotMax, err := os.ReadFile(filepath.Join(parent, "memory.max"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotMax) != "2147483648" {
		t.Fatalf("memory.max = %s", gotMax)
	}
}

func TestProtectAgentRuntimeRejectsMissingProductionReservation(t *testing.T) {
	_, err := protectAgentRuntime(config.AgentConfig{
		Profile: config.ProfileProduction,
		Node: config.NodeConfig{Resources: config.NodeResourcesConfig{
			MemoryMebibytes:         4096,
			ReservedMemoryMebibytes: 0,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "reserved memory") {
		t.Fatalf("protectAgentRuntime error = %v", err)
	}
}

func TestProtectAgentRuntimeRejectsDisabledProductionCgroups(t *testing.T) {
	_, err := protectAgentRuntime(config.AgentConfig{
		Profile: config.ProfileProduction,
		Runtime: config.RuntimeConfig{DisableCgroups: true},
	})
	if err == nil || !strings.Contains(err.Error(), "disabled in production") {
		t.Fatalf("protectAgentRuntime error = %v", err)
	}
}

func TestHostReservationFailsClosedWithoutCgroupV2Hierarchy(t *testing.T) {
	_, err := applyHostCgroupReservationAt(t.TempDir(), "/missing-agent-cgroup", 512*1024*1024, 2048*1024*1024)
	if err == nil {
		t.Fatal("missing cgroup v2 hierarchy was accepted")
	}
}

func TestProtectAgentRuntimeAllowsMissingDevelopmentReservation(t *testing.T) {
	parent, err := protectAgentRuntime(config.AgentConfig{
		Profile: config.ProfileDevelopment,
		Node: config.NodeConfig{Resources: config.NodeResourcesConfig{
			MemoryMebibytes: 4096,
		}},
	})
	if err != nil || parent != "" {
		t.Fatalf("protectAgentRuntime = %q, %v", parent, err)
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
