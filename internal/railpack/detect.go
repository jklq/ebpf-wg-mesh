package railpack

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

const (
	LanguageNode   = "node"
	LanguagePython = "python"
	LanguageGo     = "go"
	LanguageStatic = "static"
)

type Analysis struct {
	AppDir       string
	Language     string
	StartCommand string
}

func DisplayName(language string) string {
	switch language {
	case LanguageNode:
		return "Node.js"
	case LanguagePython:
		return "Python"
	case LanguageGo:
		return "Go"
	case LanguageStatic:
		return "static site"
	default:
		return language
	}
}

func Analyze(paths map[string]struct{}, read func(string) ([]byte, error)) (Analysis, error) {
	dir := SelectAppDir(paths)
	if dir == "" {
		return Analysis{}, fmt.Errorf("no buildable source detected: expected package.json (Node.js), requirements.txt or pyproject.toml (Python), go.mod (Go), or index.html (static site)")
	}
	language, start, err := AnalyzeDir(dir, paths, read)
	if err != nil {
		return Analysis{AppDir: dir, Language: language}, err
	}
	return Analysis{AppDir: dir, Language: language, StartCommand: start}, nil
}

func SelectAppDir(paths map[string]struct{}) string {
	var code, static []string
	for _, dir := range markedDirs(paths) {
		if hasCodeMarkers(dir, paths) {
			code = append(code, dir)
		} else {
			static = append(static, dir)
		}
	}
	if dir, ok := preferAppDir(code); ok {
		return dir
	}
	if dir, ok := preferAppDir(static); ok {
		return dir
	}
	return ""
}

func AnalyzeDir(dir string, paths map[string]struct{}, read func(string) ([]byte, error)) (string, string, error) {
	if read == nil {
		read = func(string) ([]byte, error) { return nil, fmt.Errorf("no file reader") }
	}
	switch {
	case has(paths, join(dir, "package.json")):
		start, err := nodeStartCommand(dir, paths, read)
		if err != nil {
			return LanguageNode, "", err
		}
		return LanguageNode, start, nil
	case hasPythonMarkers(dir, paths):
		start, err := pythonStartCommand(dir, paths, read)
		if err != nil {
			return LanguagePython, "", err
		}
		return LanguagePython, start, nil
	case has(paths, join(dir, "go.mod")):
		start, err := goStartCommand(dir, paths, read)
		if err != nil {
			return LanguageGo, "", err
		}
		return LanguageGo, start, nil
	case hasStaticMarkers(dir, paths):
		return LanguageStatic, "caddy run --config Caddyfile --adapter caddyfile 2>&1", nil
	default:
		return "", "", fmt.Errorf("no buildable source detected in %q: expected package.json (Node.js), requirements.txt or pyproject.toml (Python), go.mod (Go), or index.html (static site)", dir)
	}
}

func markedDirs(paths map[string]struct{}) []string {
	dirs := map[string]struct{}{}
	for file := range paths {
		if vendoredPath(file) {
			continue
		}
		dir := path.Dir(file)
		if dir == "/" {
			continue
		}
		if dirHasMarkers(dir, paths) {
			dirs[dir] = struct{}{}
		}
	}
	out := make([]string, 0, len(dirs))
	for dir := range dirs {
		out = append(out, dir)
	}
	return out
}

func dirHasMarkers(dir string, paths map[string]struct{}) bool {
	return hasCodeMarkers(dir, paths) || hasStaticMarkers(dir, paths)
}

func hasCodeMarkers(dir string, paths map[string]struct{}) bool {
	return has(paths, join(dir, "package.json")) || hasPythonMarkers(dir, paths) || has(paths, join(dir, "go.mod"))
}

func hasPythonMarkers(dir string, paths map[string]struct{}) bool {
	for _, name := range []string{"requirements.txt", "pyproject.toml", "setup.py", "setup.cfg", "Pipfile", "Pipfile.lock", "uv.lock", "poetry.lock"} {
		if has(paths, join(dir, name)) {
			return true
		}
	}
	return mainPythonFile(dir, paths) != ""
}

func hasStaticMarkers(dir string, paths map[string]struct{}) bool {
	return has(paths, join(dir, "Staticfile")) || has(paths, join(dir, "public")) || has(paths, join(dir, "index.html"))
}

func preferAppDir(dirs []string) (string, bool) {
	if len(dirs) == 0 {
		return "", false
	}
	for _, dir := range dirs {
		if dir == "." {
			return ".", true
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		if depth(dirs[i]) != depth(dirs[j]) {
			return depth(dirs[i]) < depth(dirs[j])
		}
		return dirs[i] < dirs[j]
	})
	return dirs[0], true
}

func depth(dir string) int {
	if dir == "." {
		return 0
	}
	return strings.Count(dir, "/") + 1
}

func vendoredPath(file string) bool {
	for _, segment := range strings.Split(file, "/") {
		switch segment {
		case "node_modules", ".git", "vendor", ".venv", "venv", "__pycache__", "target", ".hg", ".svn", ".tox":
			return true
		}
		if strings.HasSuffix(segment, ".egg-info") {
			return true
		}
	}
	return false
}

func has(paths map[string]struct{}, p string) bool {
	_, ok := paths[p]
	return ok
}

