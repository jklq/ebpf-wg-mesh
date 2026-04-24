package builder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
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

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	failureKindFetch    = "fetch failure"
	failureKindBuild    = "build failure"
	failureKindPush     = "push failure"
	failureKindProtocol = "protocol/reporting failure"
)

type commandRequest struct {
	Dir    string
	Binary string
	Env    []string
	Args   []string
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

type jobWorkspace struct {
	root         string
	repoDir      string
	metadataFile string
}

type App struct {
	cfg    config.BuilderConfig
	conn   *grpc.ClientConn
	client platformv1.BuilderServiceClient
	runner commandRunner
}

func New(cfg config.BuilderConfig) (*App, error) {
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir builder work dir: %w", err)
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
		cfg:    cfg,
		conn:   conn,
		client: platformv1.NewBuilderServiceClient(conn),
		runner: osCommandRunner{},
	}, nil
}

func (a *App) Close() error {
	if a == nil || a.conn == nil {
		return nil
	}
	return a.conn.Close()
}

func (a *App) Run(ctx context.Context) error {
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
					BuilderId: a.cfg.ID,
					BuildId:   job.GetBuildId(),
				})
				if err != nil {
					slog.Warn("report builder heartbeat", "build_id", job.GetBuildId(), "error", err)
				}
			}
		}
	}()

	imageRef, err := a.buildAndPush(jobCtx, job)
	cancel()
	<-heartbeatDone

	if err != nil {
		_, completeErr := a.client.CompleteBuild(ctx, &platformv1.CompleteBuildRequest{
			BuilderId:     a.cfg.ID,
			BuildId:       job.GetBuildId(),
			State:         platformv1.BuildState_BUILD_STATE_FAILED,
			CommitSha:     job.GetCommitSha(),
			FailureReason: err.Error(),
		})
		if completeErr != nil {
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
	}); err != nil {
		return &buildFailureError{kind: failureKindProtocol, err: fmt.Errorf("report successful build: %w", err)}
	}
	return nil
}

func (a *App) buildAndPush(ctx context.Context, job *platformv1.BuildJob) (string, error) {
	workspace, err := prepareWorkspace(a.cfg.WorkDir, job.GetBuildId())
	if err != nil {
		return "", &buildFailureError{kind: failureKindFetch, err: err}
	}
	if a.cfg.CleanupWorkDir {
		defer os.RemoveAll(workspace.root)
	}
	if err := a.materializeSourceSnapshot(ctx, job, workspace.repoDir); err != nil {
		return "", err
	}
	return a.invokeBuildctl(ctx, job, workspace)
}

func (a *App) materializeSourceSnapshot(ctx context.Context, job *platformv1.BuildJob, repoDir string) error {
	source := job.GetSource()
	if source == nil || source.GetSourceSnapshotId() == "" {
		return &buildFailureError{kind: failureKindFetch, err: errors.New("build job source snapshot is required")}
	}
	artifact, err := a.client.DownloadSourceSnapshot(ctx, &platformv1.DownloadSourceSnapshotRequest{
		SnapshotId: source.GetSourceSnapshotId(),
	})
	if err != nil {
		return &buildFailureError{kind: failureKindFetch, err: fmt.Errorf("download source snapshot: %w", err)}
	}
	if len(artifact.GetArchiveTgz()) == 0 {
		return &buildFailureError{kind: failureKindFetch, err: errors.New("source snapshot archive is empty")}
	}
	if digest := strings.TrimSpace(source.GetSourceSnapshotDigest()); digest != "" && digest != artifact.GetDigest() {
		return &buildFailureError{kind: failureKindFetch, err: fmt.Errorf("snapshot digest mismatch: job=%s artifact=%s", digest, artifact.GetDigest())}
	}
	if err := extractSourceSnapshot(repoDir, artifact.GetArchiveTgz()); err != nil {
		return &buildFailureError{kind: failureKindFetch, err: err}
	}
	return nil
}

