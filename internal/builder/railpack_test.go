package builder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func snapshotChunksForTest(snapshotID string, archive []byte) (string, []*platformv1.SourceSnapshotChunk) {
	sum := sha256.Sum256(archive)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	var chunks []*platformv1.SourceSnapshotChunk
	for offset := 0; offset < len(archive); {
		end := min(offset+1024, len(archive))
		chunks = append(chunks, &platformv1.SourceSnapshotChunk{
			SnapshotId: snapshotID,
			Digest:     digest,
			TotalSize:  int64(len(archive)),
			Offset:     int64(offset),
			Data:       append([]byte(nil), archive[offset:end]...),
		})
		offset = end
	}
	return digest, chunks
}

func TestRailpackPlanCommandRequestsMissingStartFailure(t *testing.T) {
	t.Parallel()

	req := railpackPlanCommand("railpack", "/workspace/repo/apps/web", "/workspace/plan/railpack-plan.json")
	if req.Binary != "railpack" {
		t.Fatalf("unexpected binary %q", req.Binary)
	}
	want := []string{"plan", "/workspace/repo/apps/web", "--out", "/workspace/plan/railpack-plan.json", "--error-missing-start"}
	if !reflect.DeepEqual(req.Args, want) {
		t.Fatalf("unexpected plan args:\n got: %#v\nwant: %#v", req.Args, want)
	}
	if len(req.Env) != 0 {
		t.Fatalf("plan step must not receive registry credentials, got %#v", req.Env)
	}
}

func TestRailpackBuildctlCommandUsesGatewayFrontend(t *testing.T) {
	t.Parallel()

	req := railpackBuildctlCommand(
		"buildctl",
		"unix:///run/buildkit/buildkitd.sock",
		"ghcr.io/railwayapp/railpack-frontend:latest",
		"/workspace/repo",
		"/workspace/plan",
		"registry.example.test/platform/service:build-1",
		"/workspace/metadata.json",
		[]string{"DOCKER_CONFIG=/tmp/docker"},
	)
	want := []string{
		"--addr", "unix:///run/buildkit/buildkitd.sock",
		"build",
		"--frontend", "gateway.v0",
		"--opt", "source=ghcr.io/railwayapp/railpack-frontend:latest",
		"--local", "context=/workspace/repo",
		"--local", "dockerfile=/workspace/plan",
		"--opt", "filename=railpack-plan.json",
		"--output", "type=image,name=registry.example.test/platform/service:build-1,push=true",
		"--metadata-file", "/workspace/metadata.json",
	}
	if !reflect.DeepEqual(req.Args, want) {
		t.Fatalf("unexpected railpack buildctl args:\n got: %#v\nwant: %#v", req.Args, want)
	}
	if len(req.Env) != 1 || req.Env[0] != "DOCKER_CONFIG=/tmp/docker" {
		t.Fatalf("expected registry capability on build step, got %#v", req.Env)
	}
}

func TestExecuteRejectsUnspecifiedBuilder(t *testing.T) {
	t.Parallel()

	archive := makeSnapshotArchive(t, map[string]string{
		"repo/package.json": `{"scripts":{"start":"node server.js"}}`,
	})
	spec := testExecutionSpec(t, "build-1", archive, &platformv1.BuildRecipe{})
	executor := newDevelopmentExecutor(t.TempDir(), &scriptedCommandRunner{}, "/usr/bin:/bin")
	_, err := executor.Execute(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "builder is required") {
		t.Fatalf("expected builder-required error, got %v", err)
	}
}

