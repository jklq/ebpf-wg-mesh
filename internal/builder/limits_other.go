//go:build !linux

package builder

import (
	"os"
	"syscall"
)

// applyProcessLimits is a documented no-op off Linux: only Linux
// prlimit(2) can bound an already-started child. Non-Linux builders are
// development-only; production builders run on Linux where limits are
// enforced.
func applyProcessLimits(_ int, _ ProcessLimits) error {
	return nil
}

// processAlive reports whether pid refers to a live process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
