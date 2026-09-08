package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

type Inspector struct {
	catalog *GitHubCatalog
	client  *GitHubClient
}

var (
	ErrGitHubUserAccessTokenRequired    = errors.New("github user access token is required")
	ErrGitHubRepositoryIdentityMismatch = errors.New("github repository identity mismatch")
)

type GitHubUserRepositoryAuthorizationError struct {
	Cause error
}

func (e *GitHubUserRepositoryAuthorizationError) Error() string {
	return "github user repository authorization failed"
}

func (e *GitHubUserRepositoryAuthorizationError) Unwrap() error {
	return e.Cause
}

func NewInspector(catalog *GitHubCatalog, client *GitHubClient) *Inspector {
	if catalog == nil || client == nil || !client.Enabled() {
		return nil
	}
	return &Inspector{catalog: catalog, client: client}
}

func (i *Inspector) LinkAndInspect(ctx context.Context, projectID, userID, repositorySelector, userAccessToken string) (*platformv1.InspectSourceResponse, error) {
	if i == nil || i.catalog == nil || i.client == nil {
		return nil, fmt.Errorf("github source inspection is not configured")
	}
	owner, repo, err := SplitGitHubRepositorySelector(repositorySelector)
	if err != nil {
		return nil, err
	}

	view, err := i.catalog.ResolveRepositoryView(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	if err := i.authorizeUserRepository(ctx, owner, repo, userAccessToken, view); err != nil {
		return nil, err
	}
	if view.AccessState != SourceAccessStateAvailable {
		return inspectionResponseForView(view), nil
	}
	if err := i.catalog.store.LinkProjectGitHubRepository(ctx, projectID, userID, view); err != nil {
		return nil, err
	}
	return i.inspectView(ctx, view)
}

func (i *Inspector) Inspect(ctx context.Context, projectID, repositorySelector, userAccessToken string) (*platformv1.InspectSourceResponse, error) {
	view, err := i.authorizedRepositoryView(ctx, projectID, repositorySelector)
	if err != nil {
		return nil, err
	}
	owner, repo, err := SplitGitHubRepositorySelector(repositorySelector)
	if err != nil {
		return nil, err
	}
	if err := i.authorizeUserRepository(ctx, owner, repo, userAccessToken, view); err != nil {
		return nil, err
	}
	return i.inspectView(ctx, view)
}

func (i *Inspector) authorizeUserRepository(ctx context.Context, owner, repo, accessToken string, appView GitHubRepositoryView) error {
	if strings.TrimSpace(accessToken) == "" {
		return &GitHubUserRepositoryAuthorizationError{Cause: ErrGitHubUserAccessTokenRequired}
	}
	userView, err := i.client.GetUserRepository(ctx, owner, repo, accessToken)
	if err != nil {
		return &GitHubUserRepositoryAuthorizationError{Cause: err}
	}
	userOwner, userRepo, err := normalizeRepositoryRef(userView.Owner, userView.Repo)
	if err != nil || !strings.EqualFold(userOwner, owner) || !strings.EqualFold(userRepo, repo) {
		return &GitHubUserRepositoryAuthorizationError{Cause: ErrGitHubRepositoryIdentityMismatch}
	}
	if appView.AccessState == SourceAccessStateAvailable && (appView.RepositoryID <= 0 || userView.RepositoryID != appView.RepositoryID) {
		return &GitHubUserRepositoryAuthorizationError{Cause: ErrGitHubRepositoryIdentityMismatch}
	}
	return nil
}

func (i *Inspector) Authorize(ctx context.Context, projectID string, source *platformv1.ServiceSourceSpec) error {
	if source == nil || strings.TrimSpace(strings.ToLower(source.GetProvider())) != "github" {
		return nil
	}
	view, err := i.authorizedRepositoryView(ctx, projectID, source.GetRepositorySelector())
	if err != nil {
		return err
	}
	if view.AccessState != SourceAccessStateAvailable {
		return fmt.Errorf("repository is not authorized for project")
	}
	return nil
}

func (i *Inspector) authorizedRepositoryView(ctx context.Context, projectID, repositorySelector string) (GitHubRepositoryView, error) {
	if i == nil || i.catalog == nil || i.client == nil {
		return GitHubRepositoryView{}, fmt.Errorf("github source inspection is not configured")
	}
	owner, repo, err := SplitGitHubRepositorySelector(repositorySelector)
	if err != nil {
		return GitHubRepositoryView{}, err
	}
	installationID, err := i.catalog.store.ProjectGitHubRepositoryInstallation(ctx, projectID, owner, repo)
	if err != nil {
		return GitHubRepositoryView{}, fmt.Errorf("repository is not linked to project: %w", err)
	}
	return i.catalog.RepositoryView(ctx, owner, repo, installationID)
}

func (i *Inspector) inspectView(ctx context.Context, view GitHubRepositoryView) (*platformv1.InspectSourceResponse, error) {
	resp := inspectionResponseForView(view)
	if resp.GetAccessState() != platformv1.SourceAccessState_SOURCE_ACCESS_STATE_AVAILABLE {
		return resp, nil
	}

	trackedRef := resp.GetDefaultBranch()
	if trackedRef == "" {
		trackedRef = "main"
		resp.DefaultBranch = trackedRef
	}
	commitSHA, err := i.client.GetBranchHead(ctx, view.Owner, view.Repo, trackedRef, view.InstallationID)
	if err != nil {
		return nil, err
	}
	archiveTGZ, err := i.client.FetchArchive(ctx, view.Owner, view.Repo, commitSHA, view.InstallationID)
	if err != nil {
		return nil, err
	}
	candidates, err := detectDockerfileCandidates(archiveTGZ)
	if err != nil {
		return nil, err
	}
	resp.DockerfileCandidates = candidates
	recipe, err := recommendBuildRecipe(candidates, archiveTGZ)
	if err != nil {
		return nil, err
	}
	if recipe != nil {
		resp.RecommendedBuildRecipe = recipe
		dockerfileBody, err := readArchiveFile(archiveTGZ, recipe.GetDockerfilePath())
		if err != nil {
			return nil, err
		}
		resp.RecommendedPorts = parseDockerfileExposePorts(dockerfileBody)
	}
	return resp, nil
}

func inspectionResponseForView(view GitHubRepositoryView) *platformv1.InspectSourceResponse {
	return &platformv1.InspectSourceResponse{
		AccessState:   ToProtoSourceAccessState(sourceAccessStateFromGitHubView(view)),
		DefaultBranch: strings.TrimSpace(view.DefaultBranch),
	}
}

func detectDockerfileCandidates(archiveTGZ []byte) ([]string, error) {
	gzr, err := gzip.NewReader(bytes.NewReader(archiveTGZ))
	if err != nil {
		return nil, fmt.Errorf("open source archive: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	seen := make(map[string]struct{})
	for {
		header, err := tr.Next()
		switch {
		case err == nil:
		case err == io.EOF:
			out := make([]string, 0, len(seen))
			for candidate := range seen {
				out = append(out, candidate)
			}
			sort.Strings(out)
			return out, nil
		default:
			return nil, fmt.Errorf("read source archive: %w", err)
		}
		if header == nil || header.Typeflag != tar.TypeReg {
			continue
		}
		normalized := normalizeArchivePath(header.Name)
		if normalized == "" || path.Base(normalized) != "Dockerfile" {
			continue
		}
		seen[normalized] = struct{}{}
	}
}

func recommendBuildRecipe(candidates []string, archiveTGZ []byte) (*platformv1.BuildRecipe, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	chosen := candidates[0]
	for _, candidate := range candidates {
		if candidate == "Dockerfile" {
			chosen = candidate
			break
		}
	}
	contextDir, err := recommendContextDir(chosen, archiveTGZ)
	if err != nil {
		return nil, err
	}
	return &platformv1.BuildRecipe{
		DockerfilePath: chosen,
		ContextDir:     contextDir,
	}, nil
}

func recommendContextDir(dockerfilePath string, archiveTGZ []byte) (string, error) {
	contextDir := path.Dir(dockerfilePath)
	if contextDir == "." || contextDir == "/" {
		return ".", nil
	}
	dockerfileBody, err := readArchiveFile(archiveTGZ, dockerfilePath)
	if err != nil {
		return "", err
	}
	archivePaths, err := collectArchivePaths(archiveTGZ)
	if err != nil {
		return "", err
	}
	if dockerfileRequiresRepoRootContext(contextDir, dockerfileBody, archivePaths) {
		return ".", nil
	}
	return contextDir, nil
}

func readArchiveFile(archiveTGZ []byte, target string) ([]byte, error) {
	gzr, err := gzip.NewReader(bytes.NewReader(archiveTGZ))
	if err != nil {
		return nil, fmt.Errorf("open source archive: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		switch {
		case err == nil:
		case err == io.EOF:
			return nil, fmt.Errorf("source archive missing %s", target)
		default:
			return nil, fmt.Errorf("read source archive: %w", err)
		}
		if header == nil || header.Typeflag != tar.TypeReg {
			continue
		}
		normalized := normalizeArchivePath(header.Name)
		if normalized != target {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read %s from source archive: %w", target, err)
		}
		return body, nil
	}
}

// validExposedPort reports whether a Dockerfile EXPOSE port is usable. This is
// intentionally a small local check: the service-spec port validation stays in
// delivery and this package must not import it.
func validExposedPort(port int32) bool {
	return port >= 1 && port <= 65535
}

func parseDockerfileExposePorts(body []byte) []int32 {
	seen := map[int32]struct{}{}
	var out []int32
	for _, rawLine := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.ToUpper(fields[0]) != "EXPOSE" {
			continue
		}
		for _, field := range fields[1:] {
			token := strings.TrimSpace(field)
			if slash := strings.IndexByte(token, '/'); slash >= 0 {
				token = token[:slash]
			}
			value, err := strconv.ParseInt(token, 10, 32)
			if err != nil {
				continue
			}
			port := int32(value)
			if !validExposedPort(port) {
				continue
			}
			if _, ok := seen[port]; ok {
				continue
			}
			seen[port] = struct{}{}
			out = append(out, port)
		}
	}
	return out
}

func collectArchivePaths(archiveTGZ []byte) (map[string]struct{}, error) {
	gzr, err := gzip.NewReader(bytes.NewReader(archiveTGZ))
	if err != nil {
		return nil, fmt.Errorf("open source archive: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	paths := make(map[string]struct{})
	for {
		header, err := tr.Next()
		switch {
		case err == nil:
		case err == io.EOF:
			return paths, nil
		default:
			return nil, fmt.Errorf("read source archive: %w", err)
		}
		if header == nil || header.Typeflag != tar.TypeReg {
			continue
		}
		normalized := normalizeArchivePath(header.Name)
		if normalized == "" {
			continue
		}
		paths[normalized] = struct{}{}
		for dir := path.Dir(normalized); dir != "." && dir != "/"; dir = path.Dir(dir) {
			paths[dir] = struct{}{}
		}
	}
}

func dockerfileRequiresRepoRootContext(contextDir string, dockerfileBody []byte, archivePaths map[string]struct{}) bool {
	lines := strings.Split(string(dockerfileBody), "\n")
	var logicalLine strings.Builder
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if logicalLine.Len() > 0 {
			logicalLine.WriteByte(' ')
		}
		line = strings.TrimSuffix(line, "\\")
		logicalLine.WriteString(line)
		if strings.HasSuffix(strings.TrimSpace(rawLine), "\\") {
			continue
		}
		if copyInstructionNeedsRepoRoot(contextDir, logicalLine.String(), archivePaths) {
			return true
		}
		logicalLine.Reset()
	}
	return logicalLine.Len() > 0 && copyInstructionNeedsRepoRoot(contextDir, logicalLine.String(), archivePaths)
}

func copyInstructionNeedsRepoRoot(contextDir, line string, archivePaths map[string]struct{}) bool {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return false
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return false
	}
	instruction := strings.ToUpper(fields[0])
	if instruction != "COPY" && instruction != "ADD" {
		return false
	}
	args := strings.TrimSpace(line[len(fields[0]):])
	if strings.HasPrefix(args, "[") {
		return jsonCopyNeedsRepoRoot(contextDir, args, archivePaths)
	}
	return shellCopyNeedsRepoRoot(contextDir, fields[1:], archivePaths)
}

func jsonCopyNeedsRepoRoot(contextDir, args string, archivePaths map[string]struct{}) bool {
	var entries []string
	if err := json.Unmarshal([]byte(args), &entries); err != nil {
		return false
	}
	if len(entries) < 2 {
		return false
	}
	return sourcePathsNeedRepoRoot(contextDir, entries[:len(entries)-1], archivePaths)
}

func shellCopyNeedsRepoRoot(contextDir string, fields []string, archivePaths map[string]struct{}) bool {
	args := make([]string, 0, len(fields))
	skipNextValue := false
	for _, field := range fields {
		if skipNextValue {
			skipNextValue = false
			continue
		}
		if field == "--from" {
			return false
		}
		if strings.HasPrefix(field, "--from=") {
			return false
		}
		if strings.HasPrefix(field, "--") {
			if !strings.Contains(field, "=") {
				skipNextValue = true
			}
			continue
		}
		args = append(args, field)
	}
	if len(args) < 2 {
		return false
	}
	return sourcePathsNeedRepoRoot(contextDir, args[:len(args)-1], archivePaths)
}

func sourcePathsNeedRepoRoot(contextDir string, sources []string, archivePaths map[string]struct{}) bool {
	for _, source := range sources {
		if sourcePathNeedsRepoRoot(contextDir, source, archivePaths) {
			return true
		}
	}
	return false
}

func sourcePathNeedsRepoRoot(contextDir, source string, archivePaths map[string]struct{}) bool {
	source = strings.Trim(strings.TrimSpace(source), `"'`)
	if source == "" || source == "." {
		return false
	}
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		return false
	}
	if strings.ContainsAny(source, "*?[") {
		return false
	}
	normalized := path.Clean(strings.TrimPrefix(source, "./"))
	if normalized == "." || normalized == "/" {
		return false
	}
	localPath := normalized
	if contextDir != "." {
		localPath = path.Clean(path.Join(contextDir, normalized))
	}
	if archivePathExists(archivePaths, localPath) {
		return false
	}
	return contextDir != "." && archivePathExists(archivePaths, normalized)
}

func archivePathExists(paths map[string]struct{}, candidate string) bool {
	_, ok := paths[candidate]
	return ok
}

func normalizeArchivePath(raw string) string {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "/")
	if len(parts) <= 1 {
		return path.Clean(raw)
	}
	trimmed := path.Clean(strings.Join(parts[1:], "/"))
	if trimmed == "." || trimmed == "/" || strings.HasPrefix(trimmed, "../") {
		return ""
	}
	return trimmed
}
