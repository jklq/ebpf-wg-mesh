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

const (
	// sandboxPathEnv is the fixed PATH inside build sandboxes. Build
	// tool binaries resolve from the sandbox image, never the host.
	sandboxPathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	sandboxGuestCache   = "/build/cache"
	sandboxGuestScratch = "/build/scratch"
	sandboxGuestTmp     = "/build/tmp"

	// sandboxDaemonDirName is the workspace child holding the
	// per-execution buildkitd socket and root. It stays short
	// because Unix socket paths are limited to 108 bytes.
	sandboxDaemonDirName = "s"
)

// daemonProc is a started per-execution BuildKit daemon.
type daemonProc interface {
	waitReady(ctx context.Context) error
	Stop() error
}

// hardenedExecutor is the production BuildExecutor: every build step
// runs inside a one-shot sandbox with private namespaces, zero
// capabilities, cgroup limits, and enforced egress policy, against a
// per-execution BuildKit daemon with isolated state. It holds against
// hostile builds rather than merely uncooperative ones.
type hardenedExecutor struct {
	workDir         string
	backend         SandboxBackend
	buildkitdBinary string
	nameservers     []string
	now             func() time.Time
	startDaemon     func(ctx context.Context, netnsPath, binary, sockPath, rootDir string, env []string) (daemonProc, error)
}

func newHardenedExecutor(workDir string, backend SandboxBackend, buildkitdBinary string, nameservers []string) *hardenedExecutor {
	return &hardenedExecutor{
		workDir:         workDir,
		backend:         backend,
		buildkitdBinary: buildkitdBinary,
		nameservers:     nameservers,
		now:             time.Now,
		startDaemon:     defaultStartBuildkitd,
	}
}

// defaultStartBuildkitd adapts the platform buildkitd starter to the
// executor's daemon seam.
func defaultStartBuildkitd(ctx context.Context, netnsPath, binary, sockPath, rootDir string, env []string) (daemonProc, error) {
	return startBuildkitd(ctx, netnsPath, binary, sockPath, rootDir, env)
}

func (e *hardenedExecutor) Name() string { return ExecutorHardened }

func (e *hardenedExecutor) Isolating() bool { return true }