func join(dir, name string) string {
	if dir == "." || dir == "" {
		return name
	}
	return dir + "/" + name
}

func nodeStartCommand(dir string, paths map[string]struct{}, read func(string) ([]byte, error)) (string, error) {
	body, err := read(join(dir, "package.json"))
	if err != nil {
		return "", fmt.Errorf("detected Node.js in %q but package.json could not be read: %v", dir, err)
	}
	var manifest struct {
		Main    string            `json:"main"`
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return "", fmt.Errorf("detected Node.js in %q but package.json is not valid JSON: %v", dir, err)
	}
	manager := nodePackageManager(dir, paths)
	if strings.TrimSpace(manifest.Scripts["start"]) != "" {
		return manager + " run start", nil
	}
	if strings.TrimSpace(manifest.Main) != "" {
		return nodeRuntime(manager) + " " + strings.TrimSpace(manifest.Main), nil
	}
	for _, entry := range []string{"index.js", "index.ts"} {
		if has(paths, join(dir, entry)) {
			return nodeRuntime(manager) + " " + entry, nil
		}
	}
	return "", fmt.Errorf("detected Node.js in %q but no start command was found: add a \"start\" script or \"main\" entry to package.json, or add index.js", dir)
}

func nodePackageManager(dir string, paths map[string]struct{}) string {
	switch {
	case has(paths, join(dir, "bun.lockb")) || has(paths, join(dir, "bun.lock")):
		return "bun"
	case has(paths, join(dir, "pnpm-lock.yaml")):
		return "pnpm"
	case has(paths, join(dir, "yarn.lock")):
		return "yarn"
	default:
		return "npm"
	}
}

func nodeRuntime(manager string) string {
	if manager == "bun" {
		return "bun"
	}
	return "node"
}

func pythonStartCommand(dir string, paths map[string]struct{}, read func(string) ([]byte, error)) (string, error) {
	if command := procfileWebCommand(dir, paths, read); command != "" {
		return command, nil
	}
	main := mainPythonFile(dir, paths)
	if main != "" {
		if command := pythonFrameworkStartCommand(dir, paths, read); command != "" {
			return command, nil
		}
		return "python " + main, nil
	}
	if has(paths, join(dir, "manage.py")) {
		return "python manage.py runserver 0.0.0.0:${PORT:-8000}", nil
	}
	return "", fmt.Errorf("detected Python in %q but no start command was found: add a Procfile with a web process or an entry point such as main.py", dir)
}

func mainPythonFile(dir string, paths map[string]struct{}) string {
	for _, name := range []string{"main.py", "app.py", "start.py", "bot.py", "hello.py", "server.py"} {
		if has(paths, join(dir, name)) {
			return name
		}
	}
	return ""
}

func procfileWebCommand(dir string, paths map[string]struct{}, read func(string) ([]byte, error)) string {
	if !has(paths, join(dir, "Procfile")) {
		return ""
	}
	body, err := read(join(dir, "Procfile"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(body), "\n") {
		name, command, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "web") {
			continue
		}
		if command = strings.TrimSpace(command); command != "" {
			return command
		}
	}
	return ""
}

func pythonFrameworkStartCommand(dir string, paths map[string]struct{}, read func(string) ([]byte, error)) string {
	deps := ""
	for _, name := range []string{"requirements.txt", "pyproject.toml", "Pipfile"} {
		if !has(paths, join(dir, name)) {
			continue
		}
		body, err := read(join(dir, name))
		if err != nil {
			continue
		}
		deps += "\n" + strings.ToLower(string(body))
	}
	var start string
	if strings.Contains(deps, "fasthtml") && strings.Contains(deps, "uvicorn") {
		start = "uvicorn main:app --host 0.0.0.0 --port ${PORT:-8000}"
	}
	if strings.Contains(deps, "fastapi") && strings.Contains(deps, "uvicorn") {
		start = "uvicorn main:app --host 0.0.0.0 --port ${PORT:-8000}"
	}
	if strings.Contains(deps, "flask") && strings.Contains(deps, "gunicorn") {
		start = "gunicorn --bind 0.0.0.0:${PORT:-8000} main:app"
	}
	return start
}

var goMainPackagePattern = regexp.MustCompile(`(?m)^\s*package\s+main\b`)

func goStartCommand(dir string, paths map[string]struct{}, read func(string) ([]byte, error)) (string, error) {
	for file := range paths {
		if !goMainCandidate(dir, file) {
			continue
		}
		body, err := read(file)
		if err != nil {
			continue
		}
		if goMainPackagePattern.Match(body) {
			return "./out", nil
		}
	}
	return "", fmt.Errorf("detected Go in %q but no main package was found: add a main.go file or a command under cmd/", dir)
}

func goMainCandidate(dir, file string) bool {
	if vendoredPath(file) || !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
		return false
	}
	rel := file
	if dir != "." && dir != "" {
		var ok bool
		rel, ok = strings.CutPrefix(file, dir+"/")
		if !ok {
			return false
		}
	}
	if !strings.Contains(rel, "/") {
		return true
	}
	rest, ok := strings.CutPrefix(rel, "cmd/")
	return ok && rest != ""
}
