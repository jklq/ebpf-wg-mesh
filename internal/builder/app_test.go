package builder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestBuildctlCommandPlacesGlobalFlagsBeforeSubcommand(t *testing.T) {
	t.Parallel()

	req := buildctlCommand(
		"buildctl",
		"unix:///run/buildkit/buildkitd.sock",
		"/workspace/context",
		"/workspace/repo",
		"Dockerfile",
		"ghcr.io/example/image:tag",
		"/tmp/metadata.json",
		[]string{"DOCKER_CONFIG=/tmp/docker"},
	)

	wantArgs := []string{
		"--addr", "unix:///run/buildkit/buildkitd.sock",
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=/workspace/context",
		"--local", "dockerfile=/workspace/repo",
		"--opt", "filename=Dockerfile",
		"--output", "type=image,name=ghcr.io/example/image:tag,push=true",
		"--metadata-file", "/tmp/metadata.json",
	}
	if !reflect.DeepEqual(req.Args, wantArgs) {
		t.Fatalf("unexpected buildctl args:\n got: %#v\nwant: %#v", req.Args, wantArgs)
	}
}

func TestBuildCommandUsesDockerBuildxWhenDockerBinarySelected(t *testing.T) {
	t.Parallel()

	req := buildCommand(
		"docker",
		"docker-buildx",
		"/workspace/context",
		"/workspace/repo",
		"deploy/Dockerfile",
		"ghcr.io/example/image:tag",
		"/tmp/metadata.json",
		[]string{"DOCKER_CONFIG=/tmp/docker"},
	)

	wantArgs := []string{
		"buildx", "build",
		"--progress=plain",
		"--add-host", "host.docker.internal:host-gateway",
		"--file", "/workspace/repo/deploy/Dockerfile",
		"--tag", "ghcr.io/example/image:tag",
		"--push",
		"--metadata-file", "/tmp/metadata.json",
		"/workspace/context",
	}
	if req.Binary != "docker" {
		t.Fatalf("unexpected binary %q", req.Binary)
	}
	if !reflect.DeepEqual(req.Args, wantArgs) {
		t.Fatalf("unexpected docker buildx args:\n got: %#v\nwant: %#v", req.Args, wantArgs)
	}
}

func TestOSCommandRunnerEmitsStdoutAndStderrLinesWithStreamLabels(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		got    []commandOutputLine
		runner osCommandRunner
	)
	output, err := runner.Run(context.Background(), helperCommandRequest("streams"), func(line commandOutputLine) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, line)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("expected 4 streamed lines, got %d", len(got))
	}
	want := []commandOutputLine{
		{Stream: "stdout", Line: "out1"},
		{Stream: "stdout", Line: "out2"},
		{Stream: "stderr", Line: "err1"},
		{Stream: "stderr", Line: "err2"},
	}
	for _, line := range want {
		if !slices.ContainsFunc(got, func(candidate commandOutputLine) bool {
			return candidate.Stream == line.Stream && candidate.Line == line.Line && !candidate.ObservedAt.IsZero()
		}) {
			t.Fatalf("missing streamed line %+v in %+v", line, got)
		}
	}
	text := string(output)
	for _, token := range []string{"out1", "out2", "err1", "err2"} {
		if !strings.Contains(text, token) {
			t.Fatalf("combined output %q missing %q", text, token)
		}
	}
}

