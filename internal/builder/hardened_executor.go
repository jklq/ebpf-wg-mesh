package builder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

// prepareDaemonRoot creates the per-execution buildkitd root outside
// the workspace with an owner marker, clearing any leftover from a
// dead run first. It returns the root and the build's daemon
// directory.
func prepareDaemonRoot(workDir, buildID string) (root, buildDir string, err error) {
	roots, err := safeChildPath(workDir, sandboxDaemonRootsDirName)
	if err != nil {
		return "", "", err
	}
	buildDir, err = safeChildPath(roots, buildID)
	if err != nil {
		return "", "", err
	}
	if err := os.RemoveAll(buildDir); err != nil {
		return "", "", fmt.Errorf("clear stale daemon root: %w", err)
	}
	root, err = safeChildPath(buildDir, "root")
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", "", fmt.Errorf("mkdir daemon root: %w", err)
	}
	owner := executorOwner{
		PID:     os.Getpid(),
		BuildID: buildID,
	}
	data, err := json.Marshal(owner)
	if err != nil {
		return "", "", fmt.Errorf("marshal daemon root owner: %w", err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, executorOwnerMarker), data, 0o644); err != nil {
		return "", "", fmt.Errorf("write daemon root owner: %w", err)
	}
	return root, buildDir, nil
}

// reapStaleDaemonRoots removes per-execution daemon roots whose owner
// marker names a dead worker.
func reapStaleDaemonRoots(workDir string) (int, error) {
	roots, err := safeChildPath(workDir, sandboxDaemonRootsDirName)
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(roots)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("list daemon roots: %w", err)
	}
	var (
		reclaimed int
		failures  []string
	)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(roots, entry.Name())
		owner, ok, err := readExecutorOwner(filepath.Join(dir, executorOwnerMarker))
		if err != nil || !ok {
			if err != nil {
				slog.Warn("read stale daemon root marker", "dir", dir, "error", err)
			}
			continue
		}
		if processAlive(owner.PID) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			failures = append(failures, dir+": "+err.Error())
			continue
		}
		reclaimed++
		slog.Info("reclaimed stale daemon root", "dir", dir, "build_id", owner.BuildID)
	}
	if len(failures) > 0 {
		return reclaimed, fmt.Errorf("reclaim stale daemon roots: %s", strings.Join(failures, "; "))
	}
	return reclaimed, nil
}

const (
	// sandboxPathEnv is the fixed PATH inside build sandboxes. Build
	// tool binaries resolve from the sandbox image, never the host.
	sandboxPathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	sandboxGuestCache   = "/build/cache"
	sandboxGuestScratch = "/build/scratch"
	sandboxGuestTmp     = "/build/tmp"

	// sandboxSocketDirName is the workspace child holding the
	// per-execution buildkitd socket. Only the socket lives in the
	// workspace (the sandboxed buildctl client needs it there); the
	// daemon root lives outside the workspace (see
	// sandboxDaemonRootsDirName) so daemon state and disk use are
	// neither visible to the hostile build nor counted against the
	// execution's disk budget. The name stays short because Unix
	// socket paths are limited to 108 bytes.
	sandboxSocketDirName = "s"

	// sandboxDaemonRootsDirName is the work-dir child holding
	// per-execution buildkitd roots. Each build gets
	// <workdir>/bk/<buildID>/ with an owner marker for stale
	// recovery; the sandbox never mounts it.
	sandboxDaemonRootsDirName = "bk"
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
	startDaemon     func(ctx context.Context, netnsPath, binary, sockPath, rootDir string, env []string) (daemonProc, error)
}

