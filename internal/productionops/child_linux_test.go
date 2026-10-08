//go:build linux && integration

package productionops

import (
	"os/exec"
	"syscall"
)

// Native fixtures must not outlive the test process, including a Go timeout.
func isolateChild(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL} }
