package builder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

// developmentExecutor is the in-process BuildExecutor: it runs the
// railpack and buildctl/docker build steps as host child processes
// with explicit environments, process limits, and a scoped docker
// config. It does not isolate hostile code: the BuildKit daemon is
// shared host state, network policy is not enforced on the data plane,
// and process limits bound only direct children. Production uses the
// hardened executor.
type developmentExecutor struct {
	workDir string
	runner  commandRunner
	pathEnv string
}

func newDevelopmentExecutor(workDir string, runner commandRunner, pathEnv string) *developmentExecutor {
	return &developmentExecutor{
		workDir: workDir,
		runner:  runner,
		pathEnv: pathEnv,
	}
}

func (e *developmentExecutor) Name() string { return ExecutorDevelopment }

func (e *developmentExecutor) Isolating() bool { return false }

func (e *developmentExecutor) Execute(ctx context.Context, spec ExecutionSpec) (ExecutionResult, error) {
	if err := validateExecutionSpec(spec); err != nil {
		return ExecutionResult{}, err
	}
	limits := spec.Limits.ProcessLimits()
	execCtx, cancel := context.WithTimeout(ctx, spec.Limits.Timeout)
	defer cancel()

	workspace, err := prepareExecutionWorkspace(e.workDir, spec.BuildID)
	if err != nil {
		return ExecutionResult{}, &buildFailureError{kind: failureKindBuild, err: err}
	}
	if spec.Cleanup {
		defer func() {
			if err := destroyExecutionWorkspace(workspace.root); err != nil {
				slog.Warn("destroy build workspace", "build_id", spec.BuildID, "error", err)
			}
		}()
	} else {
		defer os.Remove(workspace.markerPath)
	}

	slog.InfoContext(ctx, "build execution started",
		"build_id", spec.BuildID,
		"executor", e.Name(),
		"snapshot", spec.SnapshotDigest,
		"timeout", spec.Limits.Timeout.String(),
		"cache_mode", string(spec.Cache.Mode),
		"cache_key", spec.Cache.Key,
		"allow_egress", spec.Network.AllowGeneralEgress,
		"denied_cidrs", strings.Join(spec.Network.DeniedCIDRs, ","),
	)

	if err := materializeSnapshot(spec, workspace.repoDir); err != nil {
		return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, err)
	}
	report := func(line commandOutputLine) {
		if spec.OnLog != nil {
			spec.OnLog(line)
		}
	}
	buildCtx, stopBuild := context.WithCancel(execCtx)
	monitorDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-buildCtx.Done():
				monitorDone <- nil
				return
			case <-ticker.C:
				if err := enforceWorkspaceDiskLimit(workspace.root, spec.Limits.MaxWorkspaceBytes); err != nil {
					monitorDone <- err
					stopBuild()
					return
				}
			}
		}
	}()
	ref, err := e.invokeBuild(buildCtx, spec, workspace, report, limits)
	stopBuild()
	if limitErr := <-monitorDone; limitErr != nil {
		return ExecutionResult{}, &buildFailureError{kind: failureKindBuild, err: limitErr}
	}
	if err != nil {
		return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, err)
	}
	if err := enforceWorkspaceDiskLimit(workspace.root, spec.Limits.MaxWorkspaceBytes); err != nil {
		return ExecutionResult{}, &buildFailureError{kind: failureKindBuild, err: err}
	}
	return ExecutionResult{ImageDigestRef: ref}, nil
}

// RecoverStaleWorkspaces reclaims workspaces left behind by dead
// workers and verifies each removal.
func (e *developmentExecutor) RecoverStaleWorkspaces(_ context.Context) (int, error) {
	return recoverStaleWorkspaces(e.workDir)
}

func (e *developmentExecutor) invokeBuild(ctx context.Context, spec ExecutionSpec, workspace executionWorkspace, report func(commandOutputLine), limits ProcessLimits) (string, error) {
	switch spec.Recipe.GetBuilder() {
	case platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE:
		return e.invokeDockerfileBuild(ctx, spec, workspace, report, limits)
	case platformv1.BuilderKind_BUILDER_KIND_RAILPACK:
		return e.invokeRailpackBuild(ctx, spec, workspace, report, limits)
	default:
		return "", &buildFailureError{kind: failureKindBuild, err: errors.New("build recipe builder is required: railpack or dockerfile")}
	}
}

