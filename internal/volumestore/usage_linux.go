//go:build linux

package volumestore

import (
	"os"
	"syscall"
)

// allocatedBytes is the disk space a file occupies (st_blocks), deduplicated
// across hard links by the caller. ok is false without block accounting.
func allocatedBytes(info os.FileInfo) (bytes int64, dev, ino uint64, nlink uint64, ok bool) {
	st, isStat := info.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, 0, 0, 0, false
	}
	return st.Blocks * 512, uint64(st.Dev), st.Ino, uint64(st.Nlink), true
}
