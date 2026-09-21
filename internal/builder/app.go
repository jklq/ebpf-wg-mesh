package builder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/health"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/status"
)

const (
	failureKindFetch    = "fetch failure"
	failureKindBuild    = "build failure"
	failureKindPush     = "push failure"
	failureKindProtocol = "protocol/reporting failure"

	maxSourceArchiveCompressedBytes = 64 << 20
	maxSourceArchiveChunkBytes      = 64 << 10
	maxSourceArchiveExpandedBytes   = 1 << 30
	maxSourceArchiveFileBytes       = 256 << 20
	maxSourceArchiveEntries         = 100_000
)

type commandRequest struct {
	Dir    string
	Binary string
	// Env is the child's complete environment. The runner never
	// inherits the builder process environment: the executor supplies
	// every variable explicitly so ambient host state cannot leak
	// into a build.
	Env    []string
	Args   []string
	Limits *ProcessLimits
}

type commandRunner interface {
	Run(ctx context.Context, req commandRequest, onLine func(commandOutputLine)) ([]byte, error)
}

type buildFailureError struct {
	kind string
	err  error
}

func (e *buildFailureError) Error() string {
	if e == nil {
		return ""
	}
	return e.kind + ": " + e.err.Error()
}

func (e *buildFailureError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

type App struct {
	cfg        config.BuilderConfig
	conn       *grpc.ClientConn
	client     platformv1.BuilderServiceClient
	executor   BuildExecutor
	healthStop func(context.Context) error
}

func New(cfg config.BuilderConfig) (*App, error) {
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir builder work dir: %w", err)
	}
	executor, err := newExecutorForConfig(cfg)
	if err != nil {
		return nil, err
	}
	// Defense in depth: config validation already refuses the
	// development executor in production, but the builder must never
	// run untrusted builds without isolation even if validation is
	// bypassed.
	if cfg.Profile.IsProduction() && !executor.Isolating() {
		return nil, fmt.Errorf("builder executor %q does not isolate untrusted code: production requires the hardened executor", executor.Name())
	}
	creds, err := loadTransportCredentials(cfg.ControlPlane.TLS)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, cfg.ControlPlane.Address, grpc.WithTransportCredentials(creds), grpc.WithBlock())
	if err != nil {
		return nil, fmt.Errorf("dial control plane: %w", err)
	}
	return &App{
		cfg:      cfg,
		conn:     conn,
		client:   platformv1.NewBuilderServiceClient(conn),
		executor: executor,
	}, nil
}

// newExecutorForConfig selects the build executor backend. Config
// validation already rejects unknown executors; this fails closed
// anyway.
func newExecutorForConfig(cfg config.BuilderConfig) (BuildExecutor, error) {
	switch cfg.Executor {
	case "", ExecutorDevelopment:
		return newDevelopmentExecutor(cfg.WorkDir, osCommandRunner{}, os.Getenv("PATH")), nil
	case ExecutorHardened:
		backend, err := NewSandboxBackend(SandboxBackendConfig{
			Socket:          cfg.Sandbox.Socket,
			Namespace:       cfg.Sandbox.Namespace,
			Image:           cfg.Sandbox.Image,
			Runtime:         cfg.Sandbox.Runtime,
			Snapshotter:     cfg.Sandbox.Snapshotter,
			CNIPluginDir:    cfg.Sandbox.CNIPluginDir,
			CNIConfDir:      cfg.Sandbox.CNIConfDir,
			CNINetwork:      cfg.Sandbox.CNINetwork,
			Nameservers:     cfg.Sandbox.Nameservers,
			BuildkitdBinary: cfg.Sandbox.BuildkitdBinary,
			WorkDir:         cfg.WorkDir,
		})
		if err != nil {
			return nil, err
		}
		return newHardenedExecutor(cfg.WorkDir, backend, cfg.Sandbox.BuildkitdBinary, cfg.Sandbox.Nameservers), nil
	default:
		return nil, fmt.Errorf("unknown build executor %q", cfg.Executor)
	}
}

func (a *App) Close() error {
	if a == nil {
		return nil
	}
	if a.healthStop != nil {
		_ = a.healthStop(context.Background())
	}
	if a.conn == nil {
		return nil
	}
	return a.conn.Close()
}

