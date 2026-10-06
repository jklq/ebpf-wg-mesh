package builder

import (
	"context"
	"ebof-wg-mesh/internal/config"
	"fmt"
	"testing"
)

func TestHardenedExecutorDoesNotOpenBackendWhileIdle(t *testing.T) {
	cfg := config.BuilderConfig{Executor: ExecutorHardened, WorkDir: t.TempDir()}
	executor, err := newExecutorForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	lazy := executor.(*onDemandExecutor)
	opens := 0
	lazy.open = func(SandboxBackendConfig) (SandboxBackend, error) { opens++; return nil, fmt.Errorf("backend probe") }
	if _, err := lazy.RecoverStaleWorkspaces(context.Background()); err != nil {
		t.Fatal(err)
	}
	if opens != 0 {
		t.Fatal("idle builder opened sandbox backend")
	}
	// Invalid execution input must not create a sandbox or daemon either.
	if _, err := lazy.Execute(context.Background(), ExecutionSpec{}); err == nil {
		t.Fatal("invalid spec accepted")
	}
	if opens != 0 {
		t.Fatal(opens)
	}
}
