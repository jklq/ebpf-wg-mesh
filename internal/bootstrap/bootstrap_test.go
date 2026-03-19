package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlPlaneBootstrapParsesFlags(t *testing.T) {
	t.Parallel()

	cfg, err := ControlPlane([]string{
		"-public-listen", "0.0.0.0:8080",
		"-internal-server-names", "controlplane,controlplane-internal",
		"-agent-bootstrap-tokens", "token-a,token-b",
		"-db-url", "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable",
		"-state-dir", "var/controlplane",
		"-oidc-issuer", "https://issuer.example",
		"-oidc-audience", "platform",
		"-oidc-jwks-url", "https://issuer.example/jwks.json",
		"-bootstrap-user", "demo-user:demo@example.com:demo,ops",
	})
	if err != nil {
		t.Fatalf("ControlPlane: %v", err)
	}
	if cfg.Mesh.NetworkCIDR != "fd00:44::/64" {
		t.Fatalf("unexpected mesh network CIDR %q", cfg.Mesh.NetworkCIDR)
	}
	if len(cfg.Bootstrap.Users) != 1 {
		t.Fatalf("expected one bootstrap user, got %d", len(cfg.Bootstrap.Users))
	}
	if got := strings.Join(cfg.Bootstrap.Users[0].Projects, ","); got != "demo,ops" {
		t.Fatalf("unexpected bootstrap projects %q", got)
	}
	if got := strings.Join(cfg.InternalGRPC.TLS.ServerNames, ","); got != "controlplane,controlplane-internal" {
		t.Fatalf("unexpected internal server names %q", got)
	}
	if got := strings.Join(cfg.InternalGRPC.TLS.BootstrapTokens, ","); got != "token-a,token-b" {
		t.Fatalf("unexpected bootstrap tokens %q", got)
	}
	if got := cfg.Database.URL; got != "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable" {
		t.Fatalf("unexpected db url %q", got)
	}
	if got := cfg.StateDir; got != "var/controlplane" {
		t.Fatalf("unexpected state dir %q", got)
	}
}

func TestAgentBootstrapPersistsWireGuardKey(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	cfg, err := Agent([]string{
		"-node-id", "node-1",
		"-node-name", "node-1",
		"-advertise-addr", "fd00:30::10",
		"-controlplane-address", "controlplane:9443",
		"-ca-file", "ca.crt",
		"-bootstrap-token", "test-token",
		"-data-dir", dataDir,
	})
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}
	keyPath := filepath.Join(dataDir, "wireguard.key")
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read wireguard key: %v", err)
	}
	if strings.TrimSpace(string(keyData)) == "" {
		t.Fatal("expected persisted wireguard key")
	}
	if cfg.Mesh.Host.IPv6 != "fd00:30::10" {
		t.Fatalf("unexpected host IPv6 %q", cfg.Mesh.Host.IPv6)
	}
	if cfg.Mesh.WireGuard.PrivateKey == "" {
		t.Fatal("expected bootstrap to load wireguard private key")
	}
	if cfg.ControlPlane.TLS.BootstrapToken != "test-token" {
		t.Fatalf("unexpected bootstrap token %q", cfg.ControlPlane.TLS.BootstrapToken)
	}
}