func TestOSCommandRunnerTruncatesLongLines(t *testing.T) {
	t.Parallel()

	var lines []commandOutputLine
	output, err := (osCommandRunner{}).Run(context.Background(), helperCommandRequest("longline"), func(line commandOutputLine) {
		lines = append(lines, line)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if lines[0].Stream != "stdout" {
		t.Fatalf("unexpected stream %q", lines[0].Stream)
	}
	if len(lines[0].Line) != commandScannerMaxLineBytes {
		t.Fatalf("expected truncated line length %d, got %d", commandScannerMaxLineBytes, len(lines[0].Line))
	}
	if len(output) > commandFailureOutputBytes {
		t.Fatalf("expected output tail <= %d bytes, got %d", commandFailureOutputBytes, len(output))
	}
}

func TestOSCommandRunnerPreservesFailureOutputTail(t *testing.T) {
	t.Parallel()

	output, err := (osCommandRunner{}).Run(context.Background(), helperCommandRequest("failure"), nil)
	if err == nil {
		t.Fatal("expected command failure")
	}
	text := string(output)
	for _, token := range []string{"hello", "boom"} {
		if !strings.Contains(text, token) {
			t.Fatalf("failure output %q missing %q", text, token)
		}
	}
}

func TestBuildLogReporterBatchesLinesWithIncreasingSequence(t *testing.T) {
	t.Parallel()

	client := &recordingBuilderServiceClient{calls: make(chan struct{}, 8)}
	reporter := newBuildLogReporter(context.Background(), client, "builder-1", "build-1")
	now := time.Now().UTC()
	for i := 0; i < buildLogBatchSize+1; i++ {
		reporter.Report(context.Background(), commandOutputLine{
			ObservedAt: now.Add(time.Duration(i) * time.Millisecond),
			Stream:     "stdout",
			Line:       "line",
		})
	}
	waitForBuilderReportCall(t, client.calls)
	reporter.Close()

	requests := client.ReportRequests()
	if len(requests) != 2 {
		t.Fatalf("expected 2 report requests, got %d", len(requests))
	}
	var sequences []uint64
	for _, req := range requests {
		for _, line := range req.GetLines() {
			sequences = append(sequences, line.GetSequence())
		}
	}
	if len(sequences) != buildLogBatchSize+1 {
		t.Fatalf("expected %d sequences, got %d", buildLogBatchSize+1, len(sequences))
	}
	for i, sequence := range sequences {
		if want := uint64(i + 1); sequence != want {
			t.Fatalf("sequence %d = %d, want %d", i, sequence, want)
		}
	}
}

func TestBuildLogReporterFlushesRemainingLinesOnClose(t *testing.T) {
	t.Parallel()

	client := &recordingBuilderServiceClient{calls: make(chan struct{}, 4)}
	reporter := newBuildLogReporter(context.Background(), client, "builder-1", "build-1")
	reporter.Report(context.Background(), commandOutputLine{ObservedAt: time.Now().UTC(), Stream: "stdout", Line: "one"})
	reporter.Report(context.Background(), commandOutputLine{ObservedAt: time.Now().UTC(), Stream: "stderr", Line: "two"})
	reporter.Close()

	requests := client.ReportRequests()
	if len(requests) != 1 {
		t.Fatalf("expected 1 report request, got %d", len(requests))
	}
	if got := len(requests[0].GetLines()); got != 2 {
		t.Fatalf("expected 2 flushed lines, got %d", got)
	}
}

func TestBuildLogReporterIgnoresReportErrors(t *testing.T) {
	t.Parallel()

	client := &recordingBuilderServiceClient{
		calls:     make(chan struct{}, 4),
		reportErr: errors.New("boom"),
	}
	reporter := newBuildLogReporter(context.Background(), client, "builder-1", "build-1")
	reporter.Report(context.Background(), commandOutputLine{ObservedAt: time.Now().UTC(), Stream: "stdout", Line: "one"})
	reporter.Close()

	if len(client.ReportRequests()) != 1 {
		t.Fatal("expected reporter to attempt a flush despite RPC errors")
	}
}

func TestValidateBuildInputsRejectsEscapingPaths(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "deploy"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "deploy", "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(Dockerfile): %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(repoDir, "deploy", "escape")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	if _, _, err := validateBuildInputs(repoDir, &platformv1.BuildRecipe{
		ContextDir:     "..",
		DockerfilePath: "deploy/Dockerfile",
	}); err == nil || !strings.Contains(err.Error(), "context dir") {
		t.Fatalf("expected escaping context dir to be rejected, got %v", err)
	}

	if _, _, err := validateBuildInputs(repoDir, &platformv1.BuildRecipe{
		ContextDir:     "deploy",
		DockerfilePath: "../Dockerfile",
	}); err == nil || !strings.Contains(err.Error(), "dockerfile path") {
		t.Fatalf("expected escaping dockerfile path to be rejected, got %v", err)
	}

	if _, _, err := validateBuildInputs(repoDir, &platformv1.BuildRecipe{
		ContextDir:     "deploy/escape",
		DockerfilePath: "deploy/Dockerfile",
	}); err == nil || !strings.Contains(err.Error(), "symlink escapes repository") {
		t.Fatalf("expected symlink escape to be rejected, got %v", err)
	}
}

