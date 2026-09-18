package localteststack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartCloudflareTunnelPassesTokenViaEnv(t *testing.T) {
	const token = "tunnel-token-secret-value"

	binDir := t.TempDir()
	argvDump := filepath.Join(binDir, "argv.txt")
	envDump := filepath.Join(binDir, "env.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvDump + "\nenv > " + envDump + "\nsleep 10\n"
	if err := os.WriteFile(filepath.Join(binDir, "cloudflared"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cloudflared: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(CloudflareTunnelTokenKey, "ambient-token-must-not-inherit")
	t.Setenv(OPServiceAccountTokenKey, "ambient-op-token-must-not-inherit")

	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()

	result, err := StartCloudflareTunnel(ctx, token, "mesh.dev.example.test")
	if err != nil {
		t.Fatalf("StartCloudflareTunnel: %v", err)
	}
	defer result.Close()

	argv, err := os.ReadFile(argvDump)
	if err != nil {
		t.Fatalf("read argv dump: %v", err)
	}
	if strings.Contains(string(argv), token) {
		t.Fatalf("tunnel token appeared in cloudflared argv: %q", argv)
	}
	if strings.Contains(string(argv), "--token") {
		t.Fatalf("cloudflared invoked with --token flag: %q", argv)
	}

	childEnv, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	lines := strings.Split(string(childEnv), "\n")
	values := make(map[string]string, len(lines))
	for _, line := range lines {
		if key, value, ok := strings.Cut(line, "="); ok {
			values[key] = value
		}
	}
	if values[CloudflaredTunnelTokenEnv] != token {
		t.Fatalf("child %s = %q, want the tunnel token", CloudflaredTunnelTokenEnv, values[CloudflaredTunnelTokenEnv])
	}
	for _, key := range []string{CloudflareTunnelTokenKey, OPServiceAccountTokenKey} {
		if _, exists := values[key]; exists {
			t.Fatalf("ambient secret %s inherited by cloudflared", key)
		}
	}
	if strings.Contains(result.BaseURL, token) || strings.Contains(result.Host, token) {
		t.Fatalf("tunnel token leaked into public URL result: %+v", result)
	}
}
