package builder

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

// executorOwnerMarker is written into every execution workspace at
// start and removed when the execution finishes. A workspace that
// still carries it after a restart belongs to a dead worker and is
// reclaimed by RecoverStaleWorkspaces.
const executorOwnerMarker = ".executor-owner.json"

type executorOwner struct {
	Executor  string    `json:"executor"`
	PID       int       `json:"pid"`
	BuildID   string    `json:"build_id"`
	StartedAt time.Time `json:"started_at"`
}

// executionWorkspace is the executor-owned directory layout for one
// build. The repo directory holds the verified source snapshot and is
// made read-only; scratch, tmp, and plan are the isolated writable
// areas.
type executionWorkspace struct {
	root         string
	repoDir      string
	scratchDir   string
	tmpDir       string
	planDir      string
	metadataFile string
	markerPath   string
}

// developmentExecutor is the in-process BuildExecutor: it runs the
// railpack and buildctl/docker build steps as host child processes
// with explicit environments, process limits, and a scoped docker
// config. It establishes the execution seam but does not isolate
// hostile code: the BuildKit daemon is shared host state, build
// network policy is validated but not enforced on the data plane, and
// process limits bound only the executor's direct children. 2.4b adds
// the hardened backend.
type developmentExecutor struct {
	workDir string
	runner  commandRunner
	pathEnv string
	now     func() time.Time
}

func newDevelopmentExecutor(workDir string, runner commandRunner, pathEnv string) *developmentExecutor {
	return &developmentExecutor{
		workDir: workDir,
		runner:  runner,
		pathEnv: pathEnv,
		now:     time.Now,
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

	workspace, err := prepareExecutionWorkspace(e.workDir, spec.BuildID, e.now)
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

	if err := e.materializeSnapshot(spec, workspace.repoDir); err != nil {
		return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, err)
	}
	report := func(line commandOutputLine) {
		if spec.OnLog != nil {
			spec.OnLog(line)
		}
	}
	ref, err := e.invokeBuild(execCtx, spec, workspace, report, limits)
	if err != nil {
		return ExecutionResult{}, mapExecutionError(ctx, execCtx, spec, err)
	}
	if err := enforceWorkspaceDiskLimit(workspace.root, spec.Limits.MaxWorkspaceBytes); err != nil {
		return ExecutionResult{}, &buildFailureError{kind: failureKindBuild, err: err}
	}
	return ExecutionResult{ImageDigestRef: ref}, nil
}

// mapExecutionError translates context errors into executor errors: a
// cancelled parent context stays context.Canceled, and the executor's
// own deadline becomes ErrBuildTimeout. Cancellation and timeout win
// over a coincident build failure so callers can distinguish them.
func mapExecutionError(ctx, execCtx context.Context, spec ExecutionSpec, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return &buildFailureError{kind: failureKindBuild, err: ctx.Err()}
	}
	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		return &buildFailureError{kind: failureKindBuild, err: fmt.Errorf("%w after %s", ErrBuildTimeout, spec.Limits.Timeout)}
	}
	if failure, ok := err.(*buildFailureError); ok {
		return failure
	}
	return &buildFailureError{kind: failureKindBuild, err: err}
}

func validateExecutionSpec(spec ExecutionSpec) error {
	if strings.TrimSpace(spec.BuildID) == "" {
		return &buildFailureError{kind: failureKindBuild, err: errors.New("build id is required")}
	}
	if strings.TrimSpace(spec.SnapshotArchivePath) == "" {
		return &buildFailureError{kind: failureKindFetch, err: errors.New("verified source snapshot is required")}
	}
	if !validSnapshotDigest(spec.SnapshotDigest) {
		return &buildFailureError{kind: failureKindFetch, err: errors.New("source snapshot digest is invalid")}
	}
	if spec.Recipe == nil {
		return &buildFailureError{kind: failureKindBuild, err: errors.New("build recipe is required: railpack or dockerfile")}
	}
	switch spec.Recipe.GetBuilder() {
	case platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, platformv1.BuilderKind_BUILDER_KIND_RAILPACK:
	default:
		return &buildFailureError{kind: failureKindBuild, err: errors.New("build recipe builder is required: railpack or dockerfile")}
	}
	if err := validatePushCredentials(spec.Push); err != nil {
		return &buildFailureError{kind: failureKindPush, err: err}
	}
	if strings.TrimSpace(spec.Buildkit.Binary) == "" || strings.TrimSpace(spec.Buildkit.Address) == "" {
		return &buildFailureError{kind: failureKindBuild, err: errors.New("buildkit endpoint is required")}
	}
	if spec.Recipe.GetBuilder() == platformv1.BuilderKind_BUILDER_KIND_RAILPACK {
		if strings.TrimSpace(spec.Railpack.Binary) == "" || strings.TrimSpace(spec.Railpack.FrontendImage) == "" {
			return &buildFailureError{kind: failureKindBuild, err: errors.New("railpack toolchain is required")}
		}
	}
	if err := spec.Limits.Validate(); err != nil {
		return &buildFailureError{kind: failureKindBuild, err: err}
	}
	if err := spec.Network.Validate(); err != nil {
		return &buildFailureError{kind: failureKindBuild, err: err}
	}
	if err := spec.Cache.Validate(); err != nil {
		return &buildFailureError{kind: failureKindBuild, err: err}
	}
	return nil
}

