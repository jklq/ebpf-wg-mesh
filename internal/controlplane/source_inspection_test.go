package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
)

func TestRecommendBuildRecipeUsesRootContextWhenNestedDockerfileCopiesRepoRootPaths(t *testing.T) {
	t.Parallel()

	archive := makeSourceInspectionArchive(t, "repo", map[string]string{
		"console/Dockerfile":   "FROM oven/bun:1.3.11-slim AS build\nWORKDIR /app/console\nCOPY console/package.json console/bun.lock ./\nCOPY console ./\nCOPY api/proto /app/api/proto\nRUN bun run build\n",
		"console/package.json": "{}\n",
		"console/bun.lock":     "{}\n",
		"api/proto/app.proto":  "syntax = \"proto3\";\n",
	})

	recipe, err := recommendBuildRecipe([]string{"console/Dockerfile"}, archive)
	if err != nil {
		t.Fatalf("recommendBuildRecipe: %v", err)
	}
	if recipe == nil {
		t.Fatal("expected recipe")
	}
	if recipe.GetDockerfilePath() != "console/Dockerfile" {
		t.Fatalf("unexpected dockerfile path %q", recipe.GetDockerfilePath())
	}
	if recipe.GetContextDir() != "." {
		t.Fatalf("unexpected context dir %q", recipe.GetContextDir())
	}
}

func TestRecommendBuildRecipeKeepsNestedContextWhenDockerfileOnlyUsesLocalFiles(t *testing.T) {
	t.Parallel()

	archive := makeSourceInspectionArchive(t, "repo", map[string]string{
		"console/Dockerfile":   "FROM oven/bun:1.3.11-slim AS build\nWORKDIR /app\nCOPY package.json bun.lock ./\nCOPY . ./\nRUN bun run build\n",
		"console/package.json": "{}\n",
		"console/bun.lock":     "{}\n",
	})

	recipe, err := recommendBuildRecipe([]string{"console/Dockerfile"}, archive)
	if err != nil {
		t.Fatalf("recommendBuildRecipe: %v", err)
	}
	if recipe == nil {
		t.Fatal("expected recipe")
	}
	if recipe.GetContextDir() != "console" {
		t.Fatalf("unexpected context dir %q", recipe.GetContextDir())
	}
}

func TestRecommendBuildRecipePrefersRootDockerfile(t *testing.T) {
	t.Parallel()

	archive := makeSourceInspectionArchive(t, "repo", map[string]string{
		"Dockerfile":         "FROM scratch\n",
		"console/Dockerfile": "FROM scratch\n",
	})

	recipe, err := recommendBuildRecipe([]string{"console/Dockerfile", "Dockerfile"}, archive)
	if err != nil {
		t.Fatalf("recommendBuildRecipe: %v", err)
	}
	if recipe == nil {
		t.Fatal("expected recipe")
	}
	if recipe.GetDockerfilePath() != "Dockerfile" {
		t.Fatalf("unexpected dockerfile path %q", recipe.GetDockerfilePath())
	}
	if recipe.GetContextDir() != "." {
		t.Fatalf("unexpected context dir %q", recipe.GetContextDir())
	}
}

func TestParseDockerfileExposePortsSinglePort(t *testing.T) {
	t.Parallel()

	got := parseDockerfileExposePorts([]byte("FROM scratch\nEXPOSE 8080\n"))

	if len(got) != 1 || got[0] != 8080 {
		t.Fatalf("unexpected ports %+v", got)
	}
}

func TestParseDockerfileExposePortsMultiplePortsPreservingOrder(t *testing.T) {
	t.Parallel()

	got := parseDockerfileExposePorts([]byte("EXPOSE 3000 8080\nEXPOSE 9090\n"))

	want := []int32{3000, 8080, 9090}
	if len(got) != len(want) {
		t.Fatalf("unexpected ports %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected ports %+v, want %+v", got, want)
		}
	}
}

func TestParseDockerfileExposePortsIgnoresProtocolSuffixAndInvalidValues(t *testing.T) {
	t.Parallel()

	got := parseDockerfileExposePorts([]byte("EXPOSE 8080/tcp nope 70000 8080/udp 443\n"))

	want := []int32{8080, 443}
	if len(got) != len(want) {
		t.Fatalf("unexpected ports %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected ports %+v, want %+v", got, want)
		}
	}
}

func TestParseDockerfileExposePortsReturnsEmptyWithoutExpose(t *testing.T) {
	t.Parallel()

	got := parseDockerfileExposePorts([]byte("FROM scratch\nCMD [\"/bin/app\"]\n"))

	if len(got) != 0 {
		t.Fatalf("unexpected ports %+v", got)
	}
}

func makeSourceInspectionArchive(t *testing.T, root string, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for name, body := range files {
		fullPath := root + "/" + name
		data := []byte(body)
		if err := tw.WriteHeader(&tar.Header{
			Name: fullPath,
			Mode: 0o644,
			Size: int64(len(data)),
		}); err != nil {
			t.Fatalf("WriteHeader(%s): %v", fullPath, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("Write(%s): %v", fullPath, err)
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
