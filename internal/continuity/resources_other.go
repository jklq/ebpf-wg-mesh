//go:build !linux

package continuity

import (
	"fmt"
	"os/exec"
)

func nativeResources(cmd *exec.Cmd, l Launch) (func(), error) {
	if l.EnforceResources {
		return nil, fmt.Errorf("native continuity resource enforcement requires Linux")
	}
	return func() {}, nil
}
func storageAvailable(string, int64) error {
	return fmt.Errorf("native continuity storage inspection requires Linux")
}
