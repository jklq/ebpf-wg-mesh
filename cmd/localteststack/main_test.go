package main

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestReservePortHoldsBindUntilClosed(t *testing.T) {
	t.Parallel()

	listener, port, err := reservePort("127.0.0.1")
	if err != nil {
		t.Fatalf("reservePort: %v", err)
	}
	if port <= 0 {
		t.Fatalf("unexpected port %d", port)
	}
	_, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err == nil {
		t.Fatal("expected second listen on reserved port to fail")
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close reserved listener: %v", err)
	}
	second, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("listen after release: %v", err)
	}
	_ = second.Close()
}

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

func TestDescribeCloudflareStartupErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "missing tunnel token",
			err:  errors.New("start cloudflare tunnel: CLOUDFLARE_TUNNEL_TOKEN is required"),
			want: []string{"CLOUDFLARE_TUNNEL_TOKEN is required", "mesh.example.test"},
		},
		{
			name: "early exit",
			err:  errors.New("start cloudflare tunnel: cloudflared exited: exit status 1"),
			want: []string{"cloudflared exited before becoming ready"},
		},
		{
			name: "timeout",
			err:  context.DeadlineExceeded,
			want: []string{"timed out after 30s"},
		},
	}
	for _, tc := range cases {
		message := describeCloudflareStartupError(tc.err, "mesh.example.test")
		for _, want := range tc.want {
			if !strings.Contains(message, want) {
				t.Fatalf("%s: expected %q in %q", tc.name, want, message)
			}
		}
	}
}

func TestMergeCommandEnvReplacesInheritedSecrets(t *testing.T) {
	t.Parallel()

	merged := mergeCommandEnv(
		[]string{"PATH=/bin", "DASHBOARD_DEV_USERS=inherited", "DASHBOARD_JWT_SECRET=old", "UNRELATED_API_TOKEN=do-not-inherit"},
		map[string]string{"DASHBOARD_DEV_USERS": "", "DASHBOARD_JWT_SECRET": "fresh"},
	)
	values := make(map[string]string, len(merged))
	for _, entry := range merged {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	if values["DASHBOARD_DEV_USERS"] != "" || values["DASHBOARD_JWT_SECRET"] != "fresh" {
		t.Fatalf("explicit overrides not enforced: %#v", values)
	}
	if _, exists := values["UNRELATED_API_TOKEN"]; exists {
		t.Fatalf("unrelated secret inherited by console: %#v", values)
	}
}

func TestCloudflareTunnelRequestedForCompletePublicConfig(t *testing.T) {
	t.Parallel()

	if !cloudflareTunnelRequested(false, true, "tunnel-token", "mesh.example.test") {
		t.Fatal("expected complete GitHub and Cloudflare config to request the public tunnel")
	}
}

func TestCloudflareTunnelNotRequestedForIncompleteImplicitConfig(t *testing.T) {
	t.Parallel()

	if cloudflareTunnelRequested(false, false, "tunnel-token", "mesh.example.test") {
		t.Fatal("expected local-only mode without complete GitHub config")
	}
}

func TestCloudflareTunnelExplicitModeStillValidatesRuntimeConfig(t *testing.T) {
	t.Parallel()

	if !cloudflareTunnelRequested(true, false, "", "") {
		t.Fatal("expected explicit public mode to continue into configuration validation")
	}
}
