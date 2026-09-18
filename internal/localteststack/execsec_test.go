package localteststack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSensitiveEnvKeyMatchesSecretNames(t *testing.T) {
	t.Parallel()

	sensitive := []string{
		"CLOUDFLARE_TUNNEL_TOKEN",
		"OP_SERVICE_ACCOUNT_TOKEN",
		"TUNNEL_TOKEN",
		"DASHBOARD_JWT_SECRET",
		"CONTROLPLANE_GITHUB_WEBHOOK_SECRET",
		"DB_PASSWORD",
		"CONTROLPLANE_GITHUB_PRIVATE_KEY_PEM",
		"REGISTRY_CREDENTIAL",
		"lowercase_api_token",
	}
	for _, key := range sensitive {
		if !SensitiveEnvKey(key) {
			t.Fatalf("expected %q to be sensitive", key)
		}
	}
	for _, key := range []string{"PATH", "HOME", "CLOUDFLARE_HOSTNAME", "DASHBOARD_E2E_BASE_URL", "DOCKER_HOST"} {
		if SensitiveEnvKey(key) {
			t.Fatalf("expected %q to be non-sensitive", key)
		}
	}
}

func TestScrubChildEnvDropsInheritedSecrets(t *testing.T) {
	t.Parallel()

	merged := ScrubChildEnv(
		[]string{
			"PATH=/bin",
			"CLOUDFLARE_HOSTNAME=mesh.example.test",
			"CLOUDFLARE_TUNNEL_TOKEN=inherited",
			"OP_SERVICE_ACCOUNT_TOKEN=inherited",
			"DASHBOARD_JWT_SECRET=old",
		},
		map[string]string{"DASHBOARD_JWT_SECRET": "fresh"},
	)
	values := make(map[string]string, len(merged))
	for _, entry := range merged {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	if values["PATH"] != "/bin" || values["CLOUDFLARE_HOSTNAME"] != "mesh.example.test" {
		t.Fatalf("non-sensitive inheritance broken: %#v", values)
	}
	if values["DASHBOARD_JWT_SECRET"] != "fresh" {
		t.Fatalf("explicit override not enforced: %#v", values)
	}
	for _, key := range []string{"CLOUDFLARE_TUNNEL_TOKEN", "OP_SERVICE_ACCOUNT_TOKEN"} {
		if _, exists := values[key]; exists {
			t.Fatalf("inherited secret %s leaked into child env: %#v", key, values)
		}
	}
}

func TestChildCommandRefusesSecretsInArgs(t *testing.T) {
	t.Parallel()

	_, err := ChildCommand(context.Background(), "cloudflared",
		[]string{"tunnel", "run", "--token", "tunnel-token-secret-value"},
		map[string]string{"TUNNEL_TOKEN": "tunnel-token-secret-value"},
	)
	if err == nil {
		t.Fatal("expected refusal to place secret in argv")
	}
	if !strings.Contains(err.Error(), "argv") {
		t.Fatalf("unexpected error %q", err)
	}
	if strings.Contains(err.Error(), "tunnel-token-secret-value") {
		t.Fatalf("refusal leaked the secret: %q", err)
	}
}

func TestChildCommandScrubsInheritedEnvironment(t *testing.T) {
	t.Setenv("CLOUDFLARE_TUNNEL_TOKEN", "ambient-token-must-not-inherit")
	t.Setenv("SCRUB_CHECK_PASSTHROUGH", "kept")

	cmd, err := ChildCommand(context.Background(), "cloudflared",
		[]string{"tunnel", "run"},
		map[string]string{"TUNNEL_TOKEN": "explicit-token"},
	)
	if err != nil {
		t.Fatalf("ChildCommand: %v", err)
	}
	values := make(map[string]string, len(cmd.Env))
	for _, entry := range cmd.Env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	if values["TUNNEL_TOKEN"] != "explicit-token" {
		t.Fatalf("explicit secret missing from child env: %#v", values)
	}
	if values["SCRUB_CHECK_PASSTHROUGH"] != "kept" {
		t.Fatalf("non-sensitive inheritance broken: %#v", values)
	}
	if _, exists := values["CLOUDFLARE_TUNNEL_TOKEN"]; exists {
		t.Fatalf("ambient tunnel token inherited by child: %#v", values)
	}
	for _, arg := range cmd.Args {
		if strings.Contains(arg, "explicit-token") {
			t.Fatalf("secret appeared in argv: %#v", cmd.Args)
		}
	}
}

func TestRedactErrorPreservesCleanChain(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("startup: %w", context.DeadlineExceeded)
	clean := RedactError(wrapped, "tunnel-token-secret-value")
	if !errors.Is(clean, context.DeadlineExceeded) {
		t.Fatal("expected clean error chain to be preserved")
	}

	dirty := RedactError(errors.New("failed with tunnel-token-secret-value"), "tunnel-token-secret-value")
	if strings.Contains(dirty.Error(), "tunnel-token-secret-value") {
		t.Fatalf("secret survived redaction: %q", dirty)
	}
	if !strings.Contains(dirty.Error(), "[redacted]") {
		t.Fatalf("expected redacted marker in %q", dirty)
	}
	if RedactError(nil, "secret") != nil {
		t.Fatal("expected nil to stay nil")
	}
}

func TestSanitizeDockerArgsForErrorRedactsEnvValues(t *testing.T) {
	t.Parallel()

	args := []string{"run", "--detach", "--env", "API_KEY=hunter2", "--env=DB_PASSWORD=hunter3",
		"-e", "PLAIN=visible?", "--name", "localteststack-svc-1", "image:latest"}
	got := strings.Join(sanitizeDockerArgsForError(args), " ")
	for _, secret := range []string{"hunter2", "hunter3"} {
		if strings.Contains(got, secret) {
			t.Fatalf("env value leaked into docker error args: %q", got)
		}
	}
	for _, want := range []string{"--env API_KEY=[redacted]", "--env=DB_PASSWORD=[redacted]", "-e PLAIN=[redacted]", "--name localteststack-svc-1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in %q", want, got)
		}
	}
}
