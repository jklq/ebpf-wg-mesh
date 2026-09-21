//go:build linux

package builder

import (
	"context"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// newBuildCommand starts each build child in its own process group so
// cancellation kills the whole build process tree, not just the
// direct child.
func newBuildCommand(ctx context.Context, binary string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return killBuildTree(cmd)
	}
	return cmd
}

// killBuildTree kills the build child's process group. A negative pid
// addresses the group; ESRCH means the tree already exited.
func killBuildTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if err := unix.Kill(-pid, unix.SIGKILL); err != nil && err != unix.ESRCH {
		return cmd.Process.Kill()
	}
	return nil
}
