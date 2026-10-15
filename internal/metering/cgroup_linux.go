//go:build linux

package metering

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// WorkloadCgroupParent is the cgroup holding allocation tasks.
const WorkloadCgroupParent = "/sys/fs/cgroup/ebpf-wg-mesh-workloads"

type cgroupSource struct{ parent string }

// CgroupSource scans allocation task cgroups under parent. Directory names
// are container IDs ("platform-<allocationID>").
func CgroupSource(parent string) AllocationSource {
	if parent == "" {
		parent = WorkloadCgroupParent
	}
	return cgroupSource{parent: parent}
}

func (s cgroupSource) Allocations() ([]AllocationSample, error) {
	entries, err := os.ReadDir(s.parent)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []AllocationSample
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "platform-") {
			continue
		}
		sample, err := readCgroupSample(filepath.Join(s.parent, e.Name()), strings.TrimPrefix(e.Name(), "platform-"))
		if err == nil {
			out = append(out, sample)
		}
	}
	return out, nil
}

func (s cgroupSource) Allocation(allocationID string) (AllocationSample, bool, error) {
	dir := filepath.Join(s.parent, "platform-"+allocationID)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return AllocationSample{}, false, nil
	} else if err != nil {
		return AllocationSample{}, false, err
	}
	sample, err := readCgroupSample(dir, allocationID)
	return sample, err == nil, err
}

// ReadCgroup reads one cgroup directory: cumulative CPU microseconds,
// current memory bytes, and the cgroup ID (directory inode). A nil counter
// could not be read; it is unknown, not zero. Exported for runtimes whose
// cgroups live outside the workload parent.
func ReadCgroup(dir string) (cpuMicros, memBytes *uint64, cgroupID uint64, err error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, nil, 0, err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		cgroupID = st.Ino
	}
	if raw, rerr := os.ReadFile(filepath.Join(dir, "cpu.stat")); rerr == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if v, ok := strings.CutPrefix(line, "usage_usec "); ok {
				if n, perr := strconv.ParseUint(strings.TrimSpace(v), 10, 64); perr == nil {
					cpuMicros = &n
				}
			}
		}
	}
	if raw, rerr := os.ReadFile(filepath.Join(dir, "memory.current")); rerr == nil {
		if n, perr := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64); perr == nil {
			memBytes = &n
		}
	}
	return cpuMicros, memBytes, cgroupID, nil
}

func readCgroupSample(dir, allocationID string) (AllocationSample, error) {
	cpu, mem, id, err := ReadCgroup(dir)
	if err != nil {
		return AllocationSample{}, err
	}
	return AllocationSample{AllocationID: allocationID, CgroupID: id, CPUMicros: cpu, MemoryBytes: mem}, nil
}