func TestValidateBuildInputsUsesResolvedRepoRootForDockerfileRelativePath(t *testing.T) {
	t.Parallel()

	targetDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(targetDir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(Dockerfile): %v", err)
	}

	linkDir := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(targetDir, linkDir); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	contextDir, dockerfilePath, err := validateBuildInputs(linkDir, &platformv1.BuildRecipe{})
	if err != nil {
		t.Fatalf("validateBuildInputs: %v", err)
	}
	resolvedContextDir, err := filepath.EvalSymlinks(contextDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(contextDir): %v", err)
	}
	resolvedTargetDir, err := filepath.EvalSymlinks(targetDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(targetDir): %v", err)
	}
	if resolvedContextDir != resolvedTargetDir {
		t.Fatalf("unexpected context dir %q", contextDir)
	}
	if dockerfilePath != "Dockerfile" {
		t.Fatalf("unexpected dockerfile path %q", dockerfilePath)
	}
}

func TestDockerConfigEnvMergesBaseConfigAndPreservesDockerSupportDirs(t *testing.T) {
	homeDir := t.TempDir()
	baseConfigDir := filepath.Join(homeDir, ".docker")
	for _, name := range []string{"cli-plugins", "buildx", "contexts"} {
		if err := os.MkdirAll(filepath.Join(baseConfigDir, name), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", name, err)
		}
	}
	baseConfig := map[string]any{
		"credsStore": "desktop",
		"credHelpers": map[string]any{
			"example.com": "example-helper",
			"ghcr.io":     "stale-target-helper",
		},
		"auths": map[string]any{
			"example.com": map[string]any{"auth": "existing"},
		},
	}
	baseConfigJSON, err := json.Marshal(baseConfig)
	if err != nil {
		t.Fatalf("Marshal(baseConfig): %v", err)
	}
	if err := os.WriteFile(filepath.Join(baseConfigDir, "config.json"), baseConfigJSON, 0o600); err != nil {
		t.Fatalf("WriteFile(config.json): %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("DOCKER_CONFIG", "")

	env, cleanup, err := dockerConfigEnv(t.TempDir(), "ghcr.io/acme/app:tag", "alice", "secret")
	if err != nil {
		t.Fatalf("dockerConfigEnv: %v", err)
	}
	defer cleanup()
	if len(env) != 1 || !strings.HasPrefix(env[0], "DOCKER_CONFIG=") {
		t.Fatalf("unexpected env %v", env)
	}
	configDir := strings.TrimPrefix(env[0], "DOCKER_CONFIG=")
	data, err := os.ReadFile(filepath.Join(configDir, "config.json"))
	if err != nil {
		t.Fatalf("ReadFile(config.json): %v", err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("Unmarshal(config.json): %v", err)
	}
	if got := config["credsStore"]; got != "desktop" {
		t.Fatalf("unexpected credsStore %v", got)
	}
	credentialHelpers, ok := config["credHelpers"].(map[string]any)
	if !ok {
		t.Fatalf("credHelpers missing or wrong type: %#v", config["credHelpers"])
	}
	if got := credentialHelpers["ghcr.io"]; got != "" {
		t.Fatalf("target registry helper must be disabled, got %v", got)
	}
	if got := credentialHelpers["example.com"]; got != "example-helper" {
		t.Fatalf("existing registry helper was not preserved, got %v", got)
	}
	auths, ok := config["auths"].(map[string]any)
	if !ok {
		t.Fatalf("auths missing or wrong type: %#v", config["auths"])
	}
	entry, ok := auths["ghcr.io"].(map[string]any)
	if !ok {
		t.Fatalf("ghcr.io auth missing: %#v", auths)
	}
	wantAuth := base64.StdEncoding.EncodeToString([]byte("alice:secret"))
	if got := entry["auth"]; got != wantAuth {
		t.Fatalf("unexpected auth %v", got)
	}
	if _, ok := auths["example.com"]; !ok {
		t.Fatalf("expected existing auths to be preserved: %#v", auths)
	}
	for _, name := range []string{"cli-plugins", "buildx", "contexts"} {
		target, err := os.Readlink(filepath.Join(configDir, name))
		if err != nil {
			t.Fatalf("Readlink(%s): %v", name, err)
		}
		if target != filepath.Join(baseConfigDir, name) {
			t.Fatalf("unexpected %s link target %q", name, target)
		}
	}
}

func TestExtractSourceSnapshotStripsArchiveRoot(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	archive := makeSnapshotArchive(t, map[string]string{
		"repo-root/deploy/Dockerfile": "FROM scratch\n",
		"repo-root/app/main.txt":      "hello\n",
	})
	if err := extractSourceSnapshot(repoDir, archive); err != nil {
		t.Fatalf("extractSourceSnapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "deploy", "Dockerfile")); err != nil {
		t.Fatalf("expected dockerfile to be extracted: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(repoDir, "app", "main.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("unexpected extracted content %q", string(data))
	}
}

func TestMaterializeSourceSnapshotStreamsAndVerifiesArchive(t *testing.T) {
	t.Parallel()

	archive := makeSnapshotArchive(t, map[string]string{
		"repo/Dockerfile":  "FROM scratch\n",
		"repo/app/main.go": "package main\n",
	})
	digest := sha256.Sum256(archive)
	digestString := "sha256:" + hex.EncodeToString(digest[:])
	chunks := make([]*platformv1.SourceSnapshotChunk, 0)
	for offset := 0; offset < len(archive); {
		end := min(offset+17, len(archive))
		chunks = append(chunks, &platformv1.SourceSnapshotChunk{
			SnapshotId: "snapshot-1",
			Digest:     digestString,
			TotalSize:  int64(len(archive)),
			Offset:     int64(offset),
			Data:       append([]byte(nil), archive[offset:end]...),
		})
		offset = end
	}
	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	app := &App{client: &recordingBuilderServiceClient{downloadChunks: chunks}}
	err := app.materializeSourceSnapshot(context.Background(), &platformv1.BuildJob{Source: &platformv1.BuildJobSource{
		SourceSnapshotId:     "snapshot-1",
		SourceSnapshotDigest: digestString,
	}}, repoDir)
	if err != nil {
		t.Fatalf("materializeSourceSnapshot: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(repoDir, "app", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "package main\n" {
		t.Fatalf("unexpected reconstructed source: %q", body)
	}
}

func TestMaterializeSourceSnapshotRejectsDigestMismatch(t *testing.T) {
	t.Parallel()

	archive := makeSnapshotArchive(t, map[string]string{"repo/Dockerfile": "FROM scratch\n"})
	badDigest := "sha256:" + strings.Repeat("0", sha256.Size*2)
	app := &App{client: &recordingBuilderServiceClient{downloadChunks: []*platformv1.SourceSnapshotChunk{{
		SnapshotId: "snapshot-1",
		Digest:     badDigest,
		TotalSize:  int64(len(archive)),
		Data:       archive,
	}}}}
	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	err := app.materializeSourceSnapshot(context.Background(), &platformv1.BuildJob{Source: &platformv1.BuildJobSource{
		SourceSnapshotId: "snapshot-1",
	}}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "digest verification failed") {
		t.Fatalf("expected digest verification failure, got %v", err)
	}
}

func TestParseBuildMetadataReturnsDigest(t *testing.T) {
	t.Parallel()

	digest, err := parseBuildMetadata([]byte(`{"containerimage.digest":"sha256:abc"}`))
	if err != nil {
		t.Fatalf("parseBuildMetadata: %v", err)
	}
	if digest != "sha256:abc" {
		t.Fatalf("unexpected digest %q", digest)
	}

	digest, err = parseBuildMetadata([]byte(`{
		"buildx.build.ref":"desktop-linux/desktop-linux/example",
		"containerimage.config.digest":"sha256:def",
		"containerimage.descriptor":{
			"mediaType":"application/vnd.docker.distribution.manifest.v2+json",
			"digest":"sha256:xyz",
			"size":304,
			"platform":{"architecture":"arm64","os":"linux"}
		},
		"containerimage.digest":"sha256:xyz",
		"image.name":"ghcr.io/example/app:tag"
	}`))
	if err != nil {
		t.Fatalf("parseBuildMetadata(buildx): %v", err)
	}
	if digest != "sha256:xyz" {
		t.Fatalf("unexpected buildx digest %q", digest)
	}

	if _, err := parseBuildMetadata([]byte(`{"containerimage.digest":""}`)); err == nil {
		t.Fatal("expected missing digest to fail")
	}
}

func TestRuntimeDigestRefPreservesRegistryPort(t *testing.T) {
	t.Parallel()
	got := runtimeDigestRef("registry.example.test:5000/platform/service:git-deadbeef", "sha256:abc")
	want := "registry.example.test:5000/platform/service@sha256:abc"
	if got != want {
		t.Fatalf("runtimeDigestRef() = %q, want %q", got, want)
	}
}

func makeSnapshotArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for name, body := range files {
		data := []byte(body)
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(data)),
		}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close tar writer: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("Close gzip writer: %v", err)
	}
	return buf.Bytes()
}

func TestCommandRunnerHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	switch os.Getenv("HELPER_MODE") {
	case "streams":
		_, _ = os.Stdout.WriteString("out1\nout2\n")
		_, _ = os.Stderr.WriteString("err1\nerr2\n")
		os.Exit(0)
	case "longline":
		_, _ = os.Stdout.WriteString(strings.Repeat("x", commandScannerMaxLineBytes+1024) + "\n")
		os.Exit(0)
	case "failure":
		_, _ = os.Stdout.WriteString("hello\n")
		_, _ = os.Stderr.WriteString("boom\n")
		os.Exit(7)
	default:
		os.Exit(2)
	}
}

func helperCommandRequest(mode string) commandRequest {
	return commandRequest{
		Binary: os.Args[0],
		Args:   []string{"-test.run=TestCommandRunnerHelperProcess"},
		Env: []string{
			"GO_WANT_HELPER_PROCESS=1",
			"HELPER_MODE=" + mode,
		},
	}
}

type recordingBuilderServiceClient struct {
	mu             sync.Mutex
	requests       []*platformv1.ReportBuildLogsRequest
	calls          chan struct{}
	reportErr      error
	downloadChunks []*platformv1.SourceSnapshotChunk
	downloadErr    error
}

func (c *recordingBuilderServiceClient) ClaimBuild(context.Context, *platformv1.ClaimBuildRequest, ...grpc.CallOption) (*platformv1.BuildJob, error) {
	return nil, nil
}

func (c *recordingBuilderServiceClient) DownloadSourceSnapshot(context.Context, *platformv1.DownloadSourceSnapshotRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[platformv1.SourceSnapshotChunk], error) {
	if c.downloadErr != nil {
		return nil, c.downloadErr
	}
	return &sourceSnapshotTestStream{chunks: c.downloadChunks}, nil
}

type sourceSnapshotTestStream struct {
	chunks []*platformv1.SourceSnapshotChunk
	next   int
}

func (s *sourceSnapshotTestStream) Recv() (*platformv1.SourceSnapshotChunk, error) {
	if s.next >= len(s.chunks) {
		return nil, io.EOF
	}
	chunk := s.chunks[s.next]
	s.next++
	return chunk, nil
}

func (s *sourceSnapshotTestStream) Header() (metadata.MD, error) { return nil, nil }
func (s *sourceSnapshotTestStream) Trailer() metadata.MD         { return nil }
func (s *sourceSnapshotTestStream) CloseSend() error             { return nil }
func (s *sourceSnapshotTestStream) Context() context.Context     { return context.Background() }
func (s *sourceSnapshotTestStream) SendMsg(any) error            { return nil }
func (s *sourceSnapshotTestStream) RecvMsg(message any) error {
	chunk, err := s.Recv()
	if err != nil {
		return err
	}
	target, ok := message.(*platformv1.SourceSnapshotChunk)
	if !ok {
		return errors.New("unexpected stream message type")
	}
	proto.Reset(target)
	proto.Merge(target, chunk)
	return nil
}

func (c *recordingBuilderServiceClient) ReportBuildHeartbeat(context.Context, *platformv1.BuilderHeartbeatRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (c *recordingBuilderServiceClient) ReportBuildLogs(_ context.Context, in *platformv1.ReportBuildLogsRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	c.mu.Lock()
	c.requests = append(c.requests, cloneBuildLogRequestForTest(in))
	c.mu.Unlock()
	select {
	case c.calls <- struct{}{}:
	default:
	}
	if c.reportErr != nil {
		return nil, c.reportErr
	}
	return &emptypb.Empty{}, nil
}

func (c *recordingBuilderServiceClient) CompleteBuild(context.Context, *platformv1.CompleteBuildRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (c *recordingBuilderServiceClient) ReportRequests() []*platformv1.ReportBuildLogsRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*platformv1.ReportBuildLogsRequest, 0, len(c.requests))
	for _, req := range c.requests {
		out = append(out, cloneBuildLogRequestForTest(req))
	}
	return out
}

func cloneBuildLogRequestForTest(req *platformv1.ReportBuildLogsRequest) *platformv1.ReportBuildLogsRequest {
	if req == nil {
		return nil
	}
	clone := &platformv1.ReportBuildLogsRequest{
		BuilderId: req.GetBuilderId(),
		BuildId:   req.GetBuildId(),
		Lines:     make([]*platformv1.BuildLogLine, 0, len(req.GetLines())),
	}
	for _, line := range req.GetLines() {
		if line == nil {
			continue
		}
		clone.Lines = append(clone.Lines, &platformv1.BuildLogLine{
			ObservedAt: line.GetObservedAt(),
			Stream:     line.GetStream(),
			Sequence:   line.GetSequence(),
			Line:       line.GetLine(),
		})
	}
	return clone
}

func waitForBuilderReportCall(t *testing.T, calls <-chan struct{}) {
	t.Helper()
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for build log report")
	}
}
