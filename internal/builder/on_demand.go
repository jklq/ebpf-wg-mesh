package builder

import (
	"context"
	"errors"
	"fmt"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/isolation"
)

// onDemandExecutor owns no containerd connection, unpacked image or sandbox
// while idle. Every claimed build creates and releases its backend and daemon.
type onDemandExecutor struct {
	cfg  config.BuilderConfig
	open func(SandboxBackendConfig) (SandboxBackend, error)
}

func (e *onDemandExecutor) Name() string    { return ExecutorHardened }
func (e *onDemandExecutor) Isolating() bool { return true }
func (e *onDemandExecutor) Execute(ctx context.Context, spec ExecutionSpec) (ExecutionResult, error) {
	if err := validateExecutionSpec(spec); err != nil {
		return ExecutionResult{}, err
	}
	backendCfg, err := sandboxConfig(e.cfg)
	if err != nil {
		return ExecutionResult{}, err
	}
	backend, err := e.open(backendCfg)
	if err != nil {
		return ExecutionResult{}, err
	}
	defer backend.Close()
	if _, err := backend.ReapStale(ctx); err != nil {
		return ExecutionResult{}, err
	}
	return newHardenedExecutor(e.cfg.WorkDir, backend, e.cfg.Sandbox.BuildkitdBinary, backendCfg.Isolation, e.cfg.Sandbox.Nameservers).Execute(ctx, spec)
}
func (e *onDemandExecutor) RecoverStaleWorkspaces(ctx context.Context) (int, error) {
	count, err := recoverStaleWorkspaces(e.cfg.WorkDir)
	roots, rootsErr := reapStaleDaemonRoots(e.cfg.WorkDir)
	// Reap containerd objects before accepting further work. Open only for this
	// recovery pass when on-disk execution markers show leftover work.
	if count+roots == 0 {
		return 0, errors.Join(err, rootsErr)
	}
	backendCfg, cfgErr := sandboxConfig(e.cfg)
	if cfgErr != nil {
		return count + roots, errors.Join(err, rootsErr, cfgErr)
	}
	backend, backendErr := e.open(backendCfg)
	if backendErr != nil {
		return count + roots, errors.Join(err, rootsErr, backendErr)
	}
	defer backend.Close()
	reaped, reapErr := backend.ReapStale(ctx)
	return count + roots + reaped, errors.Join(err, rootsErr, reapErr)
}
func sandboxConfig(cfg config.BuilderConfig) (SandboxBackendConfig, error) {
	policy, err := isolation.PolicyFor(cfg.Sandbox.Isolation, cfg.Profile.IsProduction())
	if err != nil {
		return SandboxBackendConfig{}, fmt.Errorf("builder.sandbox.isolation: %w", err)
	}
	return SandboxBackendConfig{Socket: cfg.Sandbox.Socket, Namespace: cfg.Sandbox.Namespace, Image: cfg.Sandbox.Image, Isolation: policy, Snapshotter: cfg.Sandbox.Snapshotter, CNIPluginDir: cfg.Sandbox.CNIPluginDir, CNIConfDir: cfg.Sandbox.CNIConfDir, CNINetwork: cfg.Sandbox.CNINetwork, Nameservers: cfg.Sandbox.Nameservers, BuildkitdBinary: cfg.Sandbox.BuildkitdBinary, WorkDir: cfg.WorkDir}, nil
}