func (a *App) readyReport(context.Context) health.Report {
	if a != nil && a.conn != nil && a.conn.GetState() == connectivity.Ready {
		return health.Report{Status: health.StatusReady}
	}
	return health.Report{Status: health.StatusNotReady, Failed: []string{"control_plane"}}
}

func (a *App) Run(ctx context.Context) error {
	if reclaimed, err := a.executor.RecoverStaleWorkspaces(ctx); err != nil {
		return fmt.Errorf("recover stale build workspaces: %w", err)
	} else if reclaimed > 0 {
		slog.Info("reclaimed stale build workspaces", "count", reclaimed)
	}
	if listen := strings.TrimSpace(a.cfg.Health.Listen); listen != "" {
		_, shutdown, err := health.ListenAndServe(ctx, listen, a.readyReport)
		if err != nil {
			return fmt.Errorf("listen health: %w", err)
		}
		a.healthStop = shutdown
	}
	pollInterval := time.Duration(a.cfg.PollIntervalSeconds) * time.Second
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		job, err := a.client.ClaimBuild(ctx, &platformv1.ClaimBuildRequest{
			BuilderId:   a.cfg.ID,
			BuilderName: a.cfg.Name,
		})
		if err != nil {
			return err
		}
		if job.GetBuildId() == "" {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(pollInterval):
				continue
			}
		}
		if err := a.executeJob(ctx, job); err != nil {
			slog.Warn("builder job failed", "build_id", job.GetBuildId(), "error", err)
		}
	}
}

func (a *App) executeJob(ctx context.Context, job *platformv1.BuildJob) error {
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	heartbeatDone := make(chan struct{})
	cancelledByControlPlane := make(chan struct{}, 1)
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(time.Duration(a.cfg.HeartbeatIntervalSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				_, err := a.client.ReportBuildHeartbeat(jobCtx, &platformv1.BuilderHeartbeatRequest{
					BuilderId:  a.cfg.ID,
					BuildId:    job.GetBuildId(),
					LeaseEpoch: job.GetLeaseEpoch(),
				})
				if err != nil {
					slog.Warn("report builder heartbeat", "build_id", job.GetBuildId(), "error", err)
					if code := status.Code(err); code == codes.PermissionDenied || code == codes.FailedPrecondition || code == codes.NotFound || code == codes.Canceled {
						select {
						case cancelledByControlPlane <- struct{}{}:
						default:
						}
						cancel()
						return
					}
				}
			}
		}
	}()

	imageRef, err := a.buildAndPush(jobCtx, job)
	cancel()
	<-heartbeatDone
	select {
	case <-cancelledByControlPlane:
		return nil
	default:
	}

	if err != nil {
		_, completeErr := a.client.CompleteBuild(ctx, &platformv1.CompleteBuildRequest{
			BuilderId:     a.cfg.ID,
			BuildId:       job.GetBuildId(),
			State:         platformv1.BuildState_BUILD_STATE_FAILED,
			CommitSha:     job.GetCommitSha(),
			FailureReason: err.Error(),
			LeaseEpoch:    job.GetLeaseEpoch(),
		})
		if completeErr != nil {
			if cooperativeBuildCancel(completeErr) {
				return nil
			}
			return &buildFailureError{kind: failureKindProtocol, err: fmt.Errorf("report failed build: %w", completeErr)}
		}
		return err
	}

	if _, err := a.client.CompleteBuild(ctx, &platformv1.CompleteBuildRequest{
		BuilderId:   a.cfg.ID,
		BuildId:     job.GetBuildId(),
		State:       platformv1.BuildState_BUILD_STATE_SUCCEEDED,
		CommitSha:   job.GetCommitSha(),
		ImageDigest: imageRef,
		LeaseEpoch:  job.GetLeaseEpoch(),
	}); err != nil {
		if cooperativeBuildCancel(err) {
			return nil
		}
		return &buildFailureError{kind: failureKindProtocol, err: fmt.Errorf("report successful build: %w", err)}
	}
	return nil
}

func (a *App) buildAndPush(ctx context.Context, job *platformv1.BuildJob) (string, error) {
	archivePath, digest, err := a.downloadSourceSnapshot(ctx, job)
	if err != nil {
		return "", err
	}
	defer os.Remove(archivePath)
	reporter := newBuildLogReporter(ctx, a.client, a.cfg.ID, job.GetBuildId(), job.GetLeaseEpoch())
	defer reporter.Close()
	spec := a.executionSpecForJob(ctx, job, archivePath, digest, reporter)
	result, err := a.executor.Execute(ctx, spec)
	if err != nil {
		return "", err
	}
	return result.ImageDigestRef, nil
}

