package railpack

import (
	"strings"
	"testing"
)

func TestAnalyzeNodeUsesStartScript(t *testing.T) {
	t.Parallel()

	analysis, err := Analyze(
		map[string]struct{}{"package.json": {}, "package-lock.json": {}},
		reader(map[string]string{"package.json": `{"scripts":{"start":"node server.js"}}`}),
	)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if analysis.AppDir != "." || analysis.Language != LanguageNode || analysis.StartCommand != "npm run start" {
		t.Fatalf("unexpected analysis %+v", analysis)
	}
}

func TestAnalyzeNodeUsesPackageManagerAndMainEntry(t *testing.T) {
	t.Parallel()

	analysis, err := Analyze(
		map[string]struct{}{"package.json": {}, "bun.lock": {}},
		reader(map[string]string{"package.json": `{"main":"src/server.js"}`}),
	)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if analysis.StartCommand != "bun src/server.js" {
		t.Fatalf("unexpected start command %q", analysis.StartCommand)
	}
}

func TestAnalyzeNodeFallsBackToIndexFile(t *testing.T) {
	t.Parallel()

	analysis, err := Analyze(
		map[string]struct{}{"package.json": {}, "index.ts": {}},
		reader(map[string]string{"package.json": `{}`}),
	)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if analysis.StartCommand != "node index.ts" {
		t.Fatalf("unexpected start command %q", analysis.StartCommand)
	}
}

func TestAnalyzeNodeMissingStartCommand(t *testing.T) {
	t.Parallel()

	analysis, err := Analyze(
		map[string]struct{}{"package.json": {}},
		reader(map[string]string{"package.json": `{"scripts":{"build":"tsc"}}`}),
	)
	if err == nil || !strings.Contains(err.Error(), "Node.js") || !strings.Contains(err.Error(), "no start command") {
		t.Fatalf("expected missing start command error, got %+v (%v)", analysis, err)
	}
	if analysis.Language != LanguageNode {
		t.Fatalf("expected detected language to survive the error, got %+v", analysis)
	}
}

func TestAnalyzeNodeRejectsInvalidPackageJSON(t *testing.T) {
	t.Parallel()

	_, err := Analyze(
		map[string]struct{}{"package.json": {}},
		reader(map[string]string{"package.json": `{"scripts":`}),
	)
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("expected invalid JSON error, got %v", err)
	}
}

func TestAnalyzePythonUsesMainFile(t *testing.T) {
	t.Parallel()

	analysis, err := Analyze(
		map[string]struct{}{"requirements.txt": {}, "app.py": {}},
		reader(nil),
	)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if analysis.Language != LanguagePython || analysis.StartCommand != "python app.py" {
		t.Fatalf("unexpected analysis %+v", analysis)
	}
}

func TestAnalyzePythonPrefersProcfileWebCommand(t *testing.T) {
	t.Parallel()

	analysis, err := Analyze(
		map[string]struct{}{"requirements.txt": {}, "main.py": {}, "Procfile": {}},
		reader(map[string]string{"Procfile": "web: gunicorn app:app --workers 2\nworker: celery\n"}),
	)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if analysis.StartCommand != "gunicorn app:app --workers 2" {
		t.Fatalf("unexpected start command %q", analysis.StartCommand)
	}
}

func TestAnalyzePythonDetectsFastAPI(t *testing.T) {
	t.Parallel()

	analysis, err := Analyze(
		map[string]struct{}{"requirements.txt": {}, "main.py": {}},
		reader(map[string]string{"requirements.txt": "fastapi==0.115\nuvicorn[standard]\n"}),
	)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if want := "uvicorn main:app --host 0.0.0.0 --port ${PORT:-8000}"; analysis.StartCommand != want {
		t.Fatalf("unexpected start command %q, want %q", analysis.StartCommand, want)
	}
}

func TestAnalyzePythonMissingStartCommand(t *testing.T) {
	t.Parallel()

	analysis, err := Analyze(
		map[string]struct{}{"requirements.txt": {}},
		reader(nil),
	)
	if err == nil || !strings.Contains(err.Error(), "Python") || !strings.Contains(err.Error(), "no start command") {
		t.Fatalf("expected missing start command error, got %+v (%v)", analysis, err)
	}
	if analysis.Language != LanguagePython {
		t.Fatalf("expected detected language to survive the error, got %+v", analysis)
	}
}

