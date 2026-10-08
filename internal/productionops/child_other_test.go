//go:build !linux && integration

package productionops

import "os/exec"

func isolateChild(*exec.Cmd) {}