func (a *App) executionSpecForJob(ctx context.Context, job *platformv1.BuildJob, archivePath, digest string, reporter *buildLogReporter) ExecutionSpec {
	return ExecutionSpec{
		BuildID:       job.GetBuildId(),
		ServiceID:     job.GetServiceId(),
		ProjectID:     job.GetProjectId(),
		EnvironmentID: job.GetEnvironmentId(),
		CommitSHA:     job.GetCommitSha(),

		SnapshotArchivePath: archivePath,
		SnapshotID:          job.GetSource().GetSourceSnapshotId(),
		SnapshotDigest:      digest,

		Recipe: job.GetSource().GetBuildRecipe(),

		Push: PushCredentials{
			Reference: job.GetRegistryPushReference(),
			Username:  job.GetRegistryUsername(),
			Password:  job.GetRegistryPassword(),
		},
		Buildkit: BuildkitEndpoint{
			Binary:  a.cfg.BuildctlBinary,
			Address: a.cfg.BuildkitAddress,
		},
		Railpack: RailpackToolchain{
			Binary:        a.cfg.RailpackBinary,
			FrontendImage: a.cfg.RailpackFrontendImage,
		},
		Limits: ResourceLimits{
			Timeout:           time.Duration(a.cfg.Limits.TimeoutSeconds) * time.Second,
			MemoryBytes:       a.cfg.Limits.MemoryBytes,
			CPUSeconds:        a.cfg.Limits.CPUSeconds,
			MaxFileBytes:      a.cfg.Limits.MaxFileBytes,
			MaxProcesses:      a.cfg.Limits.MaxProcesses,
			MaxWorkspaceBytes: a.cfg.Limits.MaxWorkspaceBytes,
		},
		Network: NetworkPolicy{
			AllowGeneralEgress: !a.cfg.Network.DenyGeneralEgress,
			DeniedCIDRs:        a.cfg.Network.DeniedCIDRs,
		},
		// The development executor exports no cache between
		// executions. The hardened executor mounts a host cache dir
		// namespaced by ContentCacheKey when the operator enables
		// content-addressed mode, and refuses any key that does not
		// match the build content.
		Cache:   a.cachePolicyForJob(job, digest),
		Cleanup: a.cfg.CleanupWorkDir,
		OnLog: func(line commandOutputLine) {
			reporter.Report(ctx, line)
		},
	}
}

// cachePolicyForJob maps the configured cache mode to an execution
// cache policy. Content-addressed keys derive purely from build
// content so cached data cannot carry state between projects.
func (a *App) cachePolicyForJob(job *platformv1.BuildJob, digest string) CachePolicy {
	if a.cfg.Cache.Mode != string(CacheModeContentAddressed) {
		return CachePolicy{Mode: CacheModeNone}
	}
	return CachePolicy{
		Mode: CacheModeContentAddressed,
		Key:  ContentCacheKey(digest, job.GetSource().GetBuildRecipe(), a.cfg.RailpackFrontendImage),
	}
}

