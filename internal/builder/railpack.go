package builder

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/railpack"
)

const railpackPlanFilename = "railpack-plan.json"

func (a *App) invokeRailpackBuild(ctx context.Context, job *platformv1.BuildJob, workspace jobWorkspace, reporter *buildLogReporter) (string, error) {
	recipe := job.GetSource().GetBuildRecipe()
	appDir, err := resolveRepoPath(workspace.repoDir, recipe.GetContextDir(), true)
	if err != nil {
		return "", &buildFailureError{kind: failureKindBuild, err: fmt.Errorf("context dir: %w", err)}
	}
	if isDockerBuildBinary(a.cfg.BuildctlBinary) {
		return "", &buildFailureError{kind: failureKindBuild, err: errors.New("railpack builds require buildctl; the docker fallback only supports dockerfile builds")}
	}
	planDir, err := safeChildPath(workspace.root, "plan")
	if err != nil {
		return "", &buildFailureError{kind: failureKindBuild, err: err}
	}
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		return "", &buildFailureError{kind: failureKindBuild, err: fmt.Errorf("mkdir railpack plan dir: %w", err)}
	}
	planPath := filepath.Join(planDir, railpackPlanFilename)

	report := func(line commandOutputLine) {
		reporter.Report(ctx, line)
	}
	planReq := railpackPlanCommand(a.cfg.RailpackBinary, appDir, planPath)
	output, err := a.runner.Run(ctx, planReq, report)
	if err != nil {
		return "", railpackPlanFailure(appDir, recipe.GetContextDir(), planReq, err, output)
	}
	if info, err := os.Stat(planPath); err != nil || info.Size() == 0 {
		return "", &buildFailureError{kind: failureKindBuild, err: errors.New("railpack plan succeeded but wrote no build plan")}
	}

	env, cleanup, err := dockerConfigEnv(workspace.root, job.GetRegistryPushReference(), job.GetRegistryUsername(), job.GetRegistryPassword())
	if err != nil {
		return "", &buildFailureError{kind: failureKindPush, err: err}
	}
	defer cleanup()

	req := railpackBuildctlCommand(a.cfg.BuildctlBinary, a.cfg.BuildkitAddress, a.cfg.RailpackFrontendImage, appDir, planDir, job.GetRegistryPushReference(), workspace.metadataFile, env)
	output, err = a.runner.Run(ctx, req, report)
	if err != nil {
		return "", classifyBuildctlFailure(formatBuildCommandError(req, err, output))
	}
	return buildDigestRefFromMetadata(job.GetRegistryPushReference(), workspace.metadataFile)
}

func railpackPlanCommand(railpackBinary, appDir, planPath string) commandRequest {
	return commandRequest{
		Binary: railpackBinary,
		Args:   []string{"plan", appDir, "--out", planPath, "--error-missing-start"},
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