func (a *App) invokeBuildctl(ctx context.Context, job *platformv1.BuildJob, workspace jobWorkspace) (string, error) {
	contextDir, dockerfilePath, err := validateBuildInputs(workspace.repoDir, job.GetSource().GetBuildRecipe())
	if err != nil {
		return "", &buildFailureError{kind: failureKindBuild, err: err}
	}
	env, cleanup, err := dockerConfigEnv(workspace.root, job.GetRegistryPushReference(), job.GetRegistryUsername(), job.GetRegistryPassword())
	if err != nil {
		return "", &buildFailureError{kind: failureKindPush, err: err}
	}
	defer cleanup()

	req := buildCommand(a.cfg.BuildctlBinary, a.cfg.BuildkitAddress, contextDir, workspace.repoDir, dockerfilePath, job.GetRegistryPushReference(), workspace.metadataFile, env)
	reporter := newBuildLogReporter(ctx, a.client, a.cfg.ID, job.GetBuildId())
	defer reporter.Close()
	output, err := a.runner.Run(ctx, req, func(line commandOutputLine) {
		reporter.Report(ctx, line)
	})
	if err != nil {
		return "", classifyBuildctlFailure(formatBuildCommandError(req, err, output))
	}
	data, err := os.ReadFile(workspace.metadataFile)
	if err != nil {
		return "", &buildFailureError{kind: failureKindProtocol, err: fmt.Errorf("read build metadata: %w", err)}
	}
	digest, err := parseBuildMetadata(data)
	if err != nil {
		return "", &buildFailureError{kind: failureKindProtocol, err: err}
	}
	return runtimeDigestRef(job.GetRegistryPushReference(), digest), nil
}

func prepareWorkspace(workDir, buildID string) (jobWorkspace, error) {
	root, err := safeChildPath(workDir, buildID)
	if err != nil {
		return jobWorkspace{}, err
	}
	repoDir, err := safeChildPath(root, "repo")
	if err != nil {
		return jobWorkspace{}, err
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return jobWorkspace{}, fmt.Errorf("mkdir repo dir: %w", err)
	}
	return jobWorkspace{
		root:         root,
		repoDir:      repoDir,
		metadataFile: filepath.Join(root, "metadata.json"),
	}, nil
}

func buildCommand(buildBinary, buildkitAddress, contextDir, repoDir, dockerfilePath, pushRef, metadataFile string, env []string) commandRequest {
	if filepath.Base(strings.TrimSpace(buildBinary)) == "docker" {
		return dockerBuildxCommand(buildBinary, contextDir, repoDir, dockerfilePath, pushRef, metadataFile, env)
	}
	return buildctlCommand(buildBinary, buildkitAddress, contextDir, repoDir, dockerfilePath, pushRef, metadataFile, env)
}

func dockerBuildxCommand(dockerBinary, contextDir, repoDir, dockerfilePath, pushRef, metadataFile string, env []string) commandRequest {
	return commandRequest{
		Binary: dockerBinary,
		Env:    env,
		Args: []string{
			"buildx", "build",
			"--progress=plain",
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

func extractSourceSnapshot(repoDir string, archiveTGZ []byte) error {
	gzr, err := gzip.NewReader(bytes.NewReader(archiveTGZ))
	if err != nil {
		return fmt.Errorf("open snapshot archive: %w", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read snapshot archive: %w", err)
		}
		name := strings.TrimSpace(hdr.Name)
		if name == "" {
			continue
		}
		rel, ok := stripArchiveRoot(name)
		if !ok || rel == "" {
			continue
		}
		target, err := safeArchivePath(repoDir, rel)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir snapshot dir: %w", err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir snapshot parent: %w", err)
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(hdr.Mode&0o777))
			if err != nil {
				return fmt.Errorf("create snapshot file: %w", err)
			}
			if _, err := io.Copy(file, tr); err != nil {
				_ = file.Close()
				return fmt.Errorf("write snapshot file: %w", err)
			}
			if err := file.Close(); err != nil {
				return fmt.Errorf("close snapshot file: %w", err)
			}
		default:
			return fmt.Errorf("unsupported snapshot entry type %d", hdr.Typeflag)
		}
	}
}

func stripArchiveRoot(name string) (string, bool) {
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == "." || clean == "/" {
		return "", false
	}
	parts := strings.Split(clean, "/")
	if len(parts) <= 1 {
		return "", false
	}
	rel := strings.Join(parts[1:], "/")
	return rel, true
}

func safeArchivePath(root, child string) (string, error) {
	if filepath.IsAbs(child) {
		return "", errors.New("snapshot entry path is absolute")
	}
	clean := filepath.Clean(child)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("snapshot entry escapes repository")
	}
	target := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("snapshot entry escapes repository")
	}
	return target, nil
}