func (e *hardenedExecutor) Execute(ctx context.Context, spec ExecutionSpec) (ExecutionResult, error) {
	if err := validateExecutionSpec(spec); err != nil {
		return ExecutionResult{}, err
	}
	if isDockerBuildBinary(spec.Buildkit.Binary) {
		return ExecutionResult{}, &buildFailureError{kind: failureKindBuild, err: errors.New("hardened executor requires buildctl: docker buildx shares ambient host builder state")}
	}
	execCtx, cancel := context.WithTimeout(ctx, spec.Limits.Timeout)
	defer cancel()

	workspace, err := prepareExecutionWorkspace(e.workDir, spec.BuildID, ExecutorHardened, e.now)
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
	cacheDir, err := resolveExecutionCacheDir(e.workDir, spec)
	if err != nil {
		return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, &buildFailureError{kind: failureKindBuild, err: err})
	}
	if cacheDir != "" {
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, &buildFailureError{kind: failureKindBuild, err: fmt.Errorf("mkdir build cache dir: %w", err)})
		}
	}
	// No host tool mirroring: no host path may enter the sandbox
	// through the config directory.
	configDir, cleanup, err := scopedDockerConfig(workspace.scratchDir, spec.Push.Reference, spec.Push.Username, spec.Push.Password, false)
	if err != nil {
		return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, &buildFailureError{kind: failureKindPush, err: err})
	}
	defer cleanup()
	resolvHost, hostsHost, err := e.renderSandboxIdentity(workspace, spec.Network)
	if err != nil {
		return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, &buildFailureError{kind: failureKindBuild, err: err})
	}

	sandboxNet, err := e.backend.SetupNet(execCtx, spec.BuildID, spec.Network)
	if err != nil {
		return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, &buildFailureError{kind: failureKindBuild, err: err})
	}
	defer func() {
		teardownCtx, teardownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer teardownCancel()
		if err := e.backend.TeardownNet(teardownCtx, sandboxNet); err != nil {
			slog.Warn("teardown sandbox network", "build_id", spec.BuildID, "error", err)
		}
	}()

	daemonDir, err := safeChildPath(workspace.root, sandboxDaemonDirName)
	if err != nil {
		return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, &buildFailureError{kind: failureKindBuild, err: err})
	}
	sockHost := filepath.Join(daemonDir, "bk.sock")
	rootHost := filepath.Join(daemonDir, "root")
	daemon, err := e.startDaemon(execCtx, sandboxNet.Path, e.buildkitdBinary, sockHost, rootHost,
		[]string{"PATH=" + sandboxPathEnv, "HOME=" + rootHost, "TMPDIR=" + workspace.tmpDir})
	if err != nil {
		return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, &buildFailureError{kind: failureKindBuild, err: err})
	}
	defer func() {
		if err := daemon.Stop(); err != nil {
			slog.Warn("stop per-execution buildkitd", "build_id", spec.BuildID, "error", err)
		}
	}()
	if err := daemon.waitReady(execCtx); err != nil {
		if failure, ok := err.(*buildFailureError); ok {
			return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, failure)
		}
		if execCtx.Err() != nil {
			return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, &buildFailureError{kind: failureKindBuild, err: err})
		}
		return ExecutionResult{}, &buildFailureError{kind: failureKindBuild, err: err}
	}
	guestSock, err := guestWorkspacePath(workspace.root, sockHost)
	if err != nil {
		return ExecutionResult{}, &buildFailureError{kind: failureKindBuild, err: err}
	}
	// The hardened executor ignores the shared BuildKit address from
	// the spec: every build gets its own daemon on a per-execution
	// socket, so sibling builds share no daemon state.
	buildkitAddr := "unix://" + guestSock

	mounts := []SandboxMount{
		{Source: workspace.root, Dest: sandboxBuildRoot},
		{Source: workspace.repoDir, Dest: sandboxBuildRoot + "/repo", ReadOnly: true},
	}
	if cacheDir != "" {
		mounts = append(mounts, SandboxMount{Source: cacheDir, Dest: sandboxGuestCache})
	}
	mounts = append(mounts,
		SandboxMount{Source: resolvHost, Dest: "/etc/resolv.conf", ReadOnly: true},
		SandboxMount{Source: hostsHost, Dest: "/etc/hosts", ReadOnly: true},
	)
	report := func(line commandOutputLine) {
		if spec.OnLog != nil {
			spec.OnLog(line)
		}
	}
	ref, err := e.invokeBuild(execCtx, spec, workspace, sandboxNet, buildkitAddr, configDir, mounts, report)
	if err != nil {
		return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, err)
	}
	if err := enforceWorkspaceDiskLimit(workspace.root, spec.Limits.MaxWorkspaceBytes); err != nil {
		return ExecutionResult{}, &buildFailureError{kind: failureKindBuild, err: err}
	}
	return ExecutionResult{ImageDigestRef: ref}, nil
}

// renderSandboxIdentity writes the resolver files bind-mounted into
// sandboxes. They carry only resolver addresses and loopback names:
// no host identity leaks in.
func (e *hardenedExecutor) renderSandboxIdentity(workspace executionWorkspace, policy NetworkPolicy) (resolvHost, hostsHost string, err error) {
	hostResolv, _ := os.ReadFile("/etc/resolv.conf")
	rendered, err := renderSandboxResolvConf(hostResolv, e.nameservers, policy.AllowGeneralEgress)
	if err != nil {
		return "", "", err
	}
	resolvHost = filepath.Join(workspace.scratchDir, "sandbox-resolv.conf")
	if err := os.WriteFile(resolvHost, rendered, 0o644); err != nil {
		return "", "", fmt.Errorf("write sandbox resolv.conf: %w", err)
	}
	hostsHost = filepath.Join(workspace.scratchDir, "sandbox-hosts")
	if err := os.WriteFile(hostsHost, renderSandboxHostsFile(), 0o644); err != nil {
		return "", "", fmt.Errorf("write sandbox hosts file: %w", err)
	}
	return resolvHost, hostsHost, nil
}

