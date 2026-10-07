//go:build linux

package recovery

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

// The caller already created a private mount/network/PID namespace. Durable
// production path validation must apply to the native runtimes in a drill too;
// private mount aliases avoid changing production validation for temporary files.
func mountOfflineWorkspace(workspace string) error {
	if err := unix.Mount("tmpfs", "/var/lib", "tmpfs", 0, "mode=0700"); err != nil {
		return err
	}
	target := "/var/lib/platform-offline"
	if err := os.MkdirAll(target, 0700); err != nil {
		return err
	}
	if err := unix.Mount(workspace, target, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	hosts := filepath.Join(workspace, "offline-hosts")
	if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost database.offline.invalid controlplane.offline.invalid console.offline.invalid\n"), 0600); err != nil {
		return err
	}
	return unix.Mount(hosts, "/etc/hosts", "", unix.MS_BIND, "")
}