// downloadSourceSnapshot streams the verified snapshot archive into a
// staging file and returns its path and verified digest. The caller
// removes the file; the executor re-verifies the digest before
// extracting.
func (a *App) downloadSourceSnapshot(ctx context.Context, job *platformv1.BuildJob) (string, string, error) {
	source := job.GetSource()
	if source == nil || source.GetSourceSnapshotId() == "" {
		return "", "", &buildFailureError{kind: failureKindFetch, err: errors.New("build job source snapshot is required")}
	}
	stream, err := a.client.DownloadSourceSnapshot(ctx, &platformv1.DownloadSourceSnapshotRequest{
		SnapshotId: source.GetSourceSnapshotId(),
	})
	if err != nil {
		return "", "", &buildFailureError{kind: failureKindFetch, err: fmt.Errorf("download source snapshot: %w", err)}
	}
	archiveFile, err := os.CreateTemp("", ".source-snapshot-*.tgz")
	if err != nil {
		return "", "", &buildFailureError{kind: failureKindFetch, err: fmt.Errorf("create source snapshot file: %w", err)}
	}
	archivePath := archiveFile.Name()
	failed := true
	defer func() {
		if failed {
			_ = os.Remove(archivePath)
		}
	}()

	expectedSnapshotID := source.GetSourceSnapshotId()
	expectedDigest := strings.TrimSpace(source.GetSourceSnapshotDigest())
	var streamDigest string
	hash := sha256.New()
	var totalSize int64 = -1
	var written int64
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			_ = archiveFile.Close()
			return "", "", &buildFailureError{kind: failureKindFetch, err: fmt.Errorf("receive source snapshot: %w", recvErr)}
		}
		if chunk.GetSnapshotId() != expectedSnapshotID {
			_ = archiveFile.Close()
			return "", "", &buildFailureError{kind: failureKindFetch, err: errors.New("source snapshot id changed while streaming")}
		}
		if totalSize < 0 {
			totalSize = chunk.GetTotalSize()
			streamDigest = strings.TrimSpace(chunk.GetDigest())
			if totalSize <= 0 {
				_ = archiveFile.Close()
				return "", "", &buildFailureError{kind: failureKindFetch, err: errors.New("source snapshot archive is empty")}
			}
			if totalSize > maxSourceArchiveCompressedBytes {
				_ = archiveFile.Close()
				return "", "", &buildFailureError{kind: failureKindFetch, err: errors.New("snapshot archive exceeds compressed size limit")}
			}
			if !strings.HasPrefix(streamDigest, "sha256:") || len(streamDigest) != len("sha256:")+sha256.Size*2 {
				_ = archiveFile.Close()
				return "", "", &buildFailureError{kind: failureKindFetch, err: errors.New("source snapshot digest is invalid")}
			}
		}
		if chunk.GetTotalSize() != totalSize || chunk.GetOffset() != written || chunk.GetDigest() != streamDigest {
			_ = archiveFile.Close()
			return "", "", &buildFailureError{kind: failureKindFetch, err: errors.New("source snapshot chunk metadata is inconsistent")}
		}
		if len(chunk.GetData()) == 0 || len(chunk.GetData()) > maxSourceArchiveChunkBytes {
			_ = archiveFile.Close()
			return "", "", &buildFailureError{kind: failureKindFetch, err: errors.New("source snapshot chunk exceeds size limit")}
		}
		if written+int64(len(chunk.GetData())) > totalSize {
			_ = archiveFile.Close()
			return "", "", &buildFailureError{kind: failureKindFetch, err: errors.New("source snapshot stream exceeds declared size")}
		}
		if expectedDigest != "" && chunk.GetDigest() != expectedDigest {
			_ = archiveFile.Close()
			return "", "", &buildFailureError{kind: failureKindFetch, err: fmt.Errorf("snapshot digest mismatch: job=%s stream=%s", expectedDigest, chunk.GetDigest())}
		}
		n, err := archiveFile.Write(chunk.GetData())
		if err != nil {
			_ = archiveFile.Close()
			return "", "", &buildFailureError{kind: failureKindFetch, err: fmt.Errorf("write source snapshot: %w", err)}
		}
		if n != len(chunk.GetData()) {
			_ = archiveFile.Close()
			return "", "", &buildFailureError{kind: failureKindFetch, err: io.ErrShortWrite}
		}
		_, _ = hash.Write(chunk.GetData())
		written += int64(len(chunk.GetData()))
	}
	if written == 0 || written != totalSize {
		_ = archiveFile.Close()
		return "", "", &buildFailureError{kind: failureKindFetch, err: errors.New("source snapshot stream ended before declared size")}
	}
	actualDigest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actualDigest != streamDigest {
		_ = archiveFile.Close()
		return "", "", &buildFailureError{kind: failureKindFetch, err: fmt.Errorf("snapshot digest verification failed: expected=%s actual=%s", streamDigest, actualDigest)}
	}
	if err := archiveFile.Close(); err != nil {
		return "", "", &buildFailureError{kind: failureKindFetch, err: fmt.Errorf("close source snapshot: %w", err)}
	}
	failed = false
	return archivePath, streamDigest, nil
}

func buildDigestRefFromMetadata(pushRef, metadataFile string) (string, error) {
	data, err := os.ReadFile(metadataFile)
	if err != nil {
		return "", &buildFailureError{kind: failureKindProtocol, err: fmt.Errorf("read build metadata: %w", err)}
	}
	digest, err := parseBuildMetadata(data)
	if err != nil {
		return "", &buildFailureError{kind: failureKindProtocol, err: err}
	}
	return runtimeDigestRef(pushRef, digest), nil
}

