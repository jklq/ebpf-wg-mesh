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
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
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
		"unix:///run/docker.sock",
		"/workspace/context",
		"/workspace/repo",
		"deploy/Dockerfile",
		"ghcr.io/example/image:tag",
		"/tmp/metadata.json",
		[]string{"DOCKER_CONFIG=/tmp/docker"},
	)

	wantArgs := []string{
		"--host", "unix:///run/docker.sock", "buildx", "build",
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

func TestScopedDockerConfigHoldsExactlyOneRepository(t *testing.T) {
	homeDir := t.TempDir()
	baseConfigDir := filepath.Join(homeDir, ".docker")
	for _, name := range []string{"cli-plugins", "buildx", "contexts"} {
		if err := os.MkdirAll(filepath.Join(baseConfigDir, name), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", name, err)
		}
	}
	rogueConfig := map[string]any{
		"credsStore": "desktop",
		"credHelpers": map[string]any{
			"example.com": "example-helper",
			"ghcr.io":     "stale-target-helper",
		},
		"auths": map[string]any{
			"example.com": map[string]any{"auth": "rogue"},
		},
	}
	rogueConfigJSON, err := json.Marshal(rogueConfig)
	if err != nil {
		t.Fatalf("Marshal(rogueConfig): %v", err)
	}
	if err := os.WriteFile(filepath.Join(baseConfigDir, "config.json"), rogueConfigJSON, 0o600); err != nil {
		t.Fatalf("WriteFile(config.json): %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("DOCKER_CONFIG", "")

	configDir, cleanup, err := scopedDockerConfig(t.TempDir(), "ghcr.io/acme/app:tag", "alice", "secret", true)
	if err != nil {
		t.Fatalf("scopedDockerConfig: %v", err)
	}
	defer cleanup()
	data, err := os.ReadFile(filepath.Join(configDir, "config.json"))
	if err != nil {
		t.Fatalf("ReadFile(config.json): %v", err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("Unmarshal(config.json): %v", err)
	}
	if _, ok := config["credsStore"]; ok {
		t.Fatalf("ambient credential store must not enter the build: %#v", config["credsStore"])
	}
	auths, ok := config["auths"].(map[string]any)
	if !ok {
		t.Fatalf("auths missing or wrong type: %#v", config["auths"])
	}
	if len(auths) != 1 {
		t.Fatalf("expected exactly one auth entry, got %#v", auths)
	}
	entry, ok := auths["ghcr.io"].(map[string]any)
	if !ok {
		t.Fatalf("ghcr.io auth missing: %#v", auths)
	}
	wantAuth := base64.StdEncoding.EncodeToString([]byte("alice:secret"))
	if got := entry["auth"]; got != wantAuth {
		t.Fatalf("unexpected auth %v", got)
	}
	credentialHelpers, ok := config["credHelpers"].(map[string]any)
	if !ok {
		t.Fatalf("credHelpers missing or wrong type: %#v", config["credHelpers"])
	}
	if len(credentialHelpers) != 1 || credentialHelpers["ghcr.io"] != "" {
		t.Fatalf("only the target registry helper may be pinned, got %#v", credentialHelpers)
	}
	for _, name := range []string{"cli-plugins", "buildx"} {
		target, err := os.Readlink(filepath.Join(configDir, name))
		if err != nil {
			t.Fatalf("Readlink(%s): %v", name, err)
		}
		if target != filepath.Join(baseConfigDir, name) {
			t.Fatalf("unexpected %s link target %q", name, target)
		}
	}
	if _, err := os.Lstat(filepath.Join(configDir, "contexts")); !os.IsNotExist(err) {
		t.Fatalf("ambient docker contexts must not enter the build, lstat err=%v", err)
	}
	if _, _, err := scopedDockerConfig(t.TempDir(), "not-a-reference", "alice", "secret", true); err == nil {
		t.Fatal("expected invalid push reference to fail")
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

func TestExtractSourceSnapshotSkipsPAXGlobalHeader(t *testing.T) {
	var archive bytes.Buffer
	gzw := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gzw)
	if err := tw.WriteHeader(&tar.Header{
		Name:       "pax_global_header",
		Typeflag:   tar.TypeXGlobalHeader,
		PAXRecords: map[string]string{"comment": "source archive"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "repo-root/Dockerfile", Mode: 0o644, Size: 13}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("FROM scratch\n")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	repoDir := t.TempDir()
	if err := extractSourceSnapshot(repoDir, archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repoDir, "Dockerfile"))
	if err != nil || string(data) != "FROM scratch\n" {
		t.Fatalf("Dockerfile = %q, %v", data, err)
	}
}

func TestExtractSourceSnapshotRejectsMultipleRoots(t *testing.T) {
	archive := makeSnapshotArchive(t, map[string]string{
		"first/app.txt":  "first",
		"second/app.txt": "second",
	})
	if err := extractSourceSnapshot(t.TempDir(), archive); err == nil {
		t.Fatal("expected multiple archive roots to be rejected")
	}
}

func TestExtractSourceSnapshotPreservesInternalLinks(t *testing.T) {
	var archive bytes.Buffer
	gzw := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gzw)
	for _, header := range []*tar.Header{
		{Name: "repo/main.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
		{Name: "repo/alias.txt", Typeflag: tar.TypeSymlink, Linkname: "main.txt"},
		{Name: "repo/copy.txt", Typeflag: tar.TypeLink, Linkname: "repo/main.txt"},
	} {
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte("data")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := extractSourceSnapshot(repo, archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alias.txt", "copy.txt"} {
		data, err := os.ReadFile(filepath.Join(repo, name))
		if err != nil || string(data) != "data" {
			t.Fatalf("%s = %q, %v", name, data, err)
		}
	}
}

func TestExtractSourceSnapshotRejectsEscapingLinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		hdr  tar.Header
	}{
		{"absolute symlink", tar.Header{Name: "repo/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}},
		{"parent symlink", tar.Header{Name: "repo/link", Typeflag: tar.TypeSymlink, Linkname: "../outside"}},
		{"hardlink outside root", tar.Header{Name: "repo/link", Typeflag: tar.TypeLink, Linkname: "other/file"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var archive bytes.Buffer
			gz := gzip.NewWriter(&archive)
			tarWriter := tar.NewWriter(gz)
			if err := tarWriter.WriteHeader(&tc.hdr); err != nil {
				t.Fatal(err)
			}
			if err := tarWriter.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			if err := extractSourceSnapshot(t.TempDir(), archive.Bytes()); err == nil {
				t.Fatal("expected escaping link to be rejected")
			}
		})
	}
}

func TestDownloadSourceSnapshotStreamsAndVerifiesArchive(t *testing.T) {
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
	app := &App{client: &recordingBuilderServiceClient{downloadChunks: chunks}}
	archivePath, gotDigest, err := app.downloadSourceSnapshot(context.Background(), &platformv1.BuildJob{Source: &platformv1.BuildJobSource{
		SourceSnapshotId:     "snapshot-1",
		SourceSnapshotDigest: digestString,
	}})
	if err != nil {
		t.Fatalf("downloadSourceSnapshot: %v", err)
	}
	defer os.Remove(archivePath)
	if gotDigest != digestString {
		t.Fatalf("unexpected verified digest %q", gotDigest)
	}
	staged, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(staged, archive) {
		t.Fatal("staged archive does not match streamed bytes")
	}
}

func TestDownloadSourceSnapshotRejectsDigestMismatch(t *testing.T) {
	t.Parallel()

	archive := makeSnapshotArchive(t, map[string]string{"repo/Dockerfile": "FROM scratch\n"})
	badDigest := "sha256:" + strings.Repeat("0", sha256.Size*2)
	app := &App{client: &recordingBuilderServiceClient{downloadChunks: []*platformv1.SourceSnapshotChunk{{
		SnapshotId: "snapshot-1",
		Digest:     badDigest,
		TotalSize:  int64(len(archive)),
		Data:       archive,
	}}}}
	_, _, err := app.downloadSourceSnapshot(context.Background(), &platformv1.BuildJob{Source: &platformv1.BuildJobSource{
		SourceSnapshotId: "snapshot-1",
	}})
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

func TestClassifyBuildctlFailureIgnoresCommandText(t *testing.T) {
	t.Parallel()
	withPushArgs := commandRequest{
		Binary: "buildctl",
		Args:   []string{"build", "--output", "type=image,name=ghcr.io/example/image:tag,push=true"},
	}
	cases := []struct {
		name   string
		req    commandRequest
		runErr error
		output string
		want   string
	}{
		{
			name:   "compile failure with push args stays a build failure",
			req:    withPushArgs,
			runErr: errors.New("exit status 1"),
			output: "ERROR: failed to solve: dockerfile parse error line 3: unknown instruction",
			want:   failureKindBuild,
		},
		{
			name:   "docker push flag with empty output stays a build failure",
			req:    commandRequest{Binary: "docker", Args: []string{"--host", "unix:///run/docker.sock", "buildx", "build", "--push", "."}},
			runErr: errors.New("exit status 1"),
			want:   failureKindBuild,
		},
		{
			name:   "denied output is a push failure",
			req:    withPushArgs,
			runErr: errors.New("exit status 1"),
			output: "denied: requested access to the resource is denied",
			want:   failureKindPush,
		},
		{
			name:   "failed to push output is a push failure",
			req:    withPushArgs,
			runErr: errors.New("exit status 1"),
			output: "error: failed to push ghcr.io/example/image:tag: unexpected status: 401",
			want:   failureKindPush,
		},
		{
			name:   "unauthorized run error is a push failure",
			req:    withPushArgs,
			runErr: errors.New("unauthorized: authentication required"),
			want:   failureKindPush,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := classifyBuildctlFailure(tc.req, tc.runErr, []byte(tc.output))
			var buildErr *buildFailureError
			if !errors.As(err, &buildErr) {
				t.Fatalf("classifyBuildctlFailure() = %v, want *buildFailureError", err)
			}
			if buildErr.kind != tc.want {
				t.Fatalf("kind = %q, want %q (err: %v)", buildErr.kind, tc.want, err)
			}
		})
	}
}

func extractSourceSnapshot(repoDir string, archiveTGZ []byte) error {
	return extractSourceSnapshotReader(repoDir, bytes.NewReader(archiveTGZ), int64(len(archiveTGZ)))
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
	case "exit75":
		_, _ = os.Stderr.WriteString("temporary failure in name resolution\n")
		os.Exit(75)
	case "spin":
		for {
		}
	case "alloc":
		megabytes, _ := strconv.Atoi(os.Getenv("HELPER_MB"))
		if megabytes <= 0 {
			os.Exit(2)
		}
		slab := make([]byte, megabytes<<20)
		for i := 0; i < len(slab); i += 1 << 20 {
			slab[i] = 1
		}
		_, _ = os.Stdout.WriteString("allocated\n")
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "bigfile":
		size, _ := strconv.Atoi(os.Getenv("HELPER_BYTES"))
		path := os.Getenv("HELPER_FILE")
		if size <= 0 || path == "" {
			os.Exit(2)
		}
		if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
			_, _ = os.Stderr.WriteString(err.Error() + "\n")
			os.Exit(1)
		}
		os.Exit(0)
	case "fork":
		children, _ := strconv.Atoi(os.Getenv("HELPER_CHILDREN"))
		if children <= 0 {
			os.Exit(2)
		}
		var procs []*os.Process
		failed := false
		for i := 0; i < children; i++ {
			cmd := exec.Command("sleep", "30")
			if err := cmd.Start(); err != nil {
				failed = true
				break
			}
			procs = append(procs, cmd.Process)
		}
		for _, proc := range procs {
			_ = proc.Kill()
			_, _ = proc.Wait()
		}
		if failed {
			os.Exit(1)
		}
		os.Exit(0)
	case "spawn":
		pidFile := os.Getenv("HELPER_PIDFILE")
		if pidFile == "" {
			os.Exit(2)
		}
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			os.Exit(1)
		}
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
			os.Exit(1)
		}
		_ = cmd.Wait()
		os.Exit(0)
	case "sleep":
		time.Sleep(60 * time.Second)
		os.Exit(0)
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
	failRemaining  int
	failErr        error
	downloadChunks []*platformv1.SourceSnapshotChunk
	downloadErr    error
	heartbeatErr   error
	completeCalls  int
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
	if c.heartbeatErr != nil {
		return nil, c.heartbeatErr
	}
	return &emptypb.Empty{}, nil
}

func (c *recordingBuilderServiceClient) ReportBuildLogs(_ context.Context, in *platformv1.ReportBuildLogsRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	c.mu.Lock()
	c.requests = append(c.requests, cloneBuildLogRequestForTest(in))
	fail := c.reportErr
	if c.failRemaining > 0 {
		c.failRemaining--
		fail = c.failErr
	}
	c.mu.Unlock()
	select {
	case c.calls <- struct{}{}:
	default:
	}
	if fail != nil {
		return nil, fail
	}
	return &emptypb.Empty{}, nil
}

func (c *recordingBuilderServiceClient) CompleteBuild(context.Context, *platformv1.CompleteBuildRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	c.mu.Lock()
	c.completeCalls++
	c.mu.Unlock()
	return &emptypb.Empty{}, nil
}

type leaseBlockingExecutor struct {
	started chan struct{}
	stopped chan struct{}
}

func (*leaseBlockingExecutor) Name() string                                        { return ExecutorDevelopment }
func (*leaseBlockingExecutor) Isolating() bool                                     { return false }
func (*leaseBlockingExecutor) RecoverStaleWorkspaces(context.Context) (int, error) { return 0, nil }
func (e *leaseBlockingExecutor) Execute(ctx context.Context, _ ExecutionSpec) (ExecutionResult, error) {
	close(e.started)
	<-ctx.Done()
	close(e.stopped)
	return ExecutionResult{}, ctx.Err()
}

func TestExecuteJobStopsBuildOnLostLease(t *testing.T) {
	archive := dockerfileArchiveForTest(t)
	digest, chunks := snapshotChunksForTest("snapshot-lease", archive)
	client := &recordingBuilderServiceClient{
		downloadChunks: chunks,
		heartbeatErr:   status.Error(codes.PermissionDenied, "lease lost"),
	}
	executor := &leaseBlockingExecutor{started: make(chan struct{}), stopped: make(chan struct{})}
	cfg := testBuilderConfig("builder-lease", t.TempDir())
	app := &App{cfg: cfg, client: client, executor: executor}
	job := &platformv1.BuildJob{
		BuildId: "build-lease",
		Source: &platformv1.BuildJobSource{
			SourceSnapshotId: "snapshot-lease", SourceSnapshotDigest: digest,
			BuildRecipe: dockerfileRecipeForTest(),
		},
		LeaseExpiresAt: timestamppb.New(time.Now().Add(300 * time.Millisecond)),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := app.executeJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	select {
	case <-executor.stopped:
	default:
		t.Fatal("build kept running after lease loss")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.completeCalls != 0 {
		t.Fatalf("reported %d completions after lease loss", client.completeCalls)
	}
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
	clone, _ := proto.Clone(req).(*platformv1.ReportBuildLogsRequest)
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
