package source

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"ebof-wg-mesh/internal/config"

	"github.com/golang-jwt/jwt/v5"
)

type githubRepositoryView struct {
	RepositoryID  int64
	Owner         string
	Repo          string
	FullName      string
	Private       bool
	DefaultBranch string
}

type githubInstallationToken struct {
	Token     string
	ExpiresAt time.Time
}

type githubInstallationView struct {
	InstallationID int64
	AccountLogin   string
	AccountType    string
	TargetType     string
}

type gitHubCommitMetadata struct {
	Message string
	Author  string
}

var errGitHubNotModified = errors.New("github not modified")

const MaxArchiveCompressedBytes = 64 << 20

type GitHubAPIError struct {
	StatusCode int
	Method     string
	Path       string
}

func (e *GitHubAPIError) Error() string {
	return fmt.Sprintf("github api %s %s: status %d", e.Method, e.Path, e.StatusCode)
}

type GitHubClient struct {
	cfg        config.GitHubAppConfig
	client     *http.Client
	signingKey *rsa.PrivateKey

	mu     sync.Mutex
	tokens map[int64]githubInstallationToken
}

func NewGitHubClient(cfg config.GitHubAppConfig) (*GitHubClient, error) {
	signingKey, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(cfg.PrivateKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("parse github app private key: %w", err)
	}
	return &GitHubClient{
		cfg:        cfg,
		client:     &http.Client{Timeout: 15 * time.Second},
		signingKey: signingKey,
		tokens:     make(map[int64]githubInstallationToken),
	}, nil
}

func (c *GitHubClient) Enabled() bool {
	return c != nil && c.cfg.Enabled
}

func (c *GitHubClient) FetchArchive(ctx context.Context, owner, repo, ref string, installationID int64) ([]byte, error) {
	req, err := c.newRequest(ctx, http.MethodGet, c.apiPath("/repos/%s/%s/tarball/%s", owner, repo, strings.TrimSpace(ref)), nil)
	if err != nil {
		return nil, err
	}
	if installationID > 0 {
		token, err := c.InstallationToken(ctx, installationID)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token.Token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &GitHubAPIError{StatusCode: resp.StatusCode, Method: req.Method, Path: req.URL.Path}
	}
	archive, err := io.ReadAll(io.LimitReader(resp.Body, MaxArchiveCompressedBytes+1))
	if err != nil {
		return nil, err
	}
	if len(archive) > MaxArchiveCompressedBytes {
		return nil, errors.New("github source archive exceeds compressed size limit")
	}
	if err := ValidateArchive(archive); err != nil {
		return nil, err
	}
	return archive, nil
}

func (c *GitHubClient) GetBranchHead(ctx context.Context, owner, repo, branch string, installationID int64) (string, error) {
	ref := "heads/" + strings.TrimSpace(branch)
	req, err := c.newRequest(ctx, http.MethodGet, c.apiPath("/repos/%s/%s/git/ref/%s", owner, repo, ref), nil)
	if err != nil {
		return "", err
	}
	if installationID > 0 {
		token, err := c.InstallationToken(ctx, installationID)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+token.Token)
	}
	var payload struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := c.doJSON(req, &payload); err != nil {
		return "", err
	}
	if strings.TrimSpace(payload.Object.SHA) == "" {
		return "", errors.New("github branch head response missing sha")
	}
	return payload.Object.SHA, nil
}

