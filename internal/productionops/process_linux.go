//go:build linux

package productionops

import (
	"context"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"ebof-wg-mesh/internal/reconciliation"
)

// processAdmission inspects systemd's actual main-process creation time. For
// stateless processes that do not expose an admission RPC, loading a replaced
// credential file requires a process created after that exact atomic rename.
func processAdmission(ctx context.Context, service, file, installation, generation string) error {
	a, err := reconciliation.ReadAuthority(file)
	if err != nil {
		return err
	}
	if a.InstallationID != installation || a.Generation != generation || !a.Paused {
		return fmt.Errorf("stateless admission differs")
	}
	if err = exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", service).Run(); err != nil {
		return err
	}
	b, err := exec.CommandContext(ctx, "systemctl", "show", "--value", "--property=ExecMainStartTimestampMonotonic", service).Output()
	if err != nil {
		return err
	}
	micros, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || micros <= 0 {
		return fmt.Errorf("unknown native process start time")
	}
	var realtime, monotonic unix.Timespec
	if err = unix.ClockGettime(unix.CLOCK_REALTIME, &realtime); err != nil {
		return err
	}
	if err = unix.ClockGettime(unix.CLOCK_MONOTONIC, &monotonic); err != nil {
		return err
	}
	started := time.Unix(0, realtime.Nano()-monotonic.Nano()+micros*1000)
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	if started.Before(info.ModTime().Add(time.Millisecond)) {
		return fmt.Errorf("process predates replacement credentials")
	}
	return nil
}
