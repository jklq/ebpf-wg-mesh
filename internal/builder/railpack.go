package builder

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"ebof-wg-mesh/internal/railpack"
)

const railpackPlanFilename = "railpack-plan.json"

func railpackPlanCommand(railpackBinary, appDir, planPath string) commandRequest {
	return commandRequest{
		Binary: railpackBinary,
		Args:   []string{"plan", appDir, "--out", planPath, "--error-missing-start"},
	}
}

func railpackBuildCommand(buildBinary, buildkitAddress, frontendImage, appDir, planDir, planPath, pushRef, metadataFile string, env []string) commandRequest {
	if isDockerBuildBinary(buildBinary) {
		return railpackDockerBuildxCommand(buildBinary, buildkitAddress, frontendImage, appDir, planPath, pushRef, metadataFile, env)
	}
	return railpackBuildctlCommand(buildBinary, buildkitAddress, frontendImage, appDir, planDir, pushRef, metadataFile, env)
}

func railpackDockerBuildxCommand(dockerBinary, dockerAddress, frontendImage, appDir, planPath, pushRef, metadataFile string, env []string) commandRequest {
	return commandRequest{
		Binary: dockerBinary,
		Env:    env,
		Args: []string{
			"--host", dockerAddress, "buildx", "build",
			"--progress=plain",
			"--add-host", "host.docker.internal:host-gateway",
			"--build-arg", "BUILDKIT_SYNTAX=" + frontendImage,
			"--file", planPath,
			"--tag", pushRef,
			"--push",
			"--metadata-file", metadataFile,
			appDir,
		},
	}
}

func railpackBuildctlCommand(buildctlBinary, buildkitAddress, frontendImage, appDir, planDir, pushRef, metadataFile string, env []string) commandRequest {
	return commandRequest{
		Binary: buildctlBinary,
		Env:    env,
		Args: []string{
			"--addr", buildkitAddress,
			"build",
			"--frontend", "gateway.v0",
			"--opt", "source=" + frontendImage,
			"--local", "context=" + appDir,
			"--local", "dockerfile=" + planDir,
			"--opt", "filename=" + railpackPlanFilename,
			"--output", "type=image,name=" + pushRef + ",push=true",
			"--metadata-file", metadataFile,
		},
	}
}

func railpackPlanFailure(appDir, contextDir string, req commandRequest, runErr error, output []byte) error {
	if errors.Is(runErr, exec.ErrNotFound) {
		return &buildFailureError{kind: failureKindBuild, err: fmt.Errorf("railpack executable not found: %s", strings.TrimSpace(req.Binary))}
	}
	message := formatBuildCommandError(req, runErr, output).Error()
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && exitErr.ExitCode() == 75 {
		message = "railpack reported a transient error (exit 75, safe to retry): " + message
	}
	if detail := workspaceAnalysisDetail(appDir, contextDir); detail != "" {
		message += "; " + detail
	}
	return &buildFailureError{kind: failureKindBuild, err: errors.New("railpack analysis failed: " + message)}
}

func workspaceAnalysisDetail(appDir, contextDir string) string {
	dir := path.Clean(strings.TrimSpace(contextDir))
	if dir == "" || dir == "/" {
		dir = "."
	}
	paths, err := collectWorkspacePaths(appDir, dir)
	if err != nil {
		return ""
	}
	_, _, analysisErr := railpack.AnalyzeDir(dir, paths, workspaceFileReader(appDir, dir))
	if analysisErr != nil {
		return analysisErr.Error()
	}
	return ""
}

func collectWorkspacePaths(root, prefix string) (map[string]struct{}, error) {
	paths := map[string]struct{}{}
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if current == root {
			return nil
		}
		rel, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		slash := filepath.ToSlash(rel)
		if prefix == "." || prefix == "" {
			paths[slash] = struct{}{}
		} else {
			paths[prefix+"/"+slash] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return paths, nil
}

func workspaceFileReader(root, prefix string) func(string) ([]byte, error) {
	return func(target string) ([]byte, error) {
		rel := target
		if prefix != "." && prefix != "" {
			var ok bool
			rel, ok = strings.CutPrefix(target, prefix+"/")
			if !ok {
				return nil, fmt.Errorf("path %q is outside the build context", target)
			}
		}
		candidate := filepath.Join(root, filepath.FromSlash(rel))
		if resolved, err := filepath.Rel(root, candidate); err != nil || resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("path %q is outside the build context", target)
		}
		return os.ReadFile(candidate)
	}
}