func TestAnalyzeGoUsesBuiltBinary(t *testing.T) {
	t.Parallel()

	analysis, err := Analyze(
		map[string]struct{}{"go.mod": {}, "cmd": {}, "cmd/server": {}, "cmd/server/main.go": {}},
		reader(map[string]string{"cmd/server/main.go": "package main\n\nfunc main() {}\n"}),
	)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if analysis.Language != LanguageGo || analysis.StartCommand != "./out" {
		t.Fatalf("unexpected analysis %+v", analysis)
	}
}

func TestAnalyzeGoRejectsLibraryWithoutMainPackage(t *testing.T) {
	t.Parallel()

	_, err := Analyze(
		map[string]struct{}{"go.mod": {}, "lib.go": {}, "lib_test.go": {}},
		reader(map[string]string{
			"lib.go":      "package lib\n",
			"lib_test.go": "package lib\n",
		}),
	)
	if err == nil || !strings.Contains(err.Error(), "Go") || !strings.Contains(err.Error(), "no main package") {
		t.Fatalf("expected missing main package error, got %v", err)
	}
}

func TestAnalyzeStaticSite(t *testing.T) {
	t.Parallel()

	analysis, err := Analyze(
		map[string]struct{}{"index.html": {}},
		reader(nil),
	)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if analysis.Language != LanguageStatic || !strings.HasPrefix(analysis.StartCommand, "caddy run") {
		t.Fatalf("unexpected analysis %+v", analysis)
	}
}

func TestAnalyzeRejectsEmptySource(t *testing.T) {
	t.Parallel()

	_, err := Analyze(map[string]struct{}{"README.md": {}}, reader(nil))
	if err == nil || !strings.Contains(err.Error(), "no buildable source") {
		t.Fatalf("expected no buildable source error, got %v", err)
	}
}

func TestSelectAppDirPrefersRoot(t *testing.T) {
	t.Parallel()

	dir := SelectAppDir(map[string]struct{}{
		"package.json":          {},
		"apps/web/package.json": {},
	})
	if dir != "." {
		t.Fatalf("unexpected app dir %q", dir)
	}
}

func TestSelectAppDirPicksShallowestThenLexical(t *testing.T) {
	t.Parallel()

	dir := SelectAppDir(map[string]struct{}{
		"services/api/requirements.txt": {},
		"services/api/app.py":           {},
		"apps/web/package.json":         {},
		"apps/web/index.js":             {},
	})
	if dir != "apps/web" {
		t.Fatalf("unexpected app dir %q", dir)
	}
}

func TestSelectAppDirSkipsVendoredMarkers(t *testing.T) {
	t.Parallel()

	dir := SelectAppDir(map[string]struct{}{
		"node_modules/tool/package.json": {},
		"apps/web/package.json":          {},
		"apps/web/index.js":              {},
	})
	if dir != "apps/web" {
		t.Fatalf("unexpected app dir %q", dir)
	}
}

func TestSelectAppDirPrefersCodeOverStaticFallback(t *testing.T) {
	t.Parallel()

	dir := SelectAppDir(map[string]struct{}{
		"index.html":            {},
		"apps/web/package.json": {},
		"apps/web/index.js":     {},
	})
	if dir != "apps/web" {
		t.Fatalf("unexpected app dir %q", dir)
	}
}

func TestAnalyzeDirPrefersNodeOverPython(t *testing.T) {
	t.Parallel()

	language, start, err := AnalyzeDir(".", map[string]struct{}{
		"package.json":     {},
		"index.js":         {},
		"requirements.txt": {},
	}, reader(map[string]string{"package.json": `{}`}),
	)
	if err != nil {
		t.Fatalf("AnalyzeDir: %v", err)
	}
	if language != LanguageNode || start != "node index.js" {
		t.Fatalf("unexpected result %q %q", language, start)
	}
}

func reader(files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		body, ok := files[path]
		if !ok {
			return nil, nil
		}
		return []byte(body), nil
	}
}
