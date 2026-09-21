//go:build !linux

package builder

// remountSnapshotReadOnly is Linux-only: elsewhere the permission
// bits are the whole read-only story.
func remountSnapshotReadOnly(string) error {
	return nil
}

// unmountSnapshot detaches a read-only snapshot bind mount. It is a
// no-op where remounts never happen.
func unmountSnapshot(string) {}
