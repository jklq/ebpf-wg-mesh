//go:build linux

package builder

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// remountSnapshotReadOnly bind-mounts the snapshot directory onto
// itself read-only so the read-only snapshot holds for root too:
// permission bits do not bind uid 0, but a read-only bind mount is
// enforced at the VFS layer regardless of capabilities. It is a no-op
// for non-root users, for whom the permission bits already hold.
// destroyExecutionWorkspace unmounts before removal.
func remountSnapshotReadOnly(repoDir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	if err := unix.Mount(repoDir, repoDir, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind-mount source snapshot: %w", err)
	}
	if err := unix.Mount(repoDir, repoDir, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		_ = unix.Unmount(repoDir, 0)
		return fmt.Errorf("remount source snapshot read-only: %w", err)
	}
	return nil
}

// unmountSnapshot detaches a read-only snapshot bind mount. It is
// best effort: removal verifies the outcome, so a failed unmount
// surfaces there rather than here.
func unmountSnapshot(repoDir string) {
	if os.Geteuid() != 0 {
		return
	}
	_ = unix.Unmount(repoDir, 0)
}
