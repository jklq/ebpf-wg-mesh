package builder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

// fakeSandboxBackend is a scriptable SandboxBackend for hardened
// executor tests. The zero value succeeds every operation; hooks
// override individual behaviors.
type fakeSandboxBackend struct {
	mu        sync.Mutex
	setupNets []SandboxNet
	setupErr  error
	steps     []SandboxStep
	runErr    error
	runHook   func(ctx context.Context, net SandboxNet, step SandboxStep) error
	teardowns []SandboxNet
	reaped    int
	reapErr   error
}

func (f *fakeSandboxBackend) SetupNet(ctx context.Context, buildID string, policy NetworkPolicy) (SandboxNet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setupErr != nil {
		return SandboxNet{}, f.setupErr
	}
	net := SandboxNet{BuildID: buildID, Path: "/fake/netns/" + buildID, Attached: policy.AllowGeneralEgress}
	f.setupNets = append(f.setupNets, net)
	return net, nil
}

func (f *fakeSandboxBackend) RunStep(ctx context.Context, net SandboxNet, step SandboxStep) error {
	f.mu.Lock()
	f.steps = append(f.steps, step)
	hook := f.runHook
	runErr := f.runErr
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if hook != nil {
		return hook(ctx, net, step)
	}
	return runErr
}

func (f *fakeSandboxBackend) TeardownNet(_ context.Context, net SandboxNet) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teardowns = append(f.teardowns, net)
	return nil
}

func (f *fakeSandboxBackend) ReapStale(_ context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reaped, f.reapErr
}

func (f *fakeSandboxBackend) Close() error { return nil }

func (f *fakeSandboxBackend) recordedSteps() []SandboxStep {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SandboxStep(nil), f.steps...)
}

// fakeDaemonProc is a scriptable per-execution daemon.
type fakeDaemonProc struct {
	readyErr   error
	readyCalls int
	stops      int
}

func (f *fakeDaemonProc) waitReady(context.Context) error {
	f.readyCalls++
	return f.readyErr
}

func (f *fakeDaemonProc) Stop() error {
	f.stops++
	return nil
}

type fakeDaemonStarter struct {
	proc     *fakeDaemonProc
	startErr error
	calls    []daemonStartArgs
}

type daemonStartArgs struct {
	netnsPath string
	binary    string
	sockPath  string
	rootDir   string
	env       []string
}

func (s *fakeDaemonStarter) start(ctx context.Context, netnsPath, binary, sockPath, rootDir string, env []string) (daemonProc, error) {
	s.calls = append(s.calls, daemonStartArgs{netnsPath: netnsPath, binary: binary, sockPath: sockPath, rootDir: rootDir, env: env})
	if s.startErr != nil {
		return nil, s.startErr
	}
	if s.proc == nil {
		s.proc = &fakeDaemonProc{}
	}
	return s.proc, nil
}

func newHardenedExecutorForTest(workDir string, backend *fakeSandboxBackend, starter *fakeDaemonStarter) *hardenedExecutor {
	// Explicit nameservers: the test machine's own resolvers (often
	// loopback-only) must not decide these tests. Resolver
	// inheritance is covered in sandbox_test.go.
	executor := newHardenedExecutor(workDir, backend, "buildkitd-test", []string{"10.0.0.53"})
	executor.startDaemon = starter.start
	return executor
}

// emulateBuildctl emulates a buildctl step inside the fake backend: it
// writes build metadata to the host path behind the step's guest
// --metadata-file flag.
func emulateBuildctl(t *testing.T, workDir, buildID, digest string) func(context.Context, SandboxNet, SandboxStep) error {
	t.Helper()
	return func(_ context.Context, _ SandboxNet, step SandboxStep) error {
		t.Helper()
		metadata := metadataFilePath(step.Argv)
		if metadata == "" {
			return errors.New("missing --metadata-file")
		}
		host, err := filepath.Rel("/build", metadata)
		if err != nil {
			return err
		}
		if step.OnLog != nil {
			step.OnLog(commandOutputLine{Stream: "stdout", Line: "fake build ok"})
		}
		return os.WriteFile(filepath.Join(workDir, buildID, host), []byte(`{"containerimage.digest":"`+digest+`"}`), 0o644)
	}
}

