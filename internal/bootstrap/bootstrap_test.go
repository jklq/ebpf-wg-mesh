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
		"-profile", "development",
		"-user-assertion-secret", "test-user-assertion-secret-at-least-32-bytes",
		"-internal-server-names", "controlplane,controlplane-internal",
		"-replica-addresses", "replica-a:9443,replica-b:9443",
		"-advertise-addr", "replica-a:9443",
		"-db-url", "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable",
		"-logs-clickhouse-url", "clickhouse://127.0.0.1:9000/default",
		"-logs-retention-days", "30",
		"-failover-reconcile-interval-seconds", "7",
		"-failover-unhealthy-threshold-seconds", "45",
		"-state-dir", "var/controlplane",
		"-internal-revoked-client-cert-serials-file", "var/security/revoked-client-serials.txt",
		"-bootstrap-user", "demo-user:demo@example.com:demo,ops+operator",
		"-agent-bootstrap-tokens", "node-a=token-a|region=us-east|failure-domain=zone-1|reserved-cpu-millis=500,node-b=token-b",
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
	if !cfg.Bootstrap.Users[0].Operator {
		t.Fatal("expected bootstrap user to be a platform operator")
	}
	if got := strings.Join(cfg.InternalGRPC.TLS.ServerNames, ","); got != "controlplane,controlplane-internal" {
		t.Fatalf("unexpected internal server names %q", got)
	}
	if got := strings.Join(cfg.ReplicaAddresses, ","); got != "replica-a:9443,replica-b:9443" {
		t.Fatalf("unexpected replica addresses %q", got)
	}
	if cfg.AdvertiseAddr != "replica-a:9443" {
		t.Fatalf("unexpected advertise addr %q", cfg.AdvertiseAddr)
	}
	if got := cfg.InternalGRPC.TLS.BootstrapTokens; len(got) != 2 || got[0].AgentID != "node-a" || got[0].Token != "token-a" || got[0].Region != "us-east" || got[0].FailureDomain != "zone-1" || got[0].ReservedCPUMillis != 500 || got[1].AgentID != "node-b" || got[1].Token != "token-b" {
		t.Fatalf("unexpected bootstrap tokens %#v", got)
	}
	if got := cfg.Database.URL; got != "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable" {
		t.Fatalf("unexpected db url %q", got)
	}
	if got := cfg.Logs.ClickHouse.URL; got != "clickhouse://127.0.0.1:9000/default" {
		t.Fatalf("unexpected clickhouse url %q", got)
	}
	if got := cfg.Logs.RetentionDays; got != 30 {
		t.Fatalf("unexpected log retention days %d", got)
	}
	if got := cfg.StateDir; got != "var/controlplane" {
		t.Fatalf("unexpected state dir %q", got)
	}
	if got := cfg.InternalGRPC.TLS.RevokedClientCertSerialsFile; got != "var/security/revoked-client-serials.txt" {
		t.Fatalf("unexpected client certificate revocation file %q", got)
	}
	if cfg.Failover.ReconcileIntervalSeconds != 7 || cfg.Failover.UnhealthyThresholdSeconds != 45 {
		t.Fatalf("unexpected failover config: %+v", cfg.Failover)
	}
}

func TestControlPlaneBootstrapDefaultsToProductionAndRejectsLoopbackDatabase(t *testing.T) {
	t.Parallel()

	_, err := ControlPlane([]string{
		"-user-assertion-secret", "production-user-assertion-secret-at-least-32",
		"-internal-server-names", "controlplane.example.test",
		"-agent-bootstrap-tokens", "node-a=token-a",
		"-db-url", "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable",
		"-health-listen", "127.0.0.1:18080",
		"-source-archives-dir", "/var/lib/ebpf-wg-mesh/controlplane/source-archives",
		"-ingress-public-addr", "platform.example.test",
	})
	if err == nil || !strings.Contains(err.Error(), "loopback host") {
		t.Fatalf("expected production to reject loopback database, got %v", err)
	}
}

func TestControlPlaneBootstrapRejectsUnboundAgentToken(t *testing.T) {
	t.Parallel()

	_, err := ControlPlane([]string{
		"-agent-bootstrap-tokens", "reusable-token",
	})
	if err == nil || err.Error() != "agent bootstrap token must be agent_id=token" {
		t.Fatalf("expected agent-bound token validation error, got %v", err)
	}
}

func TestAgentBootstrapPersistsWireGuardKey(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	cfg, err := Agent([]string{
		"-profile", "development",
		"-node-id", "node-1",
		"-node-name", "node-1",
		"-advertise-addr", "fd00:30::10",
		"-mesh-advertise-endpoint", "192.0.2.10:51820",
		"-controlplane-addresses", "replica-a:9443, replica-b:9443",
		"-ca-file", "ca.crt",
		"-bootstrap-token", "test-token",
		"-data-dir", dataDir,
		"-cpu-millis", "4000",
		"-memory-mebibytes", "8192",
		"-reserved-cpu-millis", "750",
		"-reserved-memory-mebibytes", "1024",
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
	if cfg.Mesh.WireGuard.AdvertiseEndpoint != "192.0.2.10:51820" {
		t.Fatalf("unexpected WireGuard endpoint %q", cfg.Mesh.WireGuard.AdvertiseEndpoint)
	}
	if cfg.Mesh.WireGuard.PrivateKey == "" {
		t.Fatal("expected bootstrap to load wireguard private key")
	}
	if cfg.ControlPlane.TLS.BootstrapToken != "test-token" {
		t.Fatalf("unexpected bootstrap token %q", cfg.ControlPlane.TLS.BootstrapToken)
	}
	if got, want := strings.Join(cfg.ControlPlane.Addresses, ","), "replica-a:9443,replica-b:9443"; got != want {
		t.Fatalf("unexpected control-plane discovery seeds %q, want %q", got, want)
	}
	if got := cfg.Node.Resources.AdvertisedCPUMillis(); got != 3250 {
		t.Fatalf("unexpected advertised CPU capacity %d", got)
	}
	if got := cfg.Node.Resources.AdvertisedMemoryMebibytes(); got != 7168 {
		t.Fatalf("unexpected advertised memory capacity %d", got)
	}
}
