package builder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestDestroyExecutionWorkspaceDoesNotChmodSymlinkTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("host data"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := destroyExecutionWorkspace(root); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("symlink target mode = %o, want 600", got)
	}
}

func testExecutionSpec(t *testing.T, buildID string, archive []byte, recipe *platformv1.BuildRecipe) ExecutionSpec {
	t.Helper()

	archivePath := filepath.Join(t.TempDir(), "snapshot.tgz")
	if err := os.WriteFile(archivePath, archive, 0o644); err != nil {
		t.Fatalf("WriteFile(snapshot): %v", err)
	}
	sum := sha256.Sum256(archive)
	return ExecutionSpec{
		BuildID: buildID,

		SnapshotArchivePath: archivePath,
		SnapshotDigest:      "sha256:" + hex.EncodeToString(sum[:]),

		Recipe: recipe,

		Push: PushCredentials{
			Reference: "registry.example.test/project-1/env-1/build/service:git-deadbeef",
			Username:  "alice",
			Password:  "secret",
		},
		Buildkit: BuildkitEndpoint{
			Binary:  "buildctl",
			Address: "unix:///run/buildkit/buildkitd.sock",
		},
		Railpack: RailpackToolchain{
			Binary:        "railpack",
			FrontendImage: "ghcr.io/railwayapp/railpack-frontend:latest",
		},
		Limits: ResourceLimits{
			Timeout:           time.Minute,
			MemoryBytes:       1 << 30,
			CPUSeconds:        60,
			MaxFileBytes:      1 << 30,
			MaxProcesses:      512,
			MaxWorkspaceBytes: 1 << 30,
		},
		Network: DefaultRestrictedNetworkPolicy(),
		Cache:   CachePolicy{Mode: CacheModeNone},
		Cleanup: true,
	}
}

func dockerfileArchiveForTest(t *testing.T) []byte {
	t.Helper()
	return makeSnapshotArchive(t, map[string]string{
		"repo/Dockerfile": "FROM scratch\n",
	})
}

func dockerfileRecipeForTest() *platformv1.BuildRecipe {
	return &platformv1.BuildRecipe{
		Builder:        platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE,
		ContextDir:     ".",
		DockerfilePath: "Dockerfile",
	}
}

