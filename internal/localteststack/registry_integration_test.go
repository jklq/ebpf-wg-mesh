//go:build integration

package localteststack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/registry"
	"ebof-wg-mesh/internal/controlplane/signkeys/signkeystest"
)

func TestManagedRegistryEnforcesEmbeddedTokenScope(t *testing.T) {
	if _, err := (ExecDockerRunner{}).Run(context.Background(), "version", "--format", "{{.Server.Version}}"); err != nil {
		t.Skipf("docker is unavailable: %v", err)
	}
	port := availableRegistryPort(t)
	host := fmt.Sprintf("localhost:%d", port)
	cfg := config.RegistryConfig{
		Host:                 host,
		NamespacePrefix:      "mesh",
		TokenIssuer:          "registry-integration-test",
		TokenService:         host,
		CredentialTTLSeconds: 300,
	}
	auth, err := registry.NewAuth(context.Background(), cfg, signkeystest.New(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	authServer := httptest.NewServer(auth)
	defer authServer.Close()

	tokenPath := registry.TokenPath
	registry, err := StartManagedRegistry(context.Background(), LocalRegistryConfig{
		StateDir:       t.TempDir(),
		ContainerName:  fmt.Sprintf("registry-auth-test-%d", time.Now().UnixNano()),
		HostPort:       port,
		TokenRealm:     authServer.URL + tokenPath,
		TokenService:   cfg.TokenService,
		TokenIssuer:    cfg.TokenIssuer,
		RootCertBundle: auth.BundlePath(),
	}, ExecDockerRunner{})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()

	pullExpires := time.Now().UTC().Add(48 * time.Hour)
	username, password, err := auth.MintCredential(context.Background(), "test-agent", "mesh/project-1/build-1/service-1", []string{"pull"}, &pullExpires)
	if err != nil {
		t.Fatal(err)
	}
	requestRegistryToken := func(scope string) string {
		t.Helper()
		query := url.Values{"service": {cfg.TokenService}, "scope": {scope}}
		req, err := http.NewRequest(http.MethodGet, authServer.URL+tokenPath+"?"+query.Encode(), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth(username, password)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body.Token
	}
	requestTags := func(repository, token string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "http://"+host+"/v2/"+repository+"/tags/list", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	exact := "mesh/project-1/build-1/service-1"
	exactToken := requestRegistryToken("repository:" + exact + ":pull")
	if status, body := requestTags(exact, exactToken); status != http.StatusNotFound {
		logs, _ := exec.Command("docker", "logs", registry.cfg.ContainerName).CombinedOutput()
		t.Fatalf("exact repository status = %d, want 404; body=%s; registry logs:\n%s", status, body, logs)
	}
	sibling := "mesh/project-2/build-1/service-1"
	siblingToken := requestRegistryToken("repository:" + sibling + ":pull")
	if status, _ := requestTags(sibling, siblingToken); status != http.StatusUnauthorized {
		t.Fatalf("sibling repository status = %d, want 401", status)
	}
}

func availableRegistryPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