func TestHardenedExecutorIdentity(t *testing.T) {
	t.Parallel()

	executor := newHardenedExecutor(t.TempDir(), &fakeSandboxBackend{}, "buildkitd", nil)
	if executor.Name() != ExecutorHardened {
		t.Fatalf("unexpected executor name %q", executor.Name())
	}
	if !executor.Isolating() {
		t.Fatal("hardened executor must report itself isolating")
	}
}

func TestHardenedExecuteDockerfile(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	backend := &fakeSandboxBackend{}
	starter := &fakeDaemonStarter{}
	executor := newHardenedExecutorForTest(workDir, backend, starter)
	backend.runHook = emulateBuildctl(t, workDir, "build-1", "sha256:abc")

	var logs []commandOutputLine
	spec := testExecutionSpec(t, "build-1", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	spec.Cache = CachePolicy{Mode: CacheModeNone}
	spec.OnLog = func(line commandOutputLine) { logs = append(logs, line) }
	result, err := executor.Execute(context.Background(), spec)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ImageDigestRef != "registry.example.test/project-1/env-1/build/service@sha256:abc" {
		t.Fatalf("unexpected digest ref %q", result.ImageDigestRef)
	}
	if len(logs) != 1 || logs[0].Line != "fake build ok" {
		t.Fatalf("expected streamed build output, got %+v", logs)
	}

	steps := backend.recordedSteps()
	if len(steps) != 1 {
		t.Fatalf("expected one sandboxed step, got %d", len(steps))
	}
	step := steps[0]
	if step.Name != "build" || len(step.Argv) == 0 || step.Argv[0] != "buildctl" {
		t.Fatalf("unexpected step argv %#v", step.Argv)
	}
	// The step must address the per-execution daemon socket, never
	// the shared BuildKit address from the spec.
	if addr := buildctlAddrFlag(step.Argv); addr != "unix:///build/s/bk.sock" {
		t.Fatalf("buildctl addr = %q, want the per-execution socket", addr)
	}
	if step.Dir != "/build/scratch" {
		t.Fatalf("unexpected step dir %q", step.Dir)
	}
	env := map[string]string{}
	for _, entry := range step.Env {
		name, value, _ := strings.Cut(entry, "=")
		env[name] = value
	}
	if len(env) != 4 || env["HOME"] != "/build/scratch" || env["TMPDIR"] != "/build/tmp" {
		t.Fatalf("unexpected step env %#v", step.Env)
	}
	if !strings.HasPrefix(env["DOCKER_CONFIG"], "/build/scratch/docker-config-") {
		t.Fatalf("unexpected docker config %q", env["DOCKER_CONFIG"])
	}
	mountDests := map[string]SandboxMount{}
	for _, mount := range step.Mounts {
		mountDests[mount.Dest] = mount
	}
	if mountDests["/build"].Source != filepath.Join(workDir, "build-1") || mountDests["/build"].ReadOnly {
		t.Fatalf("unexpected /build mount %+v", mountDests["/build"])
	}
	if !mountDests["/build/repo"].ReadOnly {
		t.Fatalf("snapshot mount must be read-only: %+v", mountDests["/build/repo"])
	}
	if !mountDests["/etc/resolv.conf"].ReadOnly || !mountDests["/etc/hosts"].ReadOnly {
		t.Fatal("resolver mounts must be read-only")
	}
	if _, ok := mountDests["/build/cache"]; ok {
		t.Fatal("mode none must not mount a cache dir")
	}
	for _, arg := range step.Argv {
		if strings.HasPrefix(arg, "type=local") {
			t.Fatalf("mode none must not pass cache flags: %#v", step.Argv)
		}
	}

	if len(starter.calls) != 1 {
		t.Fatalf("expected one daemon start, got %d", len(starter.calls))
	}
	call := starter.calls[0]
	if call.binary != "buildkitd-test" || call.netnsPath != "/fake/netns/build-1" {
		t.Fatalf("unexpected daemon start %+v", call)
	}
	if !strings.HasSuffix(call.sockPath, filepath.Join("build-1", "s", "bk.sock")) {
		t.Fatalf("unexpected daemon socket %q", call.sockPath)
	}
	if starter.proc.readyCalls != 1 || starter.proc.stops != 1 {
		t.Fatalf("daemon must be awaited and stopped once, got %+v", starter.proc)
	}
	if len(backend.setupNets) != 1 || len(backend.teardowns) != 1 {
		t.Fatalf("expected setup+teardown, got %+v / %+v", backend.setupNets, backend.teardowns)
	}
	if _, err := os.Stat(filepath.Join(workDir, "build-1")); !os.IsNotExist(err) {
		t.Fatalf("workspace must be removed after execution, stat err=%v", err)
	}
}

func buildctlAddrFlag(argv []string) string {
	for i, arg := range argv {
		if arg == "--addr" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func TestHardenedExecuteRailpack(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	backend := &fakeSandboxBackend{}
	starter := &fakeDaemonStarter{}
	executor := newHardenedExecutorForTest(workDir, backend, starter)
	backend.runHook = func(ctx context.Context, net SandboxNet, step SandboxStep) error {
		if step.Name == "plan" {
			// The plan step's last path argument is the guest plan
			// file; find it by suffix.
			for _, arg := range step.Argv {
				rel, err := filepath.Rel("/build", arg)
				if err != nil || !strings.HasSuffix(rel, railpackPlanFilename) {
					continue
				}
				return os.WriteFile(filepath.Join(workDir, "build-1", rel), []byte(`{"plan":true}`), 0o644)
			}
			return errors.New("plan step wrote no plan path")
		}
		return emulateBuildctl(t, workDir, "build-1", "sha256:abc")(ctx, net, step)
	}

	archive := makeSnapshotArchive(t, map[string]string{"repo/app/main.py": "print('hi')\n"})
	spec := testExecutionSpec(t, "build-1", archive, &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_RAILPACK, ContextDir: "app"})
	if _, err := executor.Execute(context.Background(), spec); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	steps := backend.recordedSteps()
	if len(steps) != 2 || steps[0].Name != "plan" || steps[1].Name != "build" {
		t.Fatalf("expected plan+build steps, got %+v", steps)
	}
	if steps[0].Argv[0] != "railpack" {
		t.Fatalf("unexpected plan argv %#v", steps[0].Argv)
	}
	for _, entry := range steps[0].Env {
		if strings.HasPrefix(entry, "DOCKER_CONFIG=") {
			t.Fatalf("plan step must not receive push credentials: %#v", steps[0].Env)
		}
	}
	for _, entry := range steps[1].Env {
		if strings.HasPrefix(entry, "DOCKER_CONFIG=/build/") {
			return
		}
	}
	t.Fatalf("build step must receive the guest docker config: %#v", steps[1].Env)
}

func TestHardenedExecuteRejectsDockerBinary(t *testing.T) {
	t.Parallel()

	backend := &fakeSandboxBackend{}
	starter := &fakeDaemonStarter{}
	executor := newHardenedExecutorForTest(t.TempDir(), backend, starter)
	spec := testExecutionSpec(t, "build-1", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	spec.Buildkit.Binary = "docker"
	_, err := executor.Execute(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "requires buildctl") {
		t.Fatalf("expected buildctl requirement, got %v", err)
	}
	if len(backend.setupNets) != 0 || len(starter.calls) != 0 {
		t.Fatal("rejected build must not touch the sandbox backend or daemon")
	}
}

func TestHardenedExecuteRejectsMismatchedCacheKey(t *testing.T) {
	t.Parallel()

	backend := &fakeSandboxBackend{}
	starter := &fakeDaemonStarter{}
	executor := newHardenedExecutorForTest(t.TempDir(), backend, starter)
	spec := testExecutionSpec(t, "build-1", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	spec.Cache = CachePolicy{Mode: CacheModeContentAddressed, Key: "sha256:project-keyed"}
	_, err := executor.Execute(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "does not match build content") {
		t.Fatalf("expected cache key refusal, got %v", err)
	}
	if len(backend.setupNets) != 0 || len(starter.calls) != 0 {
		t.Fatal("rejected build must not touch the sandbox backend or daemon")
	}
}

func TestHardenedExecuteMountsContentCache(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	backend := &fakeSandboxBackend{}
	starter := &fakeDaemonStarter{}
	executor := newHardenedExecutorForTest(workDir, backend, starter)
	backend.runHook = emulateBuildctl(t, workDir, "build-1", "sha256:abc")

	spec := testExecutionSpec(t, "build-1", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	spec.Cache = CachePolicy{
		Mode: CacheModeContentAddressed,
		Key:  ContentCacheKey(spec.SnapshotDigest, spec.Recipe, spec.Railpack.FrontendImage),
	}
	if _, err := executor.Execute(context.Background(), spec); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	steps := backend.recordedSteps()
	if len(steps) != 1 {
		t.Fatalf("expected one step, got %d", len(steps))
	}
	var cache *SandboxMount
	for i, mount := range steps[0].Mounts {
		if mount.Dest == "/build/cache" {
			cache = &steps[0].Mounts[i]
		}
	}
	if cache == nil || cache.ReadOnly {
		t.Fatalf("expected writable cache mount, got %+v", steps[0].Mounts)
	}
	if !strings.HasPrefix(cache.Source, filepath.Join(workDir, "cache", "cache-sha256-")) {
		t.Fatalf("cache source must be content-keyed under the work dir, got %q", cache.Source)
	}
	if info, err := os.Stat(cache.Source); err != nil || !info.IsDir() {
		t.Fatalf("cache dir must exist, stat err=%v", err)
	}
	argv := strings.Join(steps[0].Argv, " ")
	if !strings.Contains(argv, "--export-cache type=local,dest=/build/cache") ||
		!strings.Contains(argv, "--import-cache type=local,src=/build/cache") {
		t.Fatalf("build step must round-trip the content cache: %#v", steps[0].Argv)
	}
}

func TestHardenedExecuteTimeout(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	backend := &fakeSandboxBackend{
		runHook: func(ctx context.Context, _ SandboxNet, _ SandboxStep) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	starter := &fakeDaemonStarter{}
	executor := newHardenedExecutorForTest(workDir, backend, starter)
	spec := testExecutionSpec(t, "build-1", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	spec.Limits.Timeout = 50 * time.Millisecond
	start := time.Now()
	_, err := executor.Execute(context.Background(), spec)
	if !errors.Is(err, ErrBuildTimeout) {
		t.Fatalf("expected ErrBuildTimeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout took too long: %s", elapsed)
	}
	if starter.proc.stops != 1 || len(backend.teardowns) != 1 {
		t.Fatal("timed-out execution must stop the daemon and tear down the network")
	}
	if _, err := os.Stat(filepath.Join(workDir, "build-1")); !os.IsNotExist(err) {
		t.Fatalf("timed-out workspace must be removed, stat err=%v", err)
	}
}

func TestHardenedExecuteCancellation(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	started := make(chan struct{})
	backend := &fakeSandboxBackend{
		runHook: func(ctx context.Context, _ SandboxNet, _ SandboxStep) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
	}
	starter := &fakeDaemonStarter{}
	executor := newHardenedExecutorForTest(workDir, backend, starter)
	spec := testExecutionSpec(t, "build-1", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := executor.Execute(ctx, spec)
		done <- err
	}()
	select {
	case <-started:
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

func TestHardenedExecuteClassifiesStepFailure(t *testing.T) {
	t.Parallel()

	backend := &fakeSandboxBackend{
		runErr: &SandboxStepError{Step: "build", ExitCode: 1, Tail: "error: failed to push: denied: insufficient_scope"},
	}
	starter := &fakeDaemonStarter{}
	executor := newHardenedExecutorForTest(t.TempDir(), backend, starter)
	spec := testExecutionSpec(t, "build-1", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	_, err := executor.Execute(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "push failure") {
		t.Fatalf("expected push classification, got %v", err)
	}

	backend.runErr = &SandboxStepError{Step: "build", ExitCode: 1, Tail: "error: dockerfile parse error"}
	_, err = executor.Execute(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "build failure") {
		t.Fatalf("expected build classification, got %v", err)
	}
}

func TestHardenedExecuteDaemonFailure(t *testing.T) {
	t.Parallel()

	backend := &fakeSandboxBackend{}
	starter := &fakeDaemonStarter{startErr: errors.New("buildkitd not found")}
	executor := newHardenedExecutorForTest(t.TempDir(), backend, starter)
	spec := testExecutionSpec(t, "build-1", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	_, err := executor.Execute(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "buildkitd not found") {
		t.Fatalf("expected daemon start error, got %v", err)
	}
	if len(backend.teardowns) != 1 {
		t.Fatal("failed execution must still tear down the network")
	}

	starter.startErr = nil
	starter.proc = &fakeDaemonProc{readyErr: errors.New("daemon crashed")}
	_, err = executor.Execute(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "daemon crashed") {
		t.Fatalf("expected daemon readiness error, got %v", err)
	}
}

func TestHardenedRecoverStaleWorkspaces(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	deadRoot := filepath.Join(workDir, "build-dead")
	if err := os.MkdirAll(deadRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := `{"executor":"hardened","pid":1073741824,"build_id":"build-dead"}`
	if err := os.WriteFile(filepath.Join(deadRoot, executorOwnerMarker), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	backend := &fakeSandboxBackend{reaped: 2}
	executor := newHardenedExecutor(workDir, backend, "buildkitd", nil)
	reclaimed, err := executor.RecoverStaleWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("RecoverStaleWorkspaces: %v", err)
	}
	if reclaimed != 3 {
		t.Fatalf("expected 3 reclaimed (1 workspace + 2 sandboxes), got %d", reclaimed)
	}
	backend.reapErr = errors.New("backend down")
	if _, err := executor.RecoverStaleWorkspaces(context.Background()); err == nil || !strings.Contains(err.Error(), "backend down") {
		t.Fatalf("expected backend error to propagate, got %v", err)
	}
}

func TestResolveExecutionCacheDir(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	spec := testExecutionSpec(t, "build-1", dockerfileArchiveForTest(t), dockerfileRecipeForTest())
	spec.Cache = CachePolicy{Mode: CacheModeNone}
	dir, err := resolveExecutionCacheDir(workDir, spec)
	if err != nil || dir != "" {
		t.Fatalf("mode none must resolve no dir, got %q, %v", dir, err)
	}
	spec.Cache = CachePolicy{
		Mode: CacheModeContentAddressed,
		Key:  ContentCacheKey(spec.SnapshotDigest, spec.Recipe, spec.Railpack.FrontendImage),
	}
	dir, err = resolveExecutionCacheDir(workDir, spec)
	if err != nil {
		t.Fatalf("content key must resolve: %v", err)
	}
	if !strings.HasPrefix(dir, filepath.Join(workDir, "cache", "cache-sha256-")) {
		t.Fatalf("unexpected cache dir %q", dir)
	}
	// Identical content from another project resolves the same dir:
	// the key carries no identity, so sharing it is safe.
	other := spec
	other.ProjectID = "project-2"
	other.BuildID = "build-2"
	otherDir, err := resolveExecutionCacheDir(workDir, other)
	if err != nil || otherDir != dir {
		t.Fatalf("identical content must share the cache dir, got %q, %v", otherDir, err)
	}
}

func TestSandboxStepError(t *testing.T) {
	t.Parallel()

	err := &SandboxStepError{Step: "build", ExitCode: 3, Tail: "boom"}
	if !strings.Contains(err.Error(), `"build"`) || !strings.Contains(err.Error(), "3") {
		t.Fatalf("unexpected step error %q", err)
	}
}