func writeMetadataForTest(t *testing.T, req commandRequest, digest string) {
	t.Helper()
	metadata := metadataFilePath(req.Args)
	if metadata == "" {
		t.Fatal("missing --metadata-file")
	}
	if err := os.WriteFile(metadata, []byte(`{"containerimage.digest":"`+digest+`"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(metadata): %v", err)
	}
}

func TestExecuteDockerfileDestroysAndVerifiesWorkspace(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	runner := &scriptedCommandRunner{
		handle: func(req commandRequest) ([]byte, error) {
			writeMetadataForTest(t, req, "sha256:abc")
			return []byte("build ok\n"), nil
		},
	}
	spec := testExecutionSpec(t, "build-1", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	executor := newDevelopmentExecutor(workDir, runner, "/usr/bin:/bin")
	result, err := executor.Execute(context.Background(), spec)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ImageDigestRef != "registry.example.test/project-1/env-1/build/service@sha256:abc" {
		t.Fatalf("unexpected digest ref %q", result.ImageDigestRef)
	}
	requests := runner.Requests()
	if len(requests) != 1 || requests[0].Binary != "buildctl" {
		t.Fatalf("unexpected commands %#v", requests)
	}
	if requests[0].Limits == nil {
		t.Fatal("build child must receive process limits")
	}
	if requests[0].Limits.MemoryBytes != spec.Limits.MemoryBytes ||
		requests[0].Limits.CPUSeconds != spec.Limits.CPUSeconds ||
		requests[0].Limits.MaxFileBytes != spec.Limits.MaxFileBytes ||
		requests[0].Limits.MaxProcesses != spec.Limits.MaxProcesses {
		t.Fatalf("unexpected process limits %#v", requests[0].Limits)
	}
	if _, err := os.Stat(filepath.Join(workDir, "build-1")); !os.IsNotExist(err) {
		t.Fatalf("workspace must be removed after execution, stat err=%v", err)
	}
}

func TestExecuteMarksWorkspaceOwnedDuringRun(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	runner := &scriptedCommandRunner{
		handle: func(req commandRequest) ([]byte, error) {
			metadata := metadataFilePath(req.Args)
			if metadata == "" {
				return nil, errors.New("missing --metadata-file")
			}
			data, err := os.ReadFile(filepath.Join(filepath.Dir(metadata), executorOwnerMarker))
			if err != nil {
				return nil, errors.New("owner marker missing during execution: " + err.Error())
			}
			var owner executorOwner
			if err := json.Unmarshal(data, &owner); err != nil {
				return nil, err
			}
			if owner.BuildID != "build-1" || owner.PID != os.Getpid() {
				return nil, errors.New("unexpected owner marker content")
			}
			writeMetadataForTest(t, req, "sha256:abc")
			return []byte("build ok\n"), nil
		},
	}
	spec := testExecutionSpec(t, "build-1", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	executor := newDevelopmentExecutor(workDir, runner, "/usr/bin:/bin")
	if _, err := executor.Execute(context.Background(), spec); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestExecuteLeavesWorkspaceWithoutMarkerWhenCleanupDisabled(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	runner := &scriptedCommandRunner{
		handle: func(req commandRequest) ([]byte, error) {
			writeMetadataForTest(t, req, "sha256:abc")
			return []byte("build ok\n"), nil
		},
	}
	spec := testExecutionSpec(t, "build-1", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	spec.Cleanup = false
	t.Cleanup(func() { _ = destroyExecutionWorkspace(filepath.Join(workDir, "build-1")) })
	executor := newDevelopmentExecutor(workDir, runner, "/usr/bin:/bin")
	if _, err := executor.Execute(context.Background(), spec); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	root := filepath.Join(workDir, "build-1")
	if _, err := os.Stat(filepath.Join(root, "metadata.json")); err != nil {
		t.Fatalf("expected workspace to remain for debugging: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, executorOwnerMarker)); !os.IsNotExist(err) {
		t.Fatalf("completed execution must drop its owner marker, stat err=%v", err)
	}
}

func TestExecuteMakesSnapshotReadOnly(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	var repoPath string
	runner := &scriptedCommandRunner{
		handle: func(req commandRequest) ([]byte, error) {
			metadata := metadataFilePath(req.Args)
			repoPath = filepath.Join(filepath.Dir(metadata), "repo")
			writeMetadataForTest(t, req, "sha256:abc")
			return []byte("build ok\n"), nil
		},
	}
	spec := testExecutionSpec(t, "build-1", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	spec.Cleanup = false
	t.Cleanup(func() { _ = destroyExecutionWorkspace(filepath.Join(workDir, "build-1")) })
	executor := newDevelopmentExecutor(workDir, runner, "/usr/bin:/bin")
	if _, err := executor.Execute(context.Background(), spec); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	info, err := os.Stat(filepath.Join(repoPath, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Fatalf("snapshot file must be read-only, got %o", info.Mode().Perm())
	}
	if err := os.WriteFile(filepath.Join(repoPath, "Dockerfile"), []byte("x"), 0o644); err == nil {
		t.Fatal("snapshot file must not be writable")
	}
	if _, err := os.Stat(filepath.Join(repoPath, ".executor-write-probe")); !os.IsNotExist(err) {
		t.Fatalf("write probe must not exist, stat err=%v", err)
	}
	scratchInfo, err := os.Stat(filepath.Join(workDir, "build-1", "scratch"))
	if err != nil {
		t.Fatal(err)
	}
	if !scratchInfo.IsDir() || scratchInfo.Mode().Perm()&0o200 == 0 {
		t.Fatalf("scratch dir must stay writable, got %o", scratchInfo.Mode().Perm())
	}
}

func TestExecuteRejectsTamperedSnapshot(t *testing.T) {
	t.Parallel()

	archive := dockerfileArchiveForTest(t)
	spec := testExecutionSpec(t, "build-1", archive, dockerfileRecipeForTest())
	if err := os.WriteFile(spec.SnapshotArchivePath, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	executor := newDevelopmentExecutor(t.TempDir(), &scriptedCommandRunner{}, "/usr/bin:/bin")
	_, err := executor.Execute(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "digest verification failed") {
		t.Fatalf("expected digest verification failure, got %v", err)
	}
}

func TestExecuteRequiresScopedPushCredentials(t *testing.T) {
	t.Parallel()

	archive := dockerfileArchiveForTest(t)
	cases := map[string]PushCredentials{
		"missing reference": {Username: "alice", Password: "secret"},
		"missing username":  {Reference: "registry.example.test/a/b:tag", Password: "secret"},
		"missing password":  {Reference: "registry.example.test/a/b:tag", Username: "alice"},
		"bare hostname":     {Reference: "registry.example.test", Username: "alice", Password: "secret"},
		"no repository":     {Reference: "registry.example.test/:tag", Username: "alice", Password: "secret"},
	}
	for name, push := range cases {
		spec := testExecutionSpec(t, "build-1", archive, dockerfileRecipeForTest())
		spec.Push = push
		executor := newDevelopmentExecutor(t.TempDir(), &scriptedCommandRunner{}, "/usr/bin:/bin")
		_, err := executor.Execute(context.Background(), spec)
		if err == nil {
			t.Fatalf("%s: expected credential error, got nil", name)
		}
	}
}

func TestExecuteScrubsAmbientEnvironment(t *testing.T) {
	t.Setenv("BUILDER_ROGUE_VARIABLE", "rogue")
	t.Setenv("HTTP_PROXY", "http://proxy.example.test:3128")
	t.Setenv("HTTPS_PROXY", "http://proxy.example.test:3128")
	t.Setenv("DOCKER_CONTEXT", "rogue-context")

	runner := &scriptedCommandRunner{
		handle: func(req commandRequest) ([]byte, error) {
			writeMetadataForTest(t, req, "sha256:abc")
			return []byte("build ok\n"), nil
		},
	}
	spec := testExecutionSpec(t, "build-1", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	executor := newDevelopmentExecutor(t.TempDir(), runner, "/usr/bin:/bin")
	if _, err := executor.Execute(context.Background(), spec); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	requests := runner.Requests()
	if len(requests) != 1 {
		t.Fatalf("expected one command, got %d", len(requests))
	}
	env := map[string]string{}
	for _, entry := range requests[0].Env {
		name, value, _ := strings.Cut(entry, "=")
		env[name] = value
	}
	for _, banned := range []string{"BUILDER_ROGUE_VARIABLE", "HTTP_PROXY", "HTTPS_PROXY", "DOCKER_CONTEXT"} {
		if _, ok := env[banned]; ok {
			t.Fatalf("ambient %s must not enter the build", banned)
		}
	}
	if env["PATH"] != "/usr/bin:/bin" {
		t.Fatalf("unexpected PATH %q", env["PATH"])
	}
	for _, want := range []string{"HOME", "TMPDIR", "DOCKER_CONFIG"} {
		if env[want] == "" {
			t.Fatalf("expected explicit %s in build env", want)
		}
	}
}

func TestExecuteTimeoutDestroysWorkspace(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	runner := &blockingCommandRunner{started: make(chan commandRequest, 1)}
	spec := testExecutionSpec(t, "build-1", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	spec.Limits.Timeout = 50 * time.Millisecond
	executor := newDevelopmentExecutor(workDir, runner, "/usr/bin:/bin")
	start := time.Now()
	_, err := executor.Execute(context.Background(), spec)
	if !errors.Is(err, ErrBuildTimeout) {
		t.Fatalf("expected ErrBuildTimeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout took too long: %s", elapsed)
	}
	if _, err := os.Stat(filepath.Join(workDir, "build-1")); !os.IsNotExist(err) {
		t.Fatalf("timed-out workspace must be removed, stat err=%v", err)
	}
}

func TestExecuteCancellationDestroysWorkspace(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	runner := &blockingCommandRunner{started: make(chan commandRequest, 1)}
	spec := testExecutionSpec(t, "build-1", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	executor := newDevelopmentExecutor(workDir, runner, "/usr/bin:/bin")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := executor.Execute(ctx, spec)
		done <- err
	}()
	select {
	case <-runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for build to start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cancelled execution")
	}
	if _, err := os.Stat(filepath.Join(workDir, "build-1")); !os.IsNotExist(err) {
		t.Fatalf("cancelled workspace must be removed, stat err=%v", err)
	}
}

// blockingCommandRunner blocks until the context ends, like a build
// step that never finishes on its own.
type blockingCommandRunner struct {
	started       chan commandRequest
	fillWorkspace bool
}

func (r *blockingCommandRunner) Run(ctx context.Context, req commandRequest, _ func(commandOutputLine)) ([]byte, error) {
	if r.fillWorkspace {
		path := filepath.Join(filepath.Dir(metadataFilePath(req.Args)), "scratch", "bulk.dat")
		if err := os.WriteFile(path, make([]byte, 1<<20), 0o644); err != nil {
			return nil, err
		}
	}
	select {
	case r.started <- req:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestExecuteStopsBuildWhenWorkspaceFills(t *testing.T) {
	runner := &blockingCommandRunner{started: make(chan commandRequest, 1), fillWorkspace: true}
	spec := testExecutionSpec(t, "build-disk", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	spec.Limits.MaxWorkspaceBytes = 512 << 10
	workDir := t.TempDir()
	executor := newDevelopmentExecutor(workDir, runner, "/usr/bin:/bin")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := executor.Execute(ctx, spec)
	if err == nil || !strings.Contains(err.Error(), "exceeding") {
		t.Fatalf("expected running build to stop at workspace limit, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "build-disk")); !os.IsNotExist(err) {
		t.Fatalf("workspace remains after disk limit: %v", err)
	}
}

func TestExecuteEnforcesWorkspaceDiskLimit(t *testing.T) {
	t.Parallel()

	runner := &scriptedCommandRunner{
		handle: func(req commandRequest) ([]byte, error) {
			metadata := metadataFilePath(req.Args)
			if metadata == "" {
				return nil, errors.New("missing --metadata-file")
			}
			// Simulate a build that fills the executor-managed
			// workspace beyond its disk limit.
			if err := os.WriteFile(filepath.Join(filepath.Dir(metadata), "scratch", "bulk.dat"), make([]byte, 1<<20), 0o644); err != nil {
				return nil, err
			}
			writeMetadataForTest(t, req, "sha256:abc")
			return []byte("build ok\n"), nil
		},
	}
	spec := testExecutionSpec(t, "build-1", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	spec.Limits.MaxWorkspaceBytes = 512 << 10
	executor := newDevelopmentExecutor(t.TempDir(), runner, "/usr/bin:/bin")
	_, err := executor.Execute(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "exceeding") {
		t.Fatalf("expected workspace limit error, got %v", err)
	}
}

func TestRecoverStaleWorkspaces(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	deadRoot := filepath.Join(workDir, "build-dead")
	if err := os.MkdirAll(filepath.Join(deadRoot, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	deadMarker, err := json.Marshal(executorOwner{PID: 1 << 30, BuildID: "build-dead"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deadRoot, executorOwnerMarker), deadMarker, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deadRoot, "repo", "stale.txt"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	liveRoot := filepath.Join(workDir, "build-live")
	if err := os.MkdirAll(liveRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	liveMarker, err := json.Marshal(executorOwner{PID: os.Getpid(), BuildID: "build-live"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(liveRoot, executorOwnerMarker), liveMarker, 0o644); err != nil {
		t.Fatal(err)
	}

	foreignRoot := filepath.Join(workDir, "foreign")
	if err := os.MkdirAll(foreignRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreignRoot, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	executor := newDevelopmentExecutor(workDir, &scriptedCommandRunner{}, "/usr/bin:/bin")
	reclaimed, err := executor.RecoverStaleWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("RecoverStaleWorkspaces: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("expected 1 reclaimed workspace, got %d", reclaimed)
	}
	if _, err := os.Stat(deadRoot); !os.IsNotExist(err) {
		t.Fatalf("dead workspace must be removed, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(liveRoot, executorOwnerMarker)); err != nil {
		t.Fatalf("live workspace must be kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(foreignRoot, "keep.txt")); err != nil {
		t.Fatalf("foreign directory must be kept: %v", err)
	}
	if _, err := executor.RecoverStaleWorkspaces(context.Background()); err != nil {
		t.Fatalf("second recovery: %v", err)
	}

	missing := newDevelopmentExecutor(filepath.Join(workDir, "does-not-exist"), &scriptedCommandRunner{}, "/usr/bin:/bin")
	if reclaimed, err := missing.RecoverStaleWorkspaces(context.Background()); err != nil || reclaimed != 0 {
		t.Fatalf("missing work dir must recover cleanly, got %d, %v", reclaimed, err)
	}
}

func TestContentCacheKey(t *testing.T) {
	t.Parallel()

	recipe := &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, ContextDir: ".", DockerfilePath: "Dockerfile"}
	first := ContentCacheKey("sha256:aaa", recipe, "frontend:latest")
	second := ContentCacheKey("sha256:aaa", recipe, "frontend:latest")
	if first != second || first == "" {
		t.Fatalf("cache key must be deterministic, got %q and %q", first, second)
	}
	// The key is a pure function of content: project, service, and
	// build identity are not inputs, so identical content can never
	// carry per-project state.
	changed := map[string]string{
		"snapshot":   ContentCacheKey("sha256:bbb", recipe, "frontend:latest"),
		"context":    ContentCacheKey("sha256:aaa", &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, ContextDir: "app", DockerfilePath: "Dockerfile"}, "frontend:latest"),
		"dockerfile": ContentCacheKey("sha256:aaa", &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, ContextDir: ".", DockerfilePath: "deploy/Dockerfile"}, "frontend:latest"),
		"builder":    ContentCacheKey("sha256:aaa", &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_RAILPACK, ContextDir: "."}, "frontend:latest"),
		"frontend":   ContentCacheKey("sha256:aaa", recipe, "frontend:v2"),
	}
	for name, key := range changed {
		if key == first {
			t.Fatalf("cache key must change with %s", name)
		}
	}
}

func TestResourceLimitsValidate(t *testing.T) {
	t.Parallel()

	if err := DefaultResourceLimits().Validate(); err != nil {
		t.Fatalf("default limits must validate: %v", err)
	}
	good := DefaultResourceLimits()
	mutations := []func(*ResourceLimits){
		func(l *ResourceLimits) { l.Timeout = 0 },
		func(l *ResourceLimits) { l.MemoryBytes = 0 },
		func(l *ResourceLimits) { l.CPUSeconds = -1 },
		func(l *ResourceLimits) { l.MaxFileBytes = 0 },
		func(l *ResourceLimits) { l.MaxProcesses = 0 },
		func(l *ResourceLimits) { l.MaxWorkspaceBytes = 0 },
	}
	for i, mutate := range mutations {
		limits := good
		mutate(&limits)
		if err := limits.Validate(); err == nil {
			t.Fatalf("mutation %d must fail validation", i)
		}
	}
}

func TestNetworkPolicyValidate(t *testing.T) {
	t.Parallel()

	if err := DefaultRestrictedNetworkPolicy().Validate(); err != nil {
		t.Fatalf("default policy must validate: %v", err)
	}
	policy := DefaultRestrictedNetworkPolicy()
	policy.DeniedCIDRs = append(policy.DeniedCIDRs, "not-a-cidr")
	if err := policy.Validate(); err == nil {
		t.Fatal("expected invalid CIDR to fail validation")
	}
}

func TestCachePolicyValidate(t *testing.T) {
	t.Parallel()

	if err := (CachePolicy{Mode: CacheModeNone}).Validate(); err != nil {
		t.Fatalf("none mode must validate: %v", err)
	}
	if err := (CachePolicy{Mode: CacheModeContentAddressed, Key: "sha256:abc"}).Validate(); err != nil {
		t.Fatalf("keyed content-addressed mode must validate: %v", err)
	}
	if err := (CachePolicy{Mode: CacheModeContentAddressed}).Validate(); err == nil {
		t.Fatal("expected keyless content-addressed mode to fail")
	}
	if err := (CachePolicy{Mode: "shared"}).Validate(); err == nil {
		t.Fatal("expected unknown cache mode to fail")
	}
}

func TestSplitPushReference(t *testing.T) {
	t.Parallel()

	host, repo, err := splitPushReference("registry.example.test:5000/mesh/project/service:git-deadbeef")
	if err != nil || host != "registry.example.test:5000" || repo != "mesh/project/service" {
		t.Fatalf("unexpected tagged parse %q %q %v", host, repo, err)
	}
	host, repo, err = splitPushReference("registry.example.test/mesh/project/service@sha256:abc")
	if err != nil || host != "registry.example.test" || repo != "mesh/project/service" {
		t.Fatalf("unexpected digest parse %q %q %v", host, repo, err)
	}
	for _, bad := range []string{"", "no-slash", "https://registry.example.test/a:tag", "registry.example.test/"} {
		if _, _, err := splitPushReference(bad); err == nil {
			t.Fatalf("expected %q to fail parsing", bad)
		}
	}
}

func TestBuildAndPushRequiresScopedCredentials(t *testing.T) {
	t.Parallel()

	archive := dockerfileArchiveForTest(t)
	_, chunks := snapshotChunksForTest("snapshot-1", archive)
	workDir := t.TempDir()
	app := &App{
		cfg:      testBuilderConfig("builder-1", workDir),
		client:   &recordingBuilderServiceClient{downloadChunks: chunks},
		executor: newDevelopmentExecutor(workDir, &scriptedCommandRunner{}, "/usr/bin:/bin"),
	}
	_, err := app.buildAndPush(context.Background(), &platformv1.BuildJob{
		BuildId: "build-9",
		Source: &platformv1.BuildJobSource{
			SourceSnapshotId: "snapshot-1",
			BuildRecipe:      dockerfileRecipeForTest(),
		},
		RegistryPushReference: "registry.example.test/platform/service:build-9",
	})
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("expected scoped credential error, got %v", err)
	}
}
