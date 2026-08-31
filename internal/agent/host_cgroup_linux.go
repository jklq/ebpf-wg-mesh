//go:build linux

package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"ebof-wg-mesh/internal/config"
)

const (
	agentOOMScoreAdj          = -500
	workloadCgroupParentName  = "ebpf-wg-mesh-workloads"
	cgroupFSRoot              = "/sys/fs/cgroup"
	sandboxMemoryMinBytesPath = "memory.min"
	sandboxMemoryLowBytesPath = "memory.low"
	sandboxMemoryMaxBytesPath = "memory.max"
	sandboxCPUWeightPath      = "cpu.weight"
)

func protectAgentRuntime(cfg config.AgentConfig) (string, error) {
	if cfg.Profile.IsProduction() && cfg.Runtime.DisableCgroups {
		return "", errors.New("reserve agent/runtime cgroup resources: cgroups are disabled in production")
	}
	reservedBytes := cfg.Node.Resources.ReservedMemoryMebibytes * 1024 * 1024
	workloadBytes := cfg.Node.Resources.AdvertisedMemoryMebibytes() * 1024 * 1024
	if !cfg.Runtime.DisableCgroups && (reservedBytes <= 0 || workloadBytes <= 0) {
		if cfg.Profile.IsProduction() {
			return "", fmt.Errorf("reserve agent/runtime cgroup resources: reserved memory and schedulable workload memory must both be greater than zero")
		}
		return "", nil
	}
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte(strconv.Itoa(agentOOMScoreAdj)), 0o644); err != nil {
		if cfg.Profile.IsProduction() && !cfg.Runtime.DisableCgroups {
			return "", fmt.Errorf("protect agent from tenant OOM: %w", err)
		}
	}
	if cfg.Runtime.DisableCgroups {
		return "", nil
	}
	parent, err := applyHostCgroupReservation(cgroupFSRoot, reservedBytes, workloadBytes)
	if err != nil {
		if cfg.Profile.IsProduction() {
			return "", fmt.Errorf("reserve agent/runtime cgroup resources: %w", err)
		}
		return "", nil
	}
	return parent, nil
}

func applyHostCgroupReservation(cgroupRoot string, reservedBytes, workloadBytes int64) (string, error) {
	current, err := currentCgroupPath()
	if err != nil {
		return "", err
	}
	return applyHostCgroupReservationAt(cgroupRoot, current, reservedBytes, workloadBytes)
}

func applyHostCgroupReservationAt(cgroupRoot, current string, reservedBytes, workloadBytes int64) (string, error) {
	agentDir := filepath.Join(cgroupRoot, strings.TrimPrefix(current, "/"))
	if err := writeCgroupValue(agentDir, sandboxMemoryMinBytesPath, reservedBytes); err != nil {
		return "", err
	}
	if err := writeCgroupValue(agentDir, sandboxMemoryLowBytesPath, reservedBytes); err != nil {
		return "", err
	}
	if err := writeCgroupValue(agentDir, sandboxCPUWeightPath, 10000); err != nil {
		return "", err
	}

	parent := filepath.Join(cgroupRoot, workloadCgroupParentName)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("create workload cgroup parent: %w", err)
	}
	if err := enableCgroupControllers(cgroupRoot, "cpu", "memory", "pids"); err != nil {
		return "", err
	}
	if err := writeCgroupValue(parent, sandboxMemoryMaxBytesPath, workloadBytes); err != nil {
		return "", err
	}
	return workloadCgroupParentName, nil
}

func currentCgroupPath() (string, error) {
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "0::") {
			path := strings.TrimSpace(strings.TrimPrefix(line, "0::"))
			if path == "" {
				path = "/"
			}
			return path, nil
		}
	}
	return "", fmt.Errorf("cgroup v2 path not found")
}

func enableCgroupControllers(cgroupRoot string, controllers ...string) error {
	path := filepath.Join(cgroupRoot, "cgroup.subtree_control")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read cgroup.subtree_control: %w", err)
	}
	enabled := strings.Fields(string(raw))
	var missing []string
	for _, controller := range controllers {
		found := false
		for _, existing := range enabled {
			if existing == controller {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, "+"+controller)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if err := os.WriteFile(path, []byte(strings.Join(missing, " ")), 0o644); err != nil {
		return fmt.Errorf("enable cgroup controllers %s: %w", strings.Join(missing, " "), err)
	}
	return nil
}

func writeCgroupValue(dir, name string, value int64) error {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strconv.FormatInt(value, 10)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
