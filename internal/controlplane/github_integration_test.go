//go:build integration

package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func bootstrapProjectAndAgent(t *testing.T, store *Store, ctx context.Context) string {
	t.Helper()
	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	return projects[0].ID
}

func countSourceWorkItems(t *testing.T, store *Store, ctx context.Context, kind string) int {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM source_work_items WHERE kind = $1`, kind).Scan(&count); err != nil {
		t.Fatalf("count source work items: %v", err)
	}
	return count
}

func createRepoBackedTestService(t *testing.T, store *Store, ctx context.Context, repositorySelector string, installationID int64, trackedRef string) (string, string) {
	t.Helper()

	projectID := bootstrapProjectAndAgent(t, store, ctx)
	owner, repo, err := splitGitHubRepositorySelector(repositorySelector)
	if err != nil {
		t.Fatalf("splitGitHubRepositorySelector: %v", err)
	}
	if installationID > 0 {
		if err := store.replaceGitHubInstallationRepositories(ctx, githubInstallationRecord{
			InstallationID: installationID,
			AccountLogin:   owner,
			AccountType:    "Organization",
			TargetType:     "Organization",
			Active:         true,
		}, []githubRepositoryRecord{{
			InstallationID: installationID,
			RepositoryID:   2,
			Owner:          owner,
			Repo:           repo,
			FullName:       repositorySelector,
			Private:        true,
			DefaultBranch:  "main",
		}}); err != nil {
			t.Fatalf("replaceGitHubInstallationRepositories: %v", err)
		}
	}
	service, err := store.createService(ctx, "user-1", projectID, "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: repositorySelector,
			TrackedRef:         trackedRef,
			BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	return projectID, service.ID
}

type testGitHubServer struct {
	httpServer        *httptest.Server
	privateKey        string
	installationRepos []map[string]any
	installationID    int64
	privateRepoStatus int

	mu   sync.Mutex
	hits map[string]int
}

func newTestGitHubServer(t *testing.T, installationRepos []map[string]any) *testGitHubServer {
	t.Helper()

	privateKey := testRSAPrivateKeyPEM(t)
	server := &testGitHubServer{
		privateKey:        privateKey,
		installationRepos: installationRepos,
		installationID:    7,
		privateRepoStatus: http.StatusNotFound,
		hits:              make(map[string]int),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/7/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		server.recordHit(r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "installation-token",
			"expires_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		server.recordHit(r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer installation-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		repos := server.installationRepos
		if len(repos) == 0 {
			repos = []map[string]any{{
				"id":             2,
				"name":           "secret",
				"full_name":      "private/secret",
				"private":        true,
				"default_branch": "main",
				"owner":          map[string]any{"login": "private"},
			}}
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page <= 0 {
			page = 1
		}
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		if perPage <= 0 {
			perPage = len(repos)
		}
		start := (page - 1) * perPage
		if start > len(repos) {
			start = len(repos)
		}
		end := start + perPage
		if end > len(repos) {
			end = len(repos)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count":  len(repos),
			"repositories": repos[start:end],
		})
	})
	mux.HandleFunc("/repos/public/hello", func(w http.ResponseWriter, r *http.Request) {
		server.recordHit(r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             1,
			"name":           "hello",
			"full_name":      "public/hello",
			"private":        false,
			"default_branch": "main",
			"owner":          map[string]any{"login": "public"},
		})
	})
	mux.HandleFunc("/repos/public/hello/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		server.recordHit(r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": map[string]any{"sha": "commit-public-main"},
		})
	})
	mux.HandleFunc("/repos/public/hello/git/ref/heads/release", func(w http.ResponseWriter, r *http.Request) {
		server.recordHit(r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": map[string]any{"sha": "commit-public-release"},
		})
	})
	mux.HandleFunc("/repos/public/hello/tarball/commit-public-main", func(w http.ResponseWriter, r *http.Request) {
		server.recordHit(r.URL.Path)
		_, _ = w.Write(makeGitHubArchive(t, "public-hello-commit-public-main", map[string]string{
			"Dockerfile":  "FROM scratch\n",
			"app/main.go": "package main\n",
		}))
	})
	mux.HandleFunc("/repos/public/hello/tarball/commit-public-release", func(w http.ResponseWriter, r *http.Request) {
		server.recordHit(r.URL.Path)
		_, _ = w.Write(makeGitHubArchive(t, "public-hello-commit-public-release", map[string]string{
			"Dockerfile":      "FROM scratch\n",
			"app/release.txt": "release\n",
		}))
	})
	mux.HandleFunc("/repos/private/secret", func(w http.ResponseWriter, r *http.Request) {
		server.recordHit(r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer installation-token" {
			http.Error(w, http.StatusText(server.privateRepoStatus), server.privateRepoStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             2,
			"name":           "secret",
			"full_name":      "private/secret",
			"private":        true,
			"default_branch": "main",
			"owner":          map[string]any{"login": "private"},
		})
	})
	mux.HandleFunc("/repos/private/secret/installation", func(w http.ResponseWriter, r *http.Request) {
		server.recordHit(r.URL.Path)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if server.installationID <= 0 {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          server.installationID,
			"target_type": "Organization",
			"account": map[string]any{
				"login": "private",
				"type":  "Organization",
			},
		})
	})
	mux.HandleFunc("/repos/private/secret/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		server.recordHit(r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer installation-token" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": map[string]any{"sha": "commit-private-main"},
		})
	})
	mux.HandleFunc("/repos/private/secret/tarball/commit-private-main", func(w http.ResponseWriter, r *http.Request) {
		server.recordHit(r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer installation-token" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(makeGitHubArchive(t, "private-secret-commit-private-main", map[string]string{
			"Dockerfile": "FROM scratch\n",
			"secret.txt": "classified\n",
		}))
	})
	server.httpServer = httptest.NewServer(mux)
	t.Cleanup(server.httpServer.Close)
	return server
}

func (s *testGitHubServer) config() config.GitHubAppConfig {
	return config.GitHubAppConfig{
		Enabled:       true,
		AppID:         123,
		WebhookSecret: "topsecret",
		PrivateKeyPEM: s.privateKey,
		APIBaseURL:    s.httpServer.URL,
		WebBaseURL:    "https://github.example.test",
		WebhookPath:   "/webhooks/github",
	}
}

func (s *testGitHubServer) recordHit(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits[path]++
}

func (s *testGitHubServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, count := range s.hits {
		total += count
	}
	return total
}

func (s *testGitHubServer) pathHits(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func (s *testGitHubServer) branchHeadHits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits["/repos/public/hello/git/ref/heads/main"] +
		s.hits["/repos/public/hello/git/ref/heads/release"] +
		s.hits["/repos/private/secret/git/ref/heads/main"]
}

func makeGitHubArchive(t *testing.T, root string, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for name, body := range files {
		path := root + "/" + name
		data := []byte(body)
		if err := tw.WriteHeader(&tar.Header{
			Name: path,
			Mode: 0o644,
			Size: int64(len(data)),
		}); err != nil {
			t.Fatalf("WriteHeader(%s): %v", path, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("Write(%s): %v", path, err)
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

func testRSAPrivateKeyPEM(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

func testWebhookSignature(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
