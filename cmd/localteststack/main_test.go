package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ebof-wg-mesh/internal/localteststack"
)

func TestDescribeOnePasswordLoadErrorForDesktopAccountMismatch(t *testing.T) {
	t.Parallel()

	message := describeOnePasswordLoadError(errors.New("initialize 1Password client: error initializing client: An error occurred when processing SDK request: Error { msg: Account not found, inner: None }"))
	if !strings.Contains(message, "desktop-app auth could not find OP_ACCOUNT") {
		t.Fatalf("unexpected message %q", message)
	}
	if !strings.Contains(message, "OP_SERVICE_ACCOUNT_TOKEN") {
		t.Fatalf("expected service-account guidance in %q", message)
	}
}

func TestDescribeOnePasswordLoadErrorForInvalidServiceAccountToken(t *testing.T) {
	t.Parallel()

	message := describeOnePasswordLoadError(errors.New("initialize 1Password client: error initializing client: invalid service account token, please make sure you provide a valid service account token as parameter:  service account token base64 decoding failed, please create another token"))
	if !strings.Contains(message, "invalid OP_SERVICE_ACCOUNT_TOKEN") {
		t.Fatalf("unexpected message %q", message)
	}
	if !strings.Contains(message, "mounted repo-root .env") {
		t.Fatalf("expected mounted .env guidance in %q", message)
	}
	if !strings.Contains(message, "ops_") {
		t.Fatalf("expected ops_ token format hint in %q", message)
	}
}

func TestDescribeCloudflareStartupErrorForMissingTunnelToken(t *testing.T) {
	t.Parallel()

	message := describeCloudflareStartupError(errors.New("start cloudflare tunnel: CLOUDFLARE_TUNNEL_TOKEN is required"), "mesh.example.test")
	if !strings.Contains(message, "CLOUDFLARE_TUNNEL_TOKEN is required") {
		t.Fatalf("unexpected message %q", message)
	}
	if !strings.Contains(message, "mesh.example.test") {
		t.Fatalf("expected hostname in %q", message)
	}
}

func TestDescribeCloudflareStartupErrorForEarlyExit(t *testing.T) {
	t.Parallel()

	message := describeCloudflareStartupError(errors.New("start cloudflare tunnel: cloudflared exited: exit status 1"), "mesh.example.test")
	if !strings.Contains(message, "cloudflared exited before becoming ready") {
		t.Fatalf("unexpected message %q", message)
	}
}

func TestDescribeCloudflareStartupErrorForTimeout(t *testing.T) {
	t.Parallel()

	message := describeCloudflareStartupError(context.DeadlineExceeded, "mesh.example.test")
	if !strings.Contains(message, "timed out after 30s") {
		t.Fatalf("unexpected message %q", message)
	}
}

func TestStartupInterrupted(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !startupInterrupted(ctx) {
		t.Fatal("expected canceled context to interrupt startup")
	}
}

func TestCloudflareTunnelRequestedWithoutCompleteGitHubConfig(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		localteststack.CloudflareTunnelTokenKey: "tunnel-token",
		localteststack.CloudflareHostnameKey:    "mesh.example.test",
	}
	if !cloudflareTunnelRequested(env, env[localteststack.CloudflareTunnelTokenKey], env[localteststack.CloudflareHostnameKey]) {
		t.Fatal("expected Cloudflare credentials to request the public tunnel independently of GitHub config")
	}
}

func TestCloudflareTunnelNotRequestedWithoutCloudflareOrGitHubConfig(t *testing.T) {
	t.Parallel()

	if cloudflareTunnelRequested(nil, "", "") {
		t.Fatal("expected local-only mode when neither Cloudflare nor GitHub is configured")
	}
}