// resolveExecutionCacheDir verifies the content-addressed cache key
// and returns its host directory. The key must exactly equal the key
// derived from build content (snapshot digest, recipe, toolchain):
// any caller-supplied project, service, or build identity in the key
// is refused, so cache mounts can never carry state between projects.
func resolveExecutionCacheDir(workDir string, spec ExecutionSpec) (string, error) {
	if spec.Cache.Mode == CacheModeNone {
		return "", nil
	}
	expected := ContentCacheKey(spec.SnapshotDigest, spec.Recipe, spec.Railpack.FrontendImage)
	if spec.Cache.Key != expected {
		return "", errors.New("content-addressed cache key does not match build content: refusing project-keyed cache")
	}
	// The key matched our own derivation, so the digest suffix is a
	// fixed 64 hex characters; safeChildPath contains it regardless.
	digest := strings.TrimPrefix(expected, "sha256:")
	cacheRoot, err := safeChildPath(workDir, "cache")
	if err != nil {
		return "", err
	}
	return safeChildPath(cacheRoot, "cache-sha256-"+digest)
}

func (e *hardenedExecutor) invokeBuild(ctx context.Context, spec ExecutionSpec, workspace executionWorkspace, sandboxNet SandboxNet, buildkitAddr, dockerConfigDir string, mounts []SandboxMount, report func(commandOutputLine)) (string, error) {
	switch spec.Recipe.GetBuilder() {
	case platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE:
		return e.invokeDockerfileBuild(ctx, spec, workspace, sandboxNet, buildkitAddr, dockerConfigDir, mounts, report)
	case platformv1.BuilderKind_BUILDER_KIND_RAILPACK:
		return e.invokeRailpackBuild(ctx, spec, workspace, sandboxNet, buildkitAddr, dockerConfigDir, mounts, report)
	default:
		return "", &buildFailureError{kind: failureKindBuild, err: errors.New("build recipe builder is required: railpack or dockerfile")}
	}
}

func (e *hardenedExecutor) invokeDockerfileBuild(ctx context.Context, spec ExecutionSpec, workspace executionWorkspace, sandboxNet SandboxNet, buildkitAddr, dockerConfigDir string, mounts []SandboxMount, report func(commandOutputLine)) (string, error) {
	contextDir, dockerfilePath, err := validateBuildInputs(workspace.repoDir, spec.Recipe)
	if err != nil {
		return "", &buildFailureError{kind: failureKindBuild, err: err}
	}
	guest, err := e.guestBuildPaths(workspace, dockerConfigDir, contextDir)
	if err != nil {
		return "", &buildFailureError{kind: failureKindBuild, err: err}
	}
	req := buildCommand(spec.Buildkit.Binary, buildkitAddr, guest.contextDir, guest.repoDir, dockerfilePath, spec.Push.Reference, guest.metadataFile, e.sandboxEnv(guest.dockerConfigDir))
	e.appendCacheFlags(spec, &req)
	if err := e.runSandboxStep(ctx, sandboxNet, spec, mounts, "build", req, report); err != nil {
		return "", err
	}
	return buildDigestRefFromMetadata(spec.Push.Reference, workspace.metadataFile)
}

func (e *hardenedExecutor) invokeRailpackBuild(ctx context.Context, spec ExecutionSpec, workspace executionWorkspace, sandboxNet SandboxNet, buildkitAddr, dockerConfigDir string, mounts []SandboxMount, report func(commandOutputLine)) (string, error) {
	appDir, err := resolveRepoPath(workspace.repoDir, spec.Recipe.GetContextDir(), true)
	if err != nil {
		return "", &buildFailureError{kind: failureKindBuild, err: fmt.Errorf("context dir: %w", err)}
	}
	planHost := filepath.Join(workspace.planDir, railpackPlanFilename)
	guest, err := e.guestBuildPaths(workspace, dockerConfigDir, appDir)
	if err != nil {
		return "", &buildFailureError{kind: failureKindBuild, err: err}
	}
	guestPlan, err := guestWorkspacePath(workspace.root, planHost)
	if err != nil {
		return "", &buildFailureError{kind: failureKindBuild, err: err}
	}

	planReq := railpackPlanCommand(spec.Railpack.Binary, guest.contextDir, guestPlan)
	planReq.Env = e.sandboxEnv("")
	if err := e.runSandboxStep(ctx, sandboxNet, spec, mounts, "plan", planReq, report); err != nil {
		return "", railpackPlanFailure(appDir, spec.Recipe.GetContextDir(), planReq, err, nil)
	}
	if info, err := os.Stat(planHost); err != nil || info.Size() == 0 {
		return "", &buildFailureError{kind: failureKindBuild, err: errors.New("railpack plan succeeded but wrote no build plan")}
	}

	guestPlanDir, err := guestWorkspacePath(workspace.root, workspace.planDir)
	if err != nil {
		return "", &buildFailureError{kind: failureKindBuild, err: err}
	}
	req := railpackBuildCommand(spec.Buildkit.Binary, buildkitAddr, spec.Railpack.FrontendImage, guest.contextDir, guestPlanDir, guestPlan, spec.Push.Reference, guest.metadataFile, e.sandboxEnv(guest.dockerConfigDir))
	e.appendCacheFlags(spec, &req)
	if err := e.runSandboxStep(ctx, sandboxNet, spec, mounts, "build", req, report); err != nil {
		return "", err
	}
	return buildDigestRefFromMetadata(spec.Push.Reference, workspace.metadataFile)
}

