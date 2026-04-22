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