func validSnapshotDigest(digest string) bool {
	digest = strings.TrimSpace(digest)
	return strings.HasPrefix(digest, "sha256:") && len(digest) == len("sha256:")+sha256.Size*2
}

// validatePushCredentials requires push credentials scoped to exactly
// the one repository named by the push reference.
func validatePushCredentials(push PushCredentials) error {
	host, repository, err := splitPushReference(push.Reference)
	if err != nil {
		return err
	}
	if strings.TrimSpace(push.Username) == "" || strings.TrimSpace(push.Password) == "" {
		return fmt.Errorf("push credentials for repository %q are required", repository)
	}
	if host == "" || repository == "" {
		return errors.New("push reference must name a registry host and repository")
	}
	return nil
}

// splitPushReference parses host and repository out of a tagged or
// digest-pinned push reference.
func splitPushReference(ref string) (host, repository string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.Contains(ref, "://") {
		return "", "", errors.New("push reference must name a registry host and repository")
	}
	host, rest, ok := strings.Cut(ref, "/")
	if !ok || strings.TrimSpace(host) == "" || strings.TrimSpace(rest) == "" {
		return "", "", errors.New("push reference must name a registry host and repository")
	}
	repository = rest
	if separator := strings.LastIndexByte(rest, '@'); separator >= 0 {
		repository = rest[:separator]
	} else if separator := strings.LastIndexByte(rest, ':'); separator > strings.LastIndexByte(rest, '/') {
		repository = rest[:separator]
	}
	if strings.TrimSpace(repository) == "" {
		return "", "", errors.New("push reference must name a registry host and repository")
	}
	return host, repository, nil
}

func prepareExecutionWorkspace(workDir, buildID string, now func() time.Time) (executionWorkspace, error) {
	root, err := safeChildPath(workDir, buildID)
	if err != nil {
		return executionWorkspace{}, err
	}
	// A leftover root from a dead worker must never be reused: a fresh
	// execution starts from an empty directory.
	if err := os.RemoveAll(root); err != nil {
		return executionWorkspace{}, fmt.Errorf("clear stale workspace: %w", err)
	}
	workspace := executionWorkspace{root: root, markerPath: filepath.Join(root, executorOwnerMarker)}
	for _, child := range []string{"repo", "scratch", "tmp", "plan"} {
		dir, err := safeChildPath(root, child)
		if err != nil {
			return executionWorkspace{}, err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return executionWorkspace{}, fmt.Errorf("mkdir workspace dir: %w", err)
		}
		switch child {
		case "repo":
			workspace.repoDir = dir
		case "scratch":
			workspace.scratchDir = dir
		case "tmp":
			workspace.tmpDir = dir
		case "plan":
			workspace.planDir = dir
		}
	}
	workspace.metadataFile = filepath.Join(root, "metadata.json")
	owner := executorOwner{
		Executor:  ExecutorDevelopment,
		PID:       os.Getpid(),
		BuildID:   buildID,
		StartedAt: now().UTC(),
	}
	data, err := json.Marshal(owner)
	if err != nil {
		return executionWorkspace{}, fmt.Errorf("marshal workspace owner: %w", err)
	}
	if err := os.WriteFile(workspace.markerPath, data, 0o644); err != nil {
		return executionWorkspace{}, fmt.Errorf("write workspace owner: %w", err)
	}
	return workspace, nil
}

// destroyExecutionWorkspace removes the workspace and verifies its
// removal. Write permission is restored first because the snapshot
// directory is read-only by design.
func destroyExecutionWorkspace(root string) error {
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(path, 0o755)
		} else {
			_ = os.Chmod(path, 0o644)
		}
		return nil
	})
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove build workspace: %w", err)
	}
	if _, err := os.Stat(root); err == nil {
		return errors.New("build workspace still exists after removal")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("verify build workspace removal: %w", err)
	}
	return nil
}

// RecoverStaleWorkspaces reclaims workspaces whose owner marker names
// a dead worker and verifies each removal. Workspaces without a
// marker are not executor executions and are left alone, as are
// markers owned by a live process.
func (e *developmentExecutor) RecoverStaleWorkspaces(_ context.Context) (int, error) {
	entries, err := os.ReadDir(e.workDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("list builder work dir: %w", err)
	}
	var (
		reclaimed int
		failures  []string
	)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		root := filepath.Join(e.workDir, entry.Name())
		owner, ok, err := readExecutorOwner(filepath.Join(root, executorOwnerMarker))
		if err != nil || !ok {
			if err != nil {
				slog.Warn("read stale workspace marker", "workspace", root, "error", err)
			}
			continue
		}
		if processAlive(owner.PID) {
			continue
		}
		if err := destroyExecutionWorkspace(root); err != nil {
			failures = append(failures, root+": "+err.Error())
			continue
		}
		reclaimed++
		slog.Info("reclaimed stale build workspace", "workspace", root, "build_id", owner.BuildID)
	}
	if len(failures) > 0 {
		return reclaimed, fmt.Errorf("reclaim stale build workspaces: %s", strings.Join(failures, "; "))
	}
	return reclaimed, nil
}