// guestBuildPaths translates the host paths embedded in build commands
// to their guest paths inside the sandbox.
type guestBuildPathsResult struct {
	contextDir      string
	repoDir         string
	metadataFile    string
	dockerConfigDir string
}

func (e *hardenedExecutor) guestBuildPaths(workspace executionWorkspace, dockerConfigDir, contextDir string) (guestBuildPathsResult, error) {
	var (
		guest guestBuildPathsResult
		err   error
	)
	if guest.contextDir, err = guestWorkspacePath(workspace.root, contextDir); err != nil {
		return guest, err
	}
	if guest.repoDir, err = guestWorkspacePath(workspace.root, workspace.repoDir); err != nil {
		return guest, err
	}
	if guest.metadataFile, err = guestWorkspacePath(workspace.root, workspace.metadataFile); err != nil {
		return guest, err
	}
	if guest.dockerConfigDir, err = guestWorkspacePath(workspace.root, dockerConfigDir); err != nil {
		return guest, err
	}
	return guest, nil
}

// appendCacheFlags points the build at the execution's content-keyed
// cache dir. Import and export share the directory: it is the
// standard local-cache round trip, namespaced by content so it cannot
// carry state between projects.
func (e *hardenedExecutor) appendCacheFlags(spec ExecutionSpec, req *commandRequest) {
	if spec.Cache.Mode != CacheModeContentAddressed {
		return
	}
	req.Args = append(req.Args,
		"--export-cache", "type=local,dest="+sandboxGuestCache,
		"--import-cache", "type=local,src="+sandboxGuestCache,
	)
}

func (e *hardenedExecutor) runSandboxStep(ctx context.Context, sandboxNet SandboxNet, spec ExecutionSpec, mounts []SandboxMount, name string, req commandRequest, report func(commandOutputLine)) error {
	step := SandboxStep{
		Name:   name,
		Argv:   append([]string{req.Binary}, req.Args...),
		Env:    req.Env,
		Dir:    sandboxGuestScratch,
		Limits: spec.Limits,
		Mounts: mounts,
		OnLog:  report,
	}
	err := e.backend.RunStep(ctx, sandboxNet, step)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return &buildFailureError{kind: failureKindBuild, err: ctx.Err()}
	}
	var stepErr *SandboxStepError
	if errors.As(err, &stepErr) {
		return classifyBuildctlFailure(req, err, []byte(stepErr.Tail))
	}
	return &buildFailureError{kind: failureKindBuild, err: err}
}

// sandboxEnv returns the complete explicit environment for a sandboxed
// build step: a fixed PATH, HOME and TMPDIR under the guest build
// root, and the scoped docker config when one is present.
func (e *hardenedExecutor) sandboxEnv(dockerConfigGuest string) []string {
	env := []string{
		"PATH=" + sandboxPathEnv,
		"HOME=" + sandboxGuestScratch,
		"TMPDIR=" + sandboxGuestTmp,
	}
	if strings.TrimSpace(dockerConfigGuest) != "" {
		env = append(env, "DOCKER_CONFIG="+dockerConfigGuest)
	}
	return env
}

// RecoverStaleWorkspaces reclaims workspaces and sandboxes left
// behind by dead workers and verifies each removal.
func (e *hardenedExecutor) RecoverStaleWorkspaces(ctx context.Context) (int, error) {
	reclaimed, err := recoverStaleWorkspaces(e.workDir)
	reaped, backendErr := e.backend.ReapStale(ctx)
	return reclaimed + reaped, errors.Join(err, backendErr)
}