func (e *developmentExecutor) invokeDockerfileBuild(ctx context.Context, spec ExecutionSpec, workspace executionWorkspace, report func(commandOutputLine), limits ProcessLimits) (string, error) {
	contextDir, dockerfilePath, err := validateBuildInputs(workspace.repoDir, spec.Recipe)
	if err != nil {
		return "", &buildFailureError{kind: failureKindBuild, err: err}
	}
	configDir, cleanup, err := scopedDockerConfig(workspace.scratchDir, spec.Push.Reference, spec.Push.Username, spec.Push.Password, true)
	if err != nil {
		return "", &buildFailureError{kind: failureKindPush, err: err}
	}
	defer cleanup()

	req := buildCommand(spec.Buildkit.Binary, spec.Buildkit.Address, contextDir, workspace.repoDir, dockerfilePath, spec.Push.Reference, workspace.metadataFile, e.buildEnv(workspace, configDir))
	req.Limits = &limits
	output, err := e.runner.Run(ctx, req, report)
	if err != nil {
		return "", classifyBuildctlFailure(req, err, output)
	}
	return buildDigestRefFromMetadata(spec.Push.Reference, workspace.metadataFile)
}

func (e *developmentExecutor) invokeRailpackBuild(ctx context.Context, spec ExecutionSpec, workspace executionWorkspace, report func(commandOutputLine), limits ProcessLimits) (string, error) {
	appDir, err := resolveRepoPath(workspace.repoDir, spec.Recipe.GetContextDir(), true)
	if err != nil {
		return "", &buildFailureError{kind: failureKindBuild, err: fmt.Errorf("context dir: %w", err)}
	}
	planPath := filepath.Join(workspace.planDir, railpackPlanFilename)

	planReq := railpackPlanCommand(spec.Railpack.Binary, appDir, planPath)
	planReq.Env = e.buildEnv(workspace, "")
	planReq.Limits = &limits
	output, err := e.runner.Run(ctx, planReq, report)
	if err != nil {
		return "", railpackPlanFailure(appDir, spec.Recipe.GetContextDir(), planReq, err, output)
	}
	if info, err := os.Stat(planPath); err != nil || info.Size() == 0 {
		return "", &buildFailureError{kind: failureKindBuild, err: errors.New("railpack plan succeeded but wrote no build plan")}
	}

	configDir, cleanup, err := scopedDockerConfig(workspace.scratchDir, spec.Push.Reference, spec.Push.Username, spec.Push.Password, true)
	if err != nil {
		return "", &buildFailureError{kind: failureKindPush, err: err}
	}
	defer cleanup()

	req := railpackBuildCommand(spec.Buildkit.Binary, spec.Buildkit.Address, spec.Railpack.FrontendImage, appDir, workspace.planDir, planPath, spec.Push.Reference, workspace.metadataFile, e.buildEnv(workspace, configDir))
	req.Limits = &limits
	output, err = e.runner.Run(ctx, req, report)
	if err != nil {
		return "", classifyBuildctlFailure(req, err, output)
	}
	return buildDigestRefFromMetadata(spec.Push.Reference, workspace.metadataFile)
}

// buildEnv returns the complete explicit environment for a build
// child: a fixed PATH, a HOME and TMPDIR inside the execution
// workspace, and the scoped docker config when one is present. Proxy
// variables and every other ambient variable are deliberately absent.
func (e *developmentExecutor) buildEnv(workspace executionWorkspace, dockerConfigDir string) []string {
	env := []string{
		"PATH=" + e.pathEnv,
		"HOME=" + workspace.scratchDir,
		"TMPDIR=" + workspace.tmpDir,
	}
	if strings.TrimSpace(dockerConfigDir) != "" {
		env = append(env, "DOCKER_CONFIG="+dockerConfigDir)
	}
	return env
}
