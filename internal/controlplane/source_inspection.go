package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

type gitHubSourceInspector struct {
	catalog *GitHubCatalog
	client  *GitHubClient
}

func newGitHubSourceInspector(catalog *GitHubCatalog, client *GitHubClient) *gitHubSourceInspector {
	if catalog == nil || client == nil || !client.Enabled() {
		return nil
	}
	return &gitHubSourceInspector{catalog: catalog, client: client}
}

func (i *gitHubSourceInspector) Inspect(ctx context.Context, repositorySelector string) (*platformv1.InspectSourceResponse, error) {
	if i == nil || i.catalog == nil || i.client == nil {
		return nil, fmt.Errorf("github source inspection is not configured")
	}
	owner, repo, err := splitGitHubRepositorySelector(repositorySelector)
	if err != nil {
		return nil, err
	}

	view, err := i.catalog.ResolveRepositoryView(ctx, owner, repo)
	if err != nil {
		return nil, err
	}

	resp := &platformv1.InspectSourceResponse{
		AccessState:   toProtoSourceAccessState(sourceAccessStateFromGitHubView(view)),
		DefaultBranch: strings.TrimSpace(view.DefaultBranch),
	}
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
	if recipe := recommendBuildRecipe(candidates); recipe != nil {
		resp.RecommendedBuildRecipe = recipe
	}
	return resp, nil
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

func recommendBuildRecipe(candidates []string) *platformv1.BuildRecipe {
	if len(candidates) == 0 {
		return nil
	}
	chosen := candidates[0]
	for _, candidate := range candidates {
		if candidate == "Dockerfile" {
			chosen = candidate
			break
		}
	}
	contextDir := path.Dir(chosen)
	if contextDir == "." || contextDir == "/" {
		contextDir = "."
	}
	return &platformv1.BuildRecipe{
		DockerfilePath: chosen,
		ContextDir:     contextDir,
	}
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
