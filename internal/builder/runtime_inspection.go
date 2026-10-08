package builder

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"ebof-wg-mesh/internal/config"
)

// InspectRuntime executes the real selected toolchain with native network and
// cgroup containment. An idle gRPC connection is insufficient build readiness.
func InspectRuntime(ctx context.Context, cfg config.BuilderConfig) error {
	if err := VerifyRuntimeImage(ctx, cfg, cfg.Sandbox.Image); err != nil {
		return err
	}
	if err := exec.CommandContext(ctx, cfg.Sandbox.BuildkitdBinary, "--version").Run(); err != nil {
		return fmt.Errorf("native BuildKit executable unavailable: %w", err)
	}
	b, err := NewSandboxBackend(sandboxConfig(cfg))
	if err != nil {
		return err
	}
	defer b.Close()
	dir, err := os.MkdirTemp(cfg.WorkDir, ".runtime-inspection-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	id := "runtime-inspection-" + hex.EncodeToString(salt)
	policy := NetworkPolicy{AllowGeneralEgress: true, DeniedCIDRs: cfg.Network.DeniedCIDRs}
	n, err := b.SetupNet(ctx, id, policy)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = b.TeardownNet(cleanup, n)
	}()
	for _, tool := range []string{cfg.BuildctlBinary, cfg.RailpackBinary} {
		if err := b.RunStep(ctx, n, SandboxStep{Name: filepath.Base(tool), Argv: []string{tool, "--version"}, Env: []string{"PATH=" + sandboxPathEnv}, Dir: sandboxBuildRoot, Mounts: []SandboxMount{{Source: dir, Dest: sandboxBuildRoot}}, Limits: ResourceLimits{Timeout: time.Minute, MemoryBytes: 512 << 20, CPUSeconds: 30, MaxFileBytes: 1 << 20, MaxProcesses: 128, MaxWorkspaceBytes: 1 << 20}}); err != nil {
			return fmt.Errorf("sandbox tool %s unavailable: %w", tool, err)
		}
	}
	return nil
}
