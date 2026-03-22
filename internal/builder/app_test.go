package builder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
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