func readExecutorOwner(path string) (executorOwner, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return executorOwner{}, false, nil
		}
		return executorOwner{}, false, err
	}
	var owner executorOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return executorOwner{}, false, fmt.Errorf("parse workspace owner: %w", err)
	}
	return owner, true, nil
}

// materializeSnapshot re-verifies the staged archive digest and
// extracts it into a read-only snapshot directory.
func (e *developmentExecutor) materializeSnapshot(spec ExecutionSpec, repoDir string) error {
	info, err := os.Stat(spec.SnapshotArchivePath)
	if err != nil {
		return &buildFailureError{kind: failureKindFetch, err: fmt.Errorf("stat source snapshot: %w", err)}
	}
	if info.Size() <= 0 || info.Size() > maxSourceArchiveCompressedBytes {
		return &buildFailureError{kind: failureKindFetch, err: errors.New("snapshot archive exceeds compressed size limit")}
	}
	file, err := os.Open(spec.SnapshotArchivePath)
	if err != nil {
		return &buildFailureError{kind: failureKindFetch, err: fmt.Errorf("open source snapshot: %w", err)}
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return &buildFailureError{kind: failureKindFetch, err: fmt.Errorf("hash source snapshot: %w", err)}
	}
	actualDigest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actualDigest != strings.TrimSpace(spec.SnapshotDigest) {
		return &buildFailureError{kind: failureKindFetch, err: fmt.Errorf("snapshot digest verification failed: expected=%s actual=%s", spec.SnapshotDigest, actualDigest)}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return &buildFailureError{kind: failureKindFetch, err: fmt.Errorf("rewind source snapshot: %w", err)}
	}
	if err := extractSourceSnapshotReader(repoDir, file, info.Size()); err != nil {
		return &buildFailureError{kind: failureKindFetch, err: err}
	}
	if err := makeSnapshotReadOnly(repoDir); err != nil {
		return &buildFailureError{kind: failureKindFetch, err: err}
	}
	return nil
}

// makeSnapshotReadOnly removes write permission from the extracted
// snapshot and verifies that it cannot be written to.
func makeSnapshotReadOnly(repoDir string) error {
	if err := filepath.WalkDir(repoDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o444)
		if entry.IsDir() {
			mode = 0o555
		}
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("chmod snapshot path: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("make source snapshot read-only: %w", err)
	}
	probe, err := os.OpenFile(filepath.Join(repoDir, ".executor-write-probe"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		_ = probe.Close()
		_ = os.Remove(filepath.Join(repoDir, ".executor-write-probe"))
		return errors.New("source snapshot is writable after read-only enforcement")
	}
	return nil
}

// enforceWorkspaceDiskLimit accounts executor-managed workspace bytes
// against the execution disk limit.
func enforceWorkspaceDiskLimit(root string, maxBytes int64) error {
	var total int64
	if err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	}); err != nil {
		return fmt.Errorf("account build workspace bytes: %w", err)
	}
	if total > maxBytes {
		return fmt.Errorf("build workspace used %d bytes, exceeding the %d byte limit", total, maxBytes)
	}
	return nil
}

// scopedDockerConfig writes a per-execution docker config holding the
// push credentials for exactly the one repository named by pushRef. It
// deliberately does not merge the host's docker config: ambient
// credentials must never enter a build. Only docker tool plugins are
// linked in so the docker CLI keeps working; docker contexts (ambient
// endpoint selection) are never linked.
func scopedDockerConfig(scratchDir, pushRef, username, password string) (dockerConfigDir string, cleanup func(), err error) {
	host, _, err := splitPushReference(pushRef)
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(scratchDir, "docker-config-*")
	if err != nil {
		return "", nil, fmt.Errorf("create docker config dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	if err := mirrorDockerToolSupport(dockerDefaultConfigDir(), dir); err != nil {
		cleanup()
		return "", nil, err
	}
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	configJSON, err := json.Marshal(map[string]any{
		"auths": map[string]any{
			host: map[string]any{"auth": auth},
		},
		"credHelpers": map[string]any{
			host: "",
		},
	})
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("marshal scoped docker config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), configJSON, 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write scoped docker config: %w", err)
	}
	return dir, cleanup, nil
}

func mirrorDockerToolSupport(srcDir, dstDir string) error {
	for _, name := range []string{"cli-plugins", "buildx"} {
		if err := symlinkIfExists(filepath.Join(srcDir, name), filepath.Join(dstDir, name)); err != nil {
			return err
		}
	}
	return nil
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
	configDir, cleanup, err := scopedDockerConfig(workspace.scratchDir, spec.Push.Reference, spec.Push.Username, spec.Push.Password)
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

	configDir, cleanup, err := scopedDockerConfig(workspace.scratchDir, spec.Push.Reference, spec.Push.Username, spec.Push.Password)
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