func TestExecuteRailpackPlansThenBuilds(t *testing.T) {
	t.Parallel()

	archive := makeSnapshotArchive(t, map[string]string{
		"repo/package.json": `{"scripts":{"start":"node server.js"}}`,
		"repo/server.js":    "console.log(1)\n",
	})
	runner := &scriptedCommandRunner{
		handle: func(req commandRequest) ([]byte, error) {
			switch req.Binary {
			case "railpack":
				out := planOutPath(req.Args)
				if out == "" {
					return nil, errors.New("missing --out")
				}
				if err := os.WriteFile(out, []byte(`{"steps":[]}`), 0o644); err != nil {
					return nil, err
				}
				return []byte("plan ok\n"), nil
			case "buildctl":
				metadata := metadataFilePath(req.Args)
				if metadata == "" {
					return nil, errors.New("missing --metadata-file")
				}
				if err := os.WriteFile(metadata, []byte(`{"containerimage.digest":"sha256:abc"}`), 0o644); err != nil {
					return nil, err
				}
				return []byte("build ok\n"), nil
			default:
				return nil, errors.New("unexpected binary " + req.Binary)
			}
		},
	}
	var logged []commandOutputLine
	spec := testExecutionSpec(t, "build-1", archive, &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_RAILPACK, ContextDir: "."})
	spec.OnLog = func(line commandOutputLine) { logged = append(logged, line) }
	executor := newDevelopmentExecutor(t.TempDir(), runner, "/usr/bin:/bin")
	result, err := executor.Execute(context.Background(), spec)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ImageDigestRef != "registry.example.test/project-1/env-1/build/service@sha256:abc" {
		t.Fatalf("unexpected digest ref %q", result.ImageDigestRef)
	}
	requests := runner.Requests()
	if len(requests) != 2 {
		t.Fatalf("expected plan + build commands, got %d", len(requests))
	}
	if requests[0].Binary != "railpack" || requests[1].Binary != "buildctl" {
		t.Fatalf("unexpected command order %#v", requests)
	}
	if !slicesContains(requests[1].Args, "gateway.v0") {
		t.Fatalf("expected gateway frontend, got %#v", requests[1].Args)
	}
	if len(logged) == 0 {
		t.Fatal("expected build output to be streamed")
	}
}

func TestRailpackBuildCommandUsesBuildxFrontendSyntaxWhenDockerBinarySelected(t *testing.T) {
	t.Parallel()

	req := railpackBuildCommand(
		"docker",
		"docker-buildx",
		"ghcr.io/railwayapp/railpack-frontend:latest",
		"/workspace/repo",
		"/workspace/plan",
		"/workspace/plan/railpack-plan.json",
		"registry.example.test/platform/service:build-1",
		"/workspace/metadata.json",
		[]string{"DOCKER_CONFIG=/tmp/docker"},
	)
	want := []string{
		"buildx", "build",
		"--progress=plain",
		"--add-host", "host.docker.internal:host-gateway",
		"--build-arg", "BUILDKIT_SYNTAX=ghcr.io/railwayapp/railpack-frontend:latest",
		"--file", "/workspace/plan/railpack-plan.json",
		"--tag", "registry.example.test/platform/service:build-1",
		"--push",
		"--metadata-file", "/workspace/metadata.json",
		"/workspace/repo",
	}
	if req.Binary != "docker" {
		t.Fatalf("unexpected binary %q", req.Binary)
	}
	if !reflect.DeepEqual(req.Args, want) {
		t.Fatalf("unexpected railpack buildx args:\n got: %#v\nwant: %#v", req.Args, want)
	}
	if len(req.Env) != 1 || req.Env[0] != "DOCKER_CONFIG=/tmp/docker" {
		t.Fatalf("expected registry capability on build step, got %#v", req.Env)
	}
}

func TestExecuteRailpackBuildsWithDockerBinary(t *testing.T) {
	t.Parallel()

	archive := makeSnapshotArchive(t, map[string]string{
		"repo/package.json": `{"scripts":{"start":"node server.js"}}`,
	})
	runner := &scriptedCommandRunner{
		handle: func(req commandRequest) ([]byte, error) {
			switch req.Binary {
			case "railpack":
				out := planOutPath(req.Args)
				if out == "" {
					return nil, errors.New("missing --out")
				}
				if err := os.WriteFile(out, []byte(`{"steps":[]}`), 0o644); err != nil {
					return nil, err
				}
				return []byte("plan ok\n"), nil
			case "docker":
				metadata := metadataFilePath(req.Args)
				if metadata == "" {
					return nil, errors.New("missing --metadata-file")
				}
				if err := os.WriteFile(metadata, []byte(`{"containerimage.digest":"sha256:abc"}`), 0o644); err != nil {
					return nil, err
				}
				return []byte("build ok\n"), nil
			default:
				return nil, errors.New("unexpected binary " + req.Binary)
			}
		},
	}
	spec := testExecutionSpec(t, "build-1", archive, &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_RAILPACK, ContextDir: "."})
	spec.Buildkit.Binary = "docker"
	spec.Buildkit.Address = "docker-buildx"
	executor := newDevelopmentExecutor(t.TempDir(), runner, "/usr/bin:/bin")
	result, err := executor.Execute(context.Background(), spec)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ImageDigestRef != "registry.example.test/project-1/env-1/build/service@sha256:abc" {
		t.Fatalf("unexpected digest ref %q", result.ImageDigestRef)
	}
	requests := runner.Requests()
	if len(requests) != 2 {
		t.Fatalf("expected plan + build commands, got %d", len(requests))
	}
	if requests[0].Binary != "railpack" || requests[1].Binary != "docker" {
		t.Fatalf("unexpected command order %#v", requests)
	}
	if !slicesContains(requests[1].Args, "BUILDKIT_SYNTAX=ghcr.io/railwayapp/railpack-frontend:latest") {
		t.Fatalf("expected railpack frontend syntax, got %#v", requests[1].Args)
	}
}

