//go:build linux

package metering

import (
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type systemClock struct{ boot string }

// SystemClock reads the kernel boot ID and CLOCK_BOOTTIME, which keeps
// counting across agent restarts and host suspend within one boot.
func SystemClock() (Clock, error) {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, err
	}
	return systemClock{boot: strings.TrimSpace(string(raw))}, nil
}

func (c systemClock) Now() Instant {
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts)
	return Instant{Boot: c.boot, Mono: time.Duration(ts.Nano()), Wall: time.Now()}
}