func buildCommand(buildBinary, buildkitAddress, contextDir, repoDir, dockerfilePath, pushRef, metadataFile string, env []string) commandRequest {
	if isDockerBuildBinary(buildBinary) {
		return dockerBuildxCommand(buildBinary, contextDir, repoDir, dockerfilePath, pushRef, metadataFile, env)
	}
	return buildctlCommand(buildBinary, buildkitAddress, contextDir, repoDir, dockerfilePath, pushRef, metadataFile, env)
}

func isDockerBuildBinary(buildBinary string) bool {
	return filepath.Base(strings.TrimSpace(buildBinary)) == "docker"
}

func dockerBuildxCommand(dockerBinary, contextDir, repoDir, dockerfilePath, pushRef, metadataFile string, env []string) commandRequest {
	return commandRequest{
		Binary: dockerBinary,
		Env:    env,
		Args: []string{
			"buildx", "build",
			"--progress=plain",
			"--add-host", "host.docker.internal:host-gateway",
			"--file", filepath.Join(repoDir, filepath.FromSlash(dockerfilePath)),
			"--tag", pushRef,
			"--push",
			"--metadata-file", metadataFile,
			contextDir,
		},
	}
}

func buildctlCommand(buildctlBinary, buildkitAddress, contextDir, repoDir, dockerfilePath, pushRef, metadataFile string, env []string) commandRequest {
	return commandRequest{
		Binary: buildctlBinary,
		Env:    env,
		Args: []string{
			"--addr", buildkitAddress,
			"build",
			"--frontend", "dockerfile.v0",
			"--local", "context=" + contextDir,
			"--local", "dockerfile=" + repoDir,
			"--opt", "filename=" + dockerfilePath,
			"--output", "type=image,name=" + pushRef + ",push=true",
			"--metadata-file", metadataFile,
		},
	}
}

func validateBuildInputs(repoDir string, recipe *platformv1.BuildRecipe) (string, string, error) {
	if recipe == nil {
		return "", "", errors.New("build recipe is required")
	}
	repoRoot := repoDir
	if evaluatedRoot, err := filepath.EvalSymlinks(repoDir); err == nil {
		repoRoot = evaluatedRoot
	}
	contextDir, err := resolveRepoPath(repoDir, recipe.GetContextDir(), true)
	if err != nil {
		return "", "", fmt.Errorf("context dir: %w", err)
	}
	dockerfileAbs, err := resolveRepoPath(repoDir, recipe.GetDockerfilePath(), false)
	if err != nil {
		return "", "", fmt.Errorf("dockerfile path: %w", err)
	}
	dockerfileRel, err := filepath.Rel(repoRoot, dockerfileAbs)
	if err != nil {
		return "", "", fmt.Errorf("dockerfile path: %w", err)
	}
	return contextDir, filepath.ToSlash(dockerfileRel), nil
}

func resolveRepoPath(repoDir, raw string, expectDir bool) (string, error) {
	root := repoDir
	if evaluatedRoot, err := filepath.EvalSymlinks(repoDir); err == nil {
		root = evaluatedRoot
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		if expectDir {
			value = "."
		} else {
			value = "Dockerfile"
		}
	}
	if filepath.IsAbs(value) {
		return "", errors.New("absolute paths are not allowed")
	}
	clean := filepath.Clean(value)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes repository")
	}
	candidate := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes repository")
	}
	resolved := candidate
	if evaluated, err := filepath.EvalSymlinks(candidate); err == nil {
		rel, err = filepath.Rel(root, evaluated)
		if err != nil {
			return "", err
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", errors.New("symlink escapes repository")
		}
		resolved = evaluated
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if expectDir && !info.IsDir() {
		return "", errors.New("expected directory")
	}
	if !expectDir && info.IsDir() {
		return "", errors.New("expected file")
	}
	return resolved, nil
}

func safeChildPath(root, child string) (string, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	child = strings.TrimSpace(child)
	if root == "" || child == "" {
		return "", errors.New("workspace path is required")
	}
	if filepath.IsAbs(child) {
		return "", errors.New("absolute child path is not allowed")
	}
	path := filepath.Join(root, child)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("workspace path escapes root")
	}
	return path, nil
}
