//go:build linux

package recovery

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// Refuse direct invocation in the caller's namespace and enable the only
// interface in the fresh namespace before any release-owned tool runs.
func prepareIsolation(parent string) error {
	current, err := os.Readlink("/proc/self/ns/net")
	if err != nil || parent == "" || current == parent {
		return fmt.Errorf("restore validation requires a fresh network namespace")
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return err
	}
	if len(interfaces) != 1 || interfaces[0].Name != "lo" || interfaces[0].Flags&net.FlagLoopback == 0 {
		return fmt.Errorf("restore namespace must contain only loopback")
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	request, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, request); err != nil {
		return err
	}
	request.SetUint16(request.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, request)
}