func (c *GitHubClient) GetCommitMetadata(ctx context.Context, owner, repo, commitSHA string, installationID int64) (gitHubCommitMetadata, error) {
	req, err := c.newRequest(ctx, http.MethodGet, c.apiPath("/repos/%s/%s/commits/%s", owner, repo, strings.TrimSpace(commitSHA)), nil)
	if err != nil {
		return gitHubCommitMetadata{}, err
	}
	if installationID > 0 {
		token, err := c.InstallationToken(ctx, installationID)
		if err != nil {
			return gitHubCommitMetadata{}, err
		}
		req.Header.Set("Authorization", "Bearer "+token.Token)
	}
	var payload struct {
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
			} `json:"author"`
			Committer struct {
				Name string `json:"name"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := c.doJSON(req, &payload); err != nil {
		return gitHubCommitMetadata{}, err
	}
	author := strings.TrimSpace(payload.Commit.Author.Name)
	if author == "" {
		author = strings.TrimSpace(payload.Commit.Committer.Name)
	}
	return gitHubCommitMetadata{
		Message: strings.TrimSpace(payload.Commit.Message),
		Author:  author,
	}, nil
}

func (c *GitHubClient) GetRepository(ctx context.Context, owner, repo string, installationID int64) (githubRepositoryView, error) {
	req, err := c.newRequest(ctx, http.MethodGet, c.apiPath("/repos/%s/%s", owner, repo), nil)
	if err != nil {
		return githubRepositoryView{}, err
	}
	if installationID > 0 {
		token, err := c.InstallationToken(ctx, installationID)
		if err != nil {
			return githubRepositoryView{}, err
		}
		req.Header.Set("Authorization", "Bearer "+token.Token)
	}
	var payload struct {
		ID            int64  `json:"id"`
		Private       bool   `json:"private"`
		DefaultBranch string `json:"default_branch"`
		FullName      string `json:"full_name"`
		Name          string `json:"name"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	if err := c.doJSON(req, &payload); err != nil {
		return githubRepositoryView{}, err
	}
	return githubRepositoryView{
		RepositoryID:  payload.ID,
		Owner:         payload.Owner.Login,
		Repo:          payload.Name,
		FullName:      payload.FullName,
		Private:       payload.Private,
		DefaultBranch: payload.DefaultBranch,
	}, nil
}

// GetUserRepository resolves a repository with the signed-in user's OAuth
// token. It is deliberately separate from GetRepository so a caller cannot
// accidentally substitute GitHub App installation access for user access.
func (c *GitHubClient) GetUserRepository(ctx context.Context, owner, repo, accessToken string) (githubRepositoryView, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return githubRepositoryView{}, errors.New("github user access token is required")
	}
	req, err := c.newRequest(ctx, http.MethodGet, c.apiPath("/repos/%s/%s", owner, repo), nil)
	if err != nil {
		return githubRepositoryView{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	var payload struct {
		ID            int64  `json:"id"`
		Private       bool   `json:"private"`
		DefaultBranch string `json:"default_branch"`
		FullName      string `json:"full_name"`
		Name          string `json:"name"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	if err := c.doJSON(req, &payload); err != nil {
		return githubRepositoryView{}, err
	}
	return githubRepositoryView{
		RepositoryID:  payload.ID,
		Owner:         payload.Owner.Login,
		Repo:          payload.Name,
		FullName:      payload.FullName,
		Private:       payload.Private,
		DefaultBranch: payload.DefaultBranch,
	}, nil
}

func (c *GitHubClient) ListInstallationRepositories(ctx context.Context, installationID int64) ([]githubRepositoryView, error) {
	token, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	page := 1
	out := make([]githubRepositoryView, 0, 16)
	for {
		rawURL := c.apiPath("/installation/repositories")
		pageURL, err := url.Parse(rawURL)
		if err != nil {
			return nil, err
		}
		query := pageURL.Query()
		query.Set("per_page", "100")
		query.Set("page", fmt.Sprintf("%d", page))
		pageURL.RawQuery = query.Encode()
		req, err := c.newRequest(ctx, http.MethodGet, pageURL.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token.Token)

		var payload struct {
			TotalCount   int `json:"total_count"`
			Repositories []struct {
				ID            int64  `json:"id"`
				Name          string `json:"name"`
				FullName      string `json:"full_name"`
				Private       bool   `json:"private"`
				DefaultBranch string `json:"default_branch"`
				Owner         struct {
					Login string `json:"login"`
				} `json:"owner"`
			} `json:"repositories"`
		}
		if err := c.doJSON(req, &payload); err != nil {
			return nil, err
		}
		for _, repo := range payload.Repositories {
			out = append(out, githubRepositoryView{
				RepositoryID:  repo.ID,
				Owner:         repo.Owner.Login,
				Repo:          repo.Name,
				FullName:      repo.FullName,
				Private:       repo.Private,
				DefaultBranch: repo.DefaultBranch,
			})
		}
		if len(payload.Repositories) == 0 || len(out) >= payload.TotalCount {
			return out, nil
		}
		page++
	}
}

func (c *GitHubClient) GetRepositoryInstallation(ctx context.Context, owner, repo string) (githubInstallationView, error) {
	req, err := c.newRequest(ctx, http.MethodGet, c.apiPath("/repos/%s/%s/installation", owner, repo), nil)
	if err != nil {
		return githubInstallationView{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.appJWT())

	var payload struct {
		ID         int64  `json:"id"`
		TargetType string `json:"target_type"`
		Account    struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
	}
	if err := c.doJSON(req, &payload); err != nil {
		return githubInstallationView{}, err
	}
	if payload.ID <= 0 {
		return githubInstallationView{}, errors.New("github repository installation response missing id")
	}
	return githubInstallationView{
		InstallationID: payload.ID,
		AccountLogin:   payload.Account.Login,
		AccountType:    payload.Account.Type,
		TargetType:     payload.TargetType,
	}, nil
}

func (c *GitHubClient) InstallationToken(ctx context.Context, installationID int64) (githubInstallationToken, error) {
	if installationID <= 0 {
		return githubInstallationToken{}, errors.New("installation id is required")
	}
	c.mu.Lock()
	if cached, ok := c.tokens[installationID]; ok && time.Until(cached.ExpiresAt) > time.Minute {
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()

	req, err := c.newRequest(ctx, http.MethodPost, c.apiPath("/app/installations/%d/access_tokens", installationID), bytes.NewReader([]byte("{}")))
	if err != nil {
		return githubInstallationToken{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.appJWT())
	var payload struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.doJSON(req, &payload); err != nil {
		return githubInstallationToken{}, err
	}
	token := githubInstallationToken{Token: payload.Token, ExpiresAt: payload.ExpiresAt}
	c.mu.Lock()
	c.tokens[installationID] = token
	c.mu.Unlock()
	return token, nil
}

func (c *GitHubClient) appJWT() string {
	now := time.Now().UTC()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": c.cfg.AppID,
	})
	signed, err := token.SignedString(c.signingKey)
	if err != nil {
		panic(fmt.Sprintf("sign github app jwt: %v", err))
	}
	return signed
}

func (c *GitHubClient) newRequest(ctx context.Context, method, rawURL string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", fmt.Sprintf("ebof-wg-mesh-github-app/%d", c.cfg.AppID))
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *GitHubClient) doJSON(req *http.Request, out any) error {
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return errGitHubNotModified
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &GitHubAPIError{StatusCode: resp.StatusCode, Method: req.Method, Path: req.URL.Path}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *GitHubClient) apiPath(pattern string, args ...any) string {
	base, err := url.Parse(c.cfg.APIBaseURL)
	if err != nil {
		panic(err)
	}
	base.Path = path.Join(strings.TrimSuffix(base.Path, "/"), fmt.Sprintf(pattern, args...))
	return base.String()
}