func parseBuildMetadata(data []byte) (string, error) {
	var metadata map[string]any
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", fmt.Errorf("parse build metadata: %w", err)
	}
	digest, _ := metadata["containerimage.digest"].(string)
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return "", errors.New("build metadata missing containerimage.digest")
	}
	return digest, nil
}

func classifyBuildctlFailure(err error) error {
	message := strings.ToLower(err.Error())
	kind := failureKindBuild
	for _, token := range []string{"push", "registry", "unauthorized", "denied", "insufficient_scope"} {
		if strings.Contains(message, token) {
			kind = failureKindPush
			break
		}
	}
	return &buildFailureError{kind: kind, err: err}
}

func formatBuildCommandError(req commandRequest, err error, output []byte) error {
	message := fmt.Sprintf("%s %s: %v", req.Binary, strings.Join(req.Args, " "), err)
	tail := strings.TrimSpace(string(output))
	if tail == "" {
		return errors.New(message)
	}
	if len(tail) > maxBuildFailureTailBytes {
		tail = tail[len(tail)-maxBuildFailureTailBytes:]
	}
	return fmt.Errorf("%s: %s", message, tail)
}

func dockerConfigEnv(workDir, pushRef, username, password string) ([]string, func(), error) {
	if strings.TrimSpace(pushRef) == "" || strings.TrimSpace(username) == "" || strings.TrimSpace(password) == "" {
		return nil, func() {}, nil
	}
	host, _, _ := strings.Cut(pushRef, "/")
	dir, err := os.MkdirTemp(workDir, "docker-config-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create docker config dir: %w", err)
	}
	baseDir := dockerDefaultConfigDir()
	if err := mirrorDockerConfigSupport(baseDir, dir); err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	configJSON, err := mergedDockerConfigJSON(baseDir, host, auth)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), configJSON, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}
	return []string{"DOCKER_CONFIG=" + dir}, func() { _ = os.RemoveAll(dir) }, nil
}

func dockerDefaultConfigDir() string {
	if dir := strings.TrimSpace(os.Getenv("DOCKER_CONFIG")); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".docker")
}

func mirrorDockerConfigSupport(srcDir, dstDir string) error {
	for _, name := range []string{"cli-plugins", "buildx", "contexts"} {
		if err := symlinkIfExists(filepath.Join(srcDir, name), filepath.Join(dstDir, name)); err != nil {
			return err
		}
	}
	return nil
}

func symlinkIfExists(src, dst string) error {
	if strings.TrimSpace(src) == "" {
		return nil
	}
	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", src, err)
	}
	if err := os.Symlink(src, dst); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", dst, src, err)
	}
	return nil
}

func mergedDockerConfigJSON(baseDir, host, auth string) ([]byte, error) {
	config := map[string]any{}
	if strings.TrimSpace(baseDir) != "" {
		data, err := os.ReadFile(filepath.Join(baseDir, "config.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read base docker config: %w", err)
		}
		if len(data) > 0 {
			if err := json.Unmarshal(data, &config); err != nil {
				return nil, fmt.Errorf("parse base docker config: %w", err)
			}
		}
	}
	auths, ok := config["auths"].(map[string]any)
	if !ok || auths == nil {
		auths = map[string]any{}
	}
	entry, ok := auths[host].(map[string]any)
	if !ok || entry == nil {
		entry = map[string]any{}
	}
	entry["auth"] = auth
	auths[host] = entry
	config["auths"] = auths
	return json.Marshal(config)
}

func runtimeDigestRef(pushRef, digest string) string {
	base, _, _ := strings.Cut(pushRef, ":")
	return base + "@" + digest
}

func loadTransportCredentials(cfg config.InternalClientTLSConfig) (credentials.TransportCredentials, error) {
	caPEM, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read ca file: %w", err)
	}
	certPEM, err := os.ReadFile(cfg.CertFile)
	if err != nil {
		return nil, fmt.Errorf("read cert file: %w", err)
	}
	keyPEM, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("read key file: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load x509 key pair: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("append control plane CA: no certificates added")
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      roots,
		ServerName:   cfg.ServerName,
		MinVersion:   tls.VersionTLS13,
	}), nil
}
