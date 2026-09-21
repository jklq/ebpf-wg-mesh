//go:build linux

package builder

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// currentUIDThreadCount counts live threads owned by the current UID
// by walking /proc. Vanishing processes are skipped: the count only
// needs to be close, and headroom absorbs the rest.
func currentUIDThreadCount() (int64, error) {
	self := os.Geteuid()
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	var total int64
	for _, proc := range procs {
		if !proc.IsDir() {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(proc.Name(), "%d", &pid); err != nil || fmt.Sprintf("%d", pid) != proc.Name() {
			continue
		}
		info, err := proc.Info()
		if err != nil {
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != self {
			continue
		}
		tasks, err := os.ReadDir(filepath.Join("/proc", proc.Name(), "task"))
		if err != nil {
			continue
		}
		total += int64(len(tasks))
	}
	return total, nil
}
