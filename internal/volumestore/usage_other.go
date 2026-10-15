//go:build !linux

package volumestore

import "os"

func allocatedBytes(info os.FileInfo) (bytes int64, dev, ino uint64, nlink uint64, ok bool) {
	return info.Size(), 0, 0, 1, false
}
