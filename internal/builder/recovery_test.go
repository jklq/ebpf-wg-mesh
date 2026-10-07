package builder

import (
	"context"
	"testing"
)

func TestRecoveryPausePrecedesWorkspaceCleanupAndBuildClaims(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Nil dependencies fail if Run calls any executor, cleanup or claim path.
	app := &App{recoveryPaused: true}
	if err := app.Run(ctx); err != nil {
		t.Fatal(err)
	}
}