func TestExecuteRailpackEnrichesMissingStartCommand(t *testing.T) {
	t.Parallel()

	archive := makeSnapshotArchive(t, map[string]string{
		"repo/package.json": `{"scripts":{"build":"tsc"}}`,
	})
	spec := testExecutionSpec(t, "build-1", archive, &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_RAILPACK, ContextDir: "."})
	executor := newDevelopmentExecutor(t.TempDir(), &scriptedCommandRunner{handle: func(commandRequest) ([]byte, error) {
		return []byte("no start command found\n"), errors.New("exit status 1")
	}}, "/usr/bin:/bin")
	_, err := executor.Execute(context.Background(), spec)
	if err == nil {
		t.Fatal("expected plan failure, got nil")
	}
	for _, token := range []string{"railpack analysis failed", "no start command found", "Node.js", "no start command"} {
		if !strings.Contains(err.Error(), token) {
			t.Fatalf("expected error to contain %q, got %v", token, err)
		}
	}
}

func TestExecuteRailpackEnrichesEmptySource(t *testing.T) {
	t.Parallel()

	archive := makeSnapshotArchive(t, map[string]string{
		"repo/README.md": "# hi\n",
	})
	spec := testExecutionSpec(t, "build-1", archive, &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_RAILPACK, ContextDir: "."})
	executor := newDevelopmentExecutor(t.TempDir(), &scriptedCommandRunner{handle: func(commandRequest) ([]byte, error) {
		return []byte("no providers found\n"), errors.New("exit status 1")
	}}, "/usr/bin:/bin")
	_, err := executor.Execute(context.Background(), spec)
	if err == nil {
		t.Fatal("expected plan failure, got nil")
	}
	for _, token := range []string{"railpack analysis failed", "no providers found", "no buildable source"} {
		if !strings.Contains(err.Error(), token) {
			t.Fatalf("expected error to contain %q, got %v", token, err)
		}
	}
}

func TestExecuteRailpackReportsMissingBinary(t *testing.T) {
	t.Parallel()

	archive := makeSnapshotArchive(t, map[string]string{
		"repo/package.json": `{"scripts":{"start":"node server.js"}}`,
	})
	spec := testExecutionSpec(t, "build-1", archive, &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_RAILPACK, ContextDir: "."})
	spec.Railpack.Binary = "railpack-missing"
	executor := newDevelopmentExecutor(t.TempDir(), &scriptedCommandRunner{handle: func(req commandRequest) ([]byte, error) {
		return nil, &exec.Error{Name: req.Binary, Err: exec.ErrNotFound}
	}}, "/usr/bin:/bin")
	_, err := executor.Execute(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "railpack executable not found") {
		t.Fatalf("expected missing binary error, got %v", err)
	}
}

func TestRailpackPlanFailureMarksTransientExitCode(t *testing.T) {
	t.Parallel()

	output, runErr := (osCommandRunner{}).Run(context.Background(), helperCommandRequest("exit75"), nil)
	if runErr == nil {
		t.Fatal("expected helper failure")
	}
	err := railpackPlanFailure(t.TempDir(), ".", commandRequest{Binary: "railpack", Args: []string{"plan"}}, runErr, output)
	if err == nil || !strings.Contains(err.Error(), "transient") || !strings.Contains(err.Error(), "exit 75") {
		t.Fatalf("expected transient marker, got %v", err)
	}
}