func newHardenedExecutor(workDir string, backend SandboxBackend, buildkitdBinary string, nameservers []string) *hardenedExecutor {
	return &hardenedExecutor{
		workDir:         workDir,
		backend:         backend,
		buildkitdBinary: buildkitdBinary,
		nameservers:     nameservers,
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
	cacheDir, err := resolveExecutionCacheDir(e.workDir, spec)
	if err != nil {
		return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, &buildFailureError{kind: failureKindBuild, err: err})
	}
	if cacheDir != "" {
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, &buildFailureError{kind: failureKindBuild, err: fmt.Errorf("mkdir build cache dir: %w", err)})
		}
	}
	// The content cache is shared across builds, so only its growth
	// during this build is attributable to it. Measure before the
	// daemon starts; the post-build check charges the delta.
	var cacheBytesBefore int64
	if cacheDir != "" {
		if cacheBytesBefore, err = dirBytes(cacheDir); err != nil {
			return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, &buildFailureError{kind: failureKindBuild, err: fmt.Errorf("account build cache bytes: %w", err)})
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

	socketDir, err := safeChildPath(workspace.root, sandboxSocketDirName)
	if err != nil {
		return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, &buildFailureError{kind: failureKindBuild, err: err})
	}
	if err := os.MkdirAll(socketDir, 0o755); err != nil {
		return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, &buildFailureError{kind: failureKindBuild, err: fmt.Errorf("mkdir daemon socket dir: %w", err)})
	}
	daemonRoot, daemonBuildDir, err := prepareDaemonRoot(e.workDir, spec.BuildID)
	if err != nil {
		return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, &buildFailureError{kind: failureKindBuild, err: err})
	}
	if spec.Cleanup {
		defer func() {
			if err := os.RemoveAll(daemonBuildDir); err != nil {
				slog.Warn("remove per-execution daemon root", "build_id", spec.BuildID, "error", err)
			}
		}()
	}
	sockHost := filepath.Join(socketDir, "bk.sock")
	rootHost := daemonRoot
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
	if err := enforceHardenedDiskLimit(workspace.root, daemonBuildDir, cacheDir, cacheBytesBefore, spec.Limits.MaxWorkspaceBytes); err != nil {
		return ExecutionResult{}, &buildFailureError{kind: failureKindBuild, err: err}
	}
	return ExecutionResult{ImageDigestRef: ref}, nil
}

// enforceHardenedDiskLimit accounts every byte attributable to the
// build against the disk budget: the workspace, the per-execution
// daemon root, and the content-cache growth during the build. The
// daemon root is fully attributable (it is cleared before the build),
// while only the cache delta is charged — pre-existing cache bytes
// were written by earlier builds with identical content. Concurrent
// builds sharing one cache key may each observe the shared growth;
// that fails closed in the safe direction.
func enforceHardenedDiskLimit(workspaceRoot, daemonBuildDir, cacheDir string, cacheBytesBefore, maxBytes int64) error {
	total, err := dirBytes(workspaceRoot)
	if err != nil {
		return fmt.Errorf("account build workspace bytes: %w", err)
	}
	daemonBytes, err := dirBytes(daemonBuildDir)
	if err != nil {
		return fmt.Errorf("account per-execution daemon bytes: %w", err)
	}
	total += daemonBytes
	if cacheDir != "" {
		cacheAfter, err := dirBytes(cacheDir)
		if err != nil {
			return fmt.Errorf("account build cache bytes: %w", err)
		}
		if delta := cacheAfter - cacheBytesBefore; delta > 0 {
			total += delta
		}
	}
	if total > maxBytes {
		return fmt.Errorf("build used %d bytes (workspace, daemon, and cache growth), exceeding the %d byte limit", total, maxBytes)
	}
	return nil
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

// RecoverStaleWorkspaces reclaims workspaces, daemon roots, and
// sandboxes left behind by dead workers and verifies each removal.
func (e *hardenedExecutor) RecoverStaleWorkspaces(ctx context.Context) (int, error) {
	reclaimed, err := recoverStaleWorkspaces(e.workDir)
	roots, rootsErr := reapStaleDaemonRoots(e.workDir)
	reaped, backendErr := e.backend.ReapStale(ctx)
	return reclaimed + roots + reaped, errors.Join(err, rootsErr, backendErr)
}
