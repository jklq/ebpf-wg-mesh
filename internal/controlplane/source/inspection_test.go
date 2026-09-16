package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestRecommendBuildRecipeUsesRootContextWhenNestedDockerfileCopiesRepoRootPaths(t *testing.T) {
	t.Parallel()

	archive := makeSourceInspectionArchive(t, "repo", map[string]string{
		"console/Dockerfile":   "FROM oven/bun:1.3.11-slim AS build\nWORKDIR /app/console\nCOPY console/package.json console/bun.lock ./\nCOPY console ./\nCOPY api/proto /app/api/proto\nRUN bun run build\n",
		"console/package.json": "{}\n",
		"console/bun.lock":     "{}\n",
		"api/proto/app.proto":  "syntax = \"proto3\";\n",
	})

	recipe, err := recommendDockerfileRecipe([]string{"console/Dockerfile"}, archive)
	if err != nil {
		t.Fatalf("recommendDockerfileRecipe: %v", err)
	}
	if recipe == nil {
		t.Fatal("expected recipe")
	}
	if recipe.GetBuilder() != platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE {
		t.Fatalf("unexpected builder %s", recipe.GetBuilder())
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

	recipe, err := recommendDockerfileRecipe([]string{"console/Dockerfile"}, archive)
	if err != nil {
		t.Fatalf("recommendDockerfileRecipe: %v", err)
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

	recipe, err := recommendDockerfileRecipe([]string{"console/Dockerfile", "Dockerfile"}, archive)
	if err != nil {
		t.Fatalf("recommendDockerfileRecipe: %v", err)
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

func TestAnalyzeSourceArchiveRecommendsRailpackForNode(t *testing.T) {
	t.Parallel()

	archive := makeSourceInspectionArchive(t, "repo", map[string]string{
		"package.json": `{"scripts":{"start":"node server.js"}}`,
		"server.js":    "console.log(1)\n",
	})
	resp, err := analyzeSourceArchive(&platformv1.InspectSourceResponse{}, archive)
	if err != nil {
		t.Fatalf("analyzeSourceArchive: %v", err)
	}
	if resp.GetAnalysisError() != "" {
		t.Fatalf("unexpected analysis error %q", resp.GetAnalysisError())
	}
	if resp.GetDetectedLanguage() != "node" || resp.GetDetectedStartCommand() != "npm run start" {
		t.Fatalf("unexpected detection %q %q", resp.GetDetectedLanguage(), resp.GetDetectedStartCommand())
	}
	recipe := resp.GetRecommendedBuildRecipe()
	if recipe.GetBuilder() != platformv1.BuilderKind_BUILDER_KIND_RAILPACK || recipe.GetContextDir() != "." {
		t.Fatalf("unexpected recommended recipe %+v", recipe)
	}
	if resp.GetRecommendedDockerfileRecipe() != nil {
		t.Fatalf("unexpected dockerfile recipe %+v", resp.GetRecommendedDockerfileRecipe())
	}
}

func TestAnalyzeSourceArchiveRecommendsRailpackForPython(t *testing.T) {
	t.Parallel()

	archive := makeSourceInspectionArchive(t, "repo", map[string]string{
		"requirements.txt": "flask==3.0\ngunicorn\n",
		"app.py":           "app = 1\n",
	})
	resp, err := analyzeSourceArchive(&platformv1.InspectSourceResponse{}, archive)
	if err != nil {
		t.Fatalf("analyzeSourceArchive: %v", err)
	}
	if resp.GetDetectedLanguage() != "python" {
		t.Fatalf("unexpected language %q", resp.GetDetectedLanguage())
	}
	if resp.GetRecommendedBuildRecipe().GetBuilder() != platformv1.BuilderKind_BUILDER_KIND_RAILPACK {
		t.Fatalf("unexpected recommended recipe %+v", resp.GetRecommendedBuildRecipe())
	}
}

func TestAnalyzeSourceArchiveRecommendsRailpackForGo(t *testing.T) {
	t.Parallel()

	archive := makeSourceInspectionArchive(t, "repo", map[string]string{
		"go.mod":  "module example.com/app\n\ngo 1.27\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})
	resp, err := analyzeSourceArchive(&platformv1.InspectSourceResponse{}, archive)
	if err != nil {
		t.Fatalf("analyzeSourceArchive: %v", err)
	}
	if resp.GetDetectedLanguage() != "go" || resp.GetDetectedStartCommand() != "./out" {
		t.Fatalf("unexpected detection %q %q", resp.GetDetectedLanguage(), resp.GetDetectedStartCommand())
	}
	if resp.GetRecommendedBuildRecipe().GetBuilder() != platformv1.BuilderKind_BUILDER_KIND_RAILPACK {
		t.Fatalf("unexpected recommended recipe %+v", resp.GetRecommendedBuildRecipe())
	}
}

func TestAnalyzeSourceArchiveRecommendsRailpackForStaticSite(t *testing.T) {
	t.Parallel()

	archive := makeSourceInspectionArchive(t, "repo", map[string]string{
		"index.html": "<h1>hi</h1>\n",
	})
	resp, err := analyzeSourceArchive(&platformv1.InspectSourceResponse{}, archive)
	if err != nil {
		t.Fatalf("analyzeSourceArchive: %v", err)
	}
	if resp.GetDetectedLanguage() != "static" {
		t.Fatalf("unexpected language %q", resp.GetDetectedLanguage())
	}
	if resp.GetRecommendedBuildRecipe().GetBuilder() != platformv1.BuilderKind_BUILDER_KIND_RAILPACK {
		t.Fatalf("unexpected recommended recipe %+v", resp.GetRecommendedBuildRecipe())
	}
}

func TestAnalyzeSourceArchiveSelectsNestedMonorepoApp(t *testing.T) {
	t.Parallel()

	archive := makeSourceInspectionArchive(t, "repo", map[string]string{
		"apps/web/package.json":         `{"scripts":{"start":"node server.js"}}`,
		"apps/web/server.js":            "console.log(1)\n",
		"services/api/requirements.txt": "flask\n",
		"services/api/app.py":           "app = 1\n",
	})
	resp, err := analyzeSourceArchive(&platformv1.InspectSourceResponse{}, archive)
	if err != nil {
		t.Fatalf("analyzeSourceArchive: %v", err)
	}
	recipe := resp.GetRecommendedBuildRecipe()
	if recipe.GetBuilder() != platformv1.BuilderKind_BUILDER_KIND_RAILPACK || recipe.GetContextDir() != "apps/web" {
		t.Fatalf("unexpected recommended recipe %+v", recipe)
	}
	if resp.GetDetectedLanguage() != "node" {
		t.Fatalf("unexpected language %q", resp.GetDetectedLanguage())
	}
}

func TestAnalyzeSourceArchiveKeepsDockerfileExplicit(t *testing.T) {
	t.Parallel()

	archive := makeSourceInspectionArchive(t, "repo", map[string]string{
		"Dockerfile":   "FROM scratch\nEXPOSE 8080\n",
		"package.json": `{"scripts":{"start":"node server.js"}}`,
		"server.js":    "console.log(1)\n",
	})
	resp, err := analyzeSourceArchive(&platformv1.InspectSourceResponse{}, archive)
	if err != nil {
		t.Fatalf("analyzeSourceArchive: %v", err)
	}
	if resp.GetRecommendedBuildRecipe().GetBuilder() != platformv1.BuilderKind_BUILDER_KIND_RAILPACK {
		t.Fatalf("expected railpack default, got %+v", resp.GetRecommendedBuildRecipe())
	}
	dockerfile := resp.GetRecommendedDockerfileRecipe()
	if dockerfile.GetBuilder() != platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE || dockerfile.GetDockerfilePath() != "Dockerfile" {
		t.Fatalf("unexpected dockerfile recipe %+v", dockerfile)
	}
	if len(resp.GetDockerfileCandidates()) != 1 || len(resp.GetRecommendedPorts()) != 1 || resp.GetRecommendedPorts()[0] != 8080 {
		t.Fatalf("unexpected candidates %+v ports %+v", resp.GetDockerfileCandidates(), resp.GetRecommendedPorts())
	}
}

func TestAnalyzeSourceArchiveReportsMissingStartCommand(t *testing.T) {
	t.Parallel()

	archive := makeSourceInspectionArchive(t, "repo", map[string]string{
		"package.json": `{"scripts":{"build":"tsc"}}`,
	})
	resp, err := analyzeSourceArchive(&platformv1.InspectSourceResponse{}, archive)
	if err != nil {
		t.Fatalf("analyzeSourceArchive: %v", err)
	}
	if resp.GetRecommendedBuildRecipe() != nil {
		t.Fatalf("expected no recommended recipe, got %+v", resp.GetRecommendedBuildRecipe())
	}
	if resp.GetDetectedLanguage() != "node" {
		t.Fatalf("unexpected language %q", resp.GetDetectedLanguage())
	}
	if !strings.Contains(resp.GetAnalysisError(), "no start command") {
		t.Fatalf("expected actionable analysis error, got %q", resp.GetAnalysisError())
	}
}

func TestAnalyzeSourceArchiveReportsNoBuildableSource(t *testing.T) {
	t.Parallel()

	archive := makeSourceInspectionArchive(t, "repo", map[string]string{
		"README.md": "# hi\n",
	})
	resp, err := analyzeSourceArchive(&platformv1.InspectSourceResponse{}, archive)
	if err != nil {
		t.Fatalf("analyzeSourceArchive: %v", err)
	}
	if resp.GetRecommendedBuildRecipe() != nil || resp.GetRecommendedDockerfileRecipe() != nil {
		t.Fatalf("expected no recipes, got %+v %+v", resp.GetRecommendedBuildRecipe(), resp.GetRecommendedDockerfileRecipe())
	}
	if !strings.Contains(resp.GetAnalysisError(), "no buildable source") {
		t.Fatalf("expected actionable analysis error, got %q", resp.GetAnalysisError())
	}
}

func TestAnalyzeSourceArchivePointsDockerfileOnlyRepoAtExplicitBuilder(t *testing.T) {
	t.Parallel()

	archive := makeSourceInspectionArchive(t, "repo", map[string]string{
		"Dockerfile": "FROM scratch\n",
	})
	resp, err := analyzeSourceArchive(&platformv1.InspectSourceResponse{}, archive)
	if err != nil {
		t.Fatalf("analyzeSourceArchive: %v", err)
	}
	if resp.GetRecommendedBuildRecipe() != nil {
		t.Fatalf("expected no silent dockerfile fallback, got %+v", resp.GetRecommendedBuildRecipe())
	}
	if !strings.Contains(resp.GetAnalysisError(), "Dockerfile builder") {
		t.Fatalf("expected dockerfile guidance, got %q", resp.GetAnalysisError())
	}
	if resp.GetRecommendedDockerfileRecipe().GetDockerfilePath() != "Dockerfile" {
		t.Fatalf("unexpected dockerfile recipe %+v", resp.GetRecommendedDockerfileRecipe())
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
