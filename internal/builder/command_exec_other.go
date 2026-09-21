//go:build !linux

package builder

import (
	"context"
	"os/exec"
)

// newBuildCommand starts a build child. Off Linux there is no process
// group; cancellation kills the direct child only.
func newBuildCommand(ctx context.Context, binary string, args []string) *exec.Cmd {
	return exec.CommandContext(ctx, binary, args...)
}