func TestBuildAndPushRailpackUsesSnapshotWorkspaceAndDigest(t *testing.T) {
	t.Parallel()

	archive := makeSnapshotArchive(t, map[string]string{
		"repo/package.json": `{"scripts":{"start":"node server.js"}}`,
		"repo/server.js":    "console.log(1)\n",
	})
	digest, chunks := snapshotChunksForTest("snapshot-1", archive)
	runner := &scriptedCommandRunner{
		handle: func(req commandRequest) ([]byte, error) {
			switch req.Binary {
			case "railpack":
				if out := planOutPath(req.Args); out != "" {
					if err := os.WriteFile(out, []byte(`{"steps":[]}`), 0o644); err != nil {
						return nil, err
					}
				}
				return []byte("plan ok\n"), nil
			case "buildctl":
				if metadata := metadataFilePath(req.Args); metadata != "" {
					if err := os.WriteFile(metadata, []byte(`{"containerimage.digest":"sha256:feed"}`), 0o644); err != nil {
						return nil, err
					}
				}
				return []byte("build ok\n"), nil
			default:
				return nil, errors.New("unexpected binary " + req.Binary)
			}
		},
	}
	workDir := t.TempDir()
	t.Cleanup(func() { _ = destroyExecutionWorkspace(filepath.Join(workDir, "build-9")) })
	app := &App{
		cfg:      testBuilderConfig("builder-1", workDir),
		client:   &recordingBuilderServiceClient{downloadChunks: chunks, calls: make(chan struct{}, 8)},
		executor: newDevelopmentExecutor(workDir, runner, "/usr/bin:/bin"),
	}
	ref, err := app.buildAndPush(context.Background(), &platformv1.BuildJob{
		BuildId: "build-9",
		Source: &platformv1.BuildJobSource{
			SourceSnapshotId:     "snapshot-1",
			SourceSnapshotDigest: digest,
			BuildRecipe:          &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_RAILPACK, ContextDir: "."},
		},
		RegistryPushReference: "registry.example.test/platform/service:build-9",
		RegistryUsername:      "alice",
		RegistryPassword:      "secret",
	})
	if err != nil {
		t.Fatalf("buildAndPush: %v", err)
	}
	if ref != "registry.example.test/platform/service@sha256:feed" {
		t.Fatalf("unexpected digest ref %q", ref)
	}
	requests := runner.Requests()
	if len(requests) != 2 || requests[0].Binary != "railpack" || requests[1].Binary != "buildctl" {
		t.Fatalf("unexpected commands %#v", requests)
	}
	planDir := requests[0].Args[1]
	body, err := os.ReadFile(filepath.Join(planDir, "server.js"))
	if err != nil || string(body) != "console.log(1)\n" {
		t.Fatalf("expected plan step to run in the snapshot workspace, read %q (%v)", string(body), err)
	}
}

func testBuilderConfig(id, workDir string) config.BuilderConfig {
	return config.BuilderConfig{
		ID:                       id,
		Name:                     id,
		WorkDir:                  workDir,
		BuildctlBinary:           "buildctl",
		BuildkitAddress:          "unix:///run/buildkit/buildkitd.sock",
		RailpackBinary:           "railpack",
		RailpackFrontendImage:    "ghcr.io/railwayapp/railpack-frontend:latest",
		Limits:                   testBuilderLimitsConfig(),
		Network:                  config.BuilderNetworkConfig{DeniedCIDRs: []string{"169.254.169.254/32"}},
		CleanupWorkDir:           false,
		PollIntervalSeconds:      1,
		HeartbeatIntervalSeconds: 1,
	}
}

func testBuilderLimitsConfig() config.BuilderLimitsConfig {
	return config.BuilderLimitsConfig{
		TimeoutSeconds:    60,
		MemoryBytes:       1 << 30,
		CPUSeconds:        60,
		MaxFileBytes:      1 << 30,
		MaxProcesses:      512,
		MaxWorkspaceBytes: 1 << 30,
	}
}

type scriptedCommandRunner struct {
	mu       sync.Mutex
	requests []commandRequest
	handle   func(commandRequest) ([]byte, error)
}

func (r *scriptedCommandRunner) Run(ctx context.Context, req commandRequest, onLine func(commandOutputLine)) ([]byte, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	r.mu.Unlock()
	if onLine != nil {
		onLine(commandOutputLine{ObservedAt: time.Now().UTC(), Stream: "stdout", Line: "mock: " + req.Binary})
	}
	if r.handle == nil {
		return nil, errors.New("no scripted handler")
	}
	return r.handle(req)
}

func (r *scriptedCommandRunner) Requests() []commandRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]commandRequest(nil), r.requests...)
}

func planOutPath(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--out" {
			return args[i+1]
		}
	}
	return ""
}

func metadataFilePath(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--metadata-file" {
			return args[i+1]
		}
	}
	return ""
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
