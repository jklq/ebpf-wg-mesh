//go:build linux

package builder

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Memory admission uses actual free RAM and each cgroup v2 ancestor's remaining
// budget. It fails closed if MemAvailable cannot be read. CPU reports configured
// quota/affinity headroom, not momentary idle CPU (which changes during a build).
func availableBuildMemory(reserve int64) int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	var available int64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			n, _ := strconv.ParseInt(fields[1], 10, 64)
			available = n * 1024
			break
		}
	}
	for _, path := range buildCgroupPaths() {
		limit, ok := readCapacityInt(filepath.Join(path, "memory.max"))
		if !ok {
			continue
		}
		used, ok := readCapacityInt(filepath.Join(path, "memory.current"))
		if !ok {
			return 0
		}
		available = min(available, max(0, limit-used))
	}
	return max(0, available-reserve)
}
func availableBuildCPU(reserve int64) int64 {
	available := int64(runtime.GOMAXPROCS(0)) * 1000
	for _, path := range buildCgroupPaths() {
		data, err := os.ReadFile(filepath.Join(path, "cpu.max"))
		if err != nil {
			continue
		}
		fields := strings.Fields(string(data))
		if len(fields) != 2 || fields[0] == "max" {
			continue
		}
		quota, e1 := strconv.ParseInt(fields[0], 10, 64)
		period, e2 := strconv.ParseInt(fields[1], 10, 64)
		if e1 != nil || e2 != nil || period <= 0 {
			return 0
		}
		available = min(available, quota*1000/period)
	}
	return max(0, available-reserve)
}
func readCapacityInt(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	return n, err == nil && n >= 0
}
func buildCgroupPaths() []string {
	root := "/sys/fs/cgroup"
	path := root
	if file, err := os.Open("/proc/self/cgroup"); err == nil {
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			if relative, ok := strings.CutPrefix(scanner.Text(), "0::"); ok {
				candidate := filepath.Join(root, filepath.Clean("/"+relative))
				if _, err := os.Stat(filepath.Join(candidate, "cgroup.controllers")); err == nil {
					path = candidate
				}
				break
			}
		}
		file.Close()
	}
	var paths []string
	for {
		paths = append(paths, path)
		if path == root {
			break
		}
		parent := filepath.Dir(path)
		if parent == path || !strings.HasPrefix(parent, root) {
			break
		}
		path = parent
	}
	return paths
}
