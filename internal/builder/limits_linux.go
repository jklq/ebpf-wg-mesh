//go:build linux

package builder

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// applyProcessLimits enforces the execution's process limits on an
// already-started build child via prlimit(2). Limits are checked by the
// kernel at allocation time (address-space growth, CPU accounting,
// file growth, fork), so applying them immediately after Start bounds
// everything the child does afterwards. The development executor can
// only bound its direct build children this way; the BuildKit daemon
// doing the actual build work is shared host state until 2.4b.
// RLIMIT_NPROC counts every thread of the builder UID, so the process
// limit must be sized for the machine's total usage, not one build;
// per-execution PID containment is 2.4b work.
func applyProcessLimits(pid int, limits ProcessLimits) error {
	const unlimited = ^uint64(0)
	targets := []struct {
		name     string
		resource int
		value    int64
	}{
		{"memory", unix.RLIMIT_AS, limits.MemoryBytes},
		{"cpu", unix.RLIMIT_CPU, limits.CPUSeconds},
		{"file size", unix.RLIMIT_FSIZE, limits.MaxFileBytes},
		{"processes", unix.RLIMIT_NPROC, limits.MaxProcesses},
	}
	for _, target := range targets {
		if target.value <= 0 {
			continue
		}
		value := uint64(target.value)
		if value > unlimited {
			value = unlimited
		}
		if err := unix.Prlimit(pid, target.resource, &unix.Rlimit{Cur: value, Max: value}, nil); err != nil {
			return fmt.Errorf("apply %s limit to build process %d: %w", target.name, pid, err)
		}
	}
	return nil
}

// processAlive reports whether pid refers to a live process. EPERM
// means the process exists but belongs to another user.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := unix.Kill(pid, 0)
	return err == nil || err == unix.EPERM
}
