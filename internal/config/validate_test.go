package config

import (
	"path/filepath"
	"strings"
	"testing"
)

const testUserAssertionHMACSecret = "test-user-assertion-secret-at-least-32-bytes"

func TestFinalizeControlPlaneAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg := ControlPlaneConfig{
		Profile:        ProfileDevelopment,
		UserAssertions: UserAssertionConfig{HMACSecret: testUserAssertionHMACSecret},
		Database: DatabaseConfig{
			URL: "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable",
		},
		InternalGRPC: ListenerConfig{
			TLS: ServerTLSConfig{
				BootstrapTokens: []AgentBootstrapToken{{AgentID: "node-a", Token: "token-a"}},
			},
		},
		Dashboard: ManagedDashboardConfig{
			Enabled: false,
		},
	}

	if err := FinalizeControlPlane(&cfg); err != nil {
		t.Fatalf("FinalizeControlPlane: %v", err)
	}
	if got := cfg.Mesh.NetworkCIDR; got != "fd00:44::/64" {
		t.Fatalf("unexpected mesh network cidr %q", got)
	}
	if cfg.Database.MaxOpenConns < 32 {
		t.Fatalf("expected default max open conns >= 32, got %d", cfg.Database.MaxOpenConns)
	}
	if cfg.Database.MaxIdleConns <= 0 || cfg.Database.MaxIdleConns > cfg.Database.MaxOpenConns {
		t.Fatalf("unexpected idle/open conns %d/%d", cfg.Database.MaxIdleConns, cfg.Database.MaxOpenConns)
	}
	if len(cfg.InternalGRPC.TLS.ServerNames) == 0 {
		t.Fatal("expected default internal server names")
	}
	if got := cfg.Ingress.AdminListen; got != "127.0.0.1:2019" {
		t.Fatalf("expected loopback Caddy admin listener, got %q", got)
	}
	if got, want := cfg.InternalGRPC.TLS.RevokedClientCertSerialsFile, filepath.Join(cfg.StateDir, "pki", "revoked-client-cert-serials.txt"); got != want {
		t.Fatalf("unexpected default client certificate revocation file %q, want %q", got, want)
	}
	if cfg.Failover.ReconcileIntervalSeconds != 5 || cfg.Failover.UnhealthyThresholdSeconds != 30 {
		t.Fatalf("unexpected failover defaults: %+v", cfg.Failover)
	}
}

func TestFinalizeControlPlaneRejectsNonLoopbackCaddyAdminWithoutOptIn(t *testing.T) {
	t.Parallel()

	base := ControlPlaneConfig{
		Profile:        ProfileDevelopment,
		UserAssertions: UserAssertionConfig{HMACSecret: testUserAssertionHMACSecret},
		Database:       DatabaseConfig{URL: "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable"},
		InternalGRPC: ListenerConfig{TLS: ServerTLSConfig{
			BootstrapTokens: []AgentBootstrapToken{{AgentID: "node-a", Token: "token-a"}},
		}},
		Ingress: IngressConfig{
			AdminURL:    "http://10.0.0.2:2019/load",
			AdminListen: ":2019",
		},
	}

	if err := FinalizeControlPlane(&base); err == nil {
		t.Fatal("expected non-loopback Caddy admin configuration to be rejected")
	}
	base.Ingress.AllowNonLoopbackAdmin = true
	if err := FinalizeControlPlane(&base); err != nil {
		t.Fatalf("expected explicit non-loopback opt-in to pass: %v", err)
	}
}

func TestFinalizeControlPlaneValidatesDashboardConfig(t *testing.T) {
	t.Parallel()

	cfg := ControlPlaneConfig{
		Profile:        ProfileDevelopment,
		UserAssertions: UserAssertionConfig{HMACSecret: testUserAssertionHMACSecret},
		InternalGRPC: ListenerConfig{
			Listen: "127.0.0.1:9443",
			TLS: ServerTLSConfig{
				ServerNames:             []string{"controlplane"},
				BootstrapTokens:         []AgentBootstrapToken{{AgentID: "node-a", Token: "token-a"}},
				ServerCertValidityHours: 24,
				ClientCertValidityHours: 24,
			},
		},
		Database: DatabaseConfig{
			URL: "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable",
		},
		StateDir: "var/controlplane",
		Ingress: IngressConfig{
			PublicAddr: "platform.example.test",
		},
		Dashboard: ManagedDashboardConfig{
			Enabled: true,
		},
		Mesh: ControlPlaneMeshConfig{
			InterfaceName:    "wg0",
			ListenPort:       51820,
			NetworkCIDR:      "fd00:44::/64",
			WorkloadPoolCIDR: "fd00:200::/48",
		},
	}

	err := FinalizeControlPlane(&cfg)
	if err == nil {
		t.Fatal("expected dashboard validation error")
	}
	if got := err.Error(); got != "controlplane.dashboard.image is required when dashboard is enabled" {
		t.Fatalf("unexpected error %q", got)
	}
}

func TestFinalizeControlPlaneAllowsGitHubWithoutDashboardInstallURL(t *testing.T) {
	t.Parallel()

	cfg := ControlPlaneConfig{
		Profile:        ProfileDevelopment,
		UserAssertions: UserAssertionConfig{HMACSecret: testUserAssertionHMACSecret},
		InternalGRPC: ListenerConfig{
			Listen: "127.0.0.1:9443",
			TLS: ServerTLSConfig{
				ServerNames:             []string{"controlplane"},
				BootstrapTokens:         []AgentBootstrapToken{{AgentID: "node-a", Token: "token-a"}},
				ServerCertValidityHours: 24,
				ClientCertValidityHours: 24,
			},
		},
		Database: DatabaseConfig{
			URL: "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable",
		},
		StateDir: "var/controlplane",
		Ingress: IngressConfig{
			PublicAddr: "platform.example.test",
		},
		Dashboard: ManagedDashboardConfig{
			Enabled:          true,
			Image:            "ghcr.io/example/dashboard:latest",
			ProjectSystemKey: "mesh",
			ServiceName:      "dashboard",
			ServiceCallerID:  "dashboard",
			TrustedAgentID:   "trusted-dashboard-node",
			PublicDomain:     "dashboard.example.test",
			ControlPlaneAddr: "controlplane:9443",
		},
		GitHub: GitHubAppConfig{
			Enabled:       true,
			AppID:         123,
			WebhookSecret: "secret",
			PrivateKeyPEM: "-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----",
			APIBaseURL:    "https://api.github.com",
		},
		Registry: RegistryConfig{
			Host: "registry.example.test",
		},
		Mesh: ControlPlaneMeshConfig{
			InterfaceName:    "wg0",
			ListenPort:       51820,
			NetworkCIDR:      "fd00:44::/64",
			WorkloadPoolCIDR: "fd00:200::/48",
		},
	}

	if err := FinalizeControlPlane(&cfg); err != nil {
		t.Fatalf("FinalizeControlPlane: %v", err)
	}
	if cfg.Dashboard.GitHubInstallURL != "" {
		t.Fatalf("expected empty dashboard install URL, got %q", cfg.Dashboard.GitHubInstallURL)
	}
}

func TestFinalizeAgentRejectsReservedResourcesThatConsumeAllCapacity(t *testing.T) {
	t.Parallel()

	cfg := AgentConfig{
		Profile: ProfileDevelopment,
		Node: NodeConfig{
			ID:            "node-1",
			Name:          "node-1",
			AdvertiseAddr: "fd00:30::10",
			Resources: NodeResourcesConfig{
				CPUMillis:               500,
				MemoryMebibytes:         512,
				ReservedCPUMillis:       500,
				ReservedMemoryMebibytes: 512,
			},
		},
		ControlPlane: ControlPlaneClientConfig{
			Address: "controlplane:9443",
			TLS: ClientTLSConfig{
				CAFile:             "ca.crt",
				ServerName:         "controlplane",
				BootstrapToken:     "token-a",
				RenewBeforeMinutes: 30,
			},
		},
		Mesh: MeshConfig{
			Host: HostConfig{IPv6: "fd00:30::10"},
		},
	}
	err := FinalizeAgent(&cfg)
	if err == nil || !strings.Contains(err.Error(), "reservedCpuMillis must be less than cpuMillis") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestFinalizeAgentRejectsInvalidWireGuardPeerEndpoint(t *testing.T) {
	t.Parallel()

	cfg := AgentConfig{
		Profile: ProfileDevelopment,
		Node: NodeConfig{
			ID:            "node-1",
			Name:          "node-1",
			AdvertiseAddr: "fd00:30::10",
		},
		ControlPlane: ControlPlaneClientConfig{
			Address: "controlplane:9443",
			TLS: ClientTLSConfig{
				CAFile:             "ca.crt",
				ServerName:         "controlplane",
				BootstrapToken:     "token-a",
				RenewBeforeMinutes: 30,
			},
		},
		Mesh: MeshConfig{
			Host: HostConfig{IPv6: "fd00:30::10"},
			WireGuard: WireGuard{
				ListenPort: 51820,
				Peers: []PeerConfig{{
					Name:       "node-2",
					PublicKey:  "pubkey",
					Endpoint:   "127.0.0.1:51820",
					AllowedIPs: []string{"fd00:44::/128"},
				}},
			},
		},
	}

	err := FinalizeAgent(&cfg)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if got := err.Error(); got != `agent.mesh.wireguard peer "node-2" invalid endpoint: address 127.0.0.1: no such host` &&
		got != `agent.mesh.wireguard peer "node-2" invalid endpoint: host must be IPv6: "127.0.0.1"` {
		t.Fatalf("unexpected error %q", got)
	}
}

func TestFinalizeControlPlaneRejectsEnabledGitHubWithoutRegistryConfig(t *testing.T) {
	t.Parallel()

	cfg := ControlPlaneConfig{
		Profile:        ProfileDevelopment,
		UserAssertions: UserAssertionConfig{HMACSecret: testUserAssertionHMACSecret},
		InternalGRPC: ListenerConfig{
			Listen: "127.0.0.1:9443",
			TLS: ServerTLSConfig{
				ServerNames:             []string{"controlplane"},
				BootstrapTokens:         []AgentBootstrapToken{{AgentID: "node-a", Token: "token-a"}},
				ServerCertValidityHours: 24,
				ClientCertValidityHours: 24,
			},
		},
		Database: DatabaseConfig{URL: "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable"},
		StateDir: "var/controlplane",
		Ingress:  IngressConfig{PublicAddr: "platform.example.test"},
		GitHub: GitHubAppConfig{
			Enabled:       true,
			AppID:         123,
			WebhookSecret: "secret",
			PrivateKeyPEM: "pem",
			APIBaseURL:    "https://api.github.com",
		},
		Mesh: ControlPlaneMeshConfig{
			InterfaceName:    "wg0",
			ListenPort:       51820,
			NetworkCIDR:      "fd00:44::/64",
			WorkloadPoolCIDR: "fd00:200::/48",
		},
	}

	err := FinalizeControlPlane(&cfg)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if got := err.Error(); got != "controlplane.registry.host is required when GitHub is enabled" {
		t.Fatalf("unexpected error %q", got)
	}
}

func TestFinalizeControlPlaneRejectsInvalidRegistryAuthListen(t *testing.T) {
	t.Parallel()

	cfg := validControlPlaneConfigForRegistryTest()
	cfg.Registry.AuthListen = "not a tcp address"
	err := FinalizeControlPlane(&cfg)
	if err == nil || !strings.Contains(err.Error(), "controlplane.registry.authListen") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestFinalizeControlPlaneRejectsLongLivedRegistryCredentials(t *testing.T) {
	t.Parallel()

	cfg := validControlPlaneConfigForRegistryTest()
	cfg.Registry.CredentialTTLSeconds = 901
	err := FinalizeControlPlane(&cfg)
	if err == nil || err.Error() != "controlplane.registry.credentialTTLSeconds must be between 60 and 900" {
		t.Fatalf("unexpected error %v", err)
	}
}

func validControlPlaneConfigForRegistryTest() ControlPlaneConfig {
	return ControlPlaneConfig{
		Profile:        ProfileDevelopment,
		UserAssertions: UserAssertionConfig{HMACSecret: testUserAssertionHMACSecret},
		InternalGRPC: ListenerConfig{
			Listen: "127.0.0.1:9443",
			TLS: ServerTLSConfig{
				ServerNames:             []string{"controlplane"},
				BootstrapTokens:         []AgentBootstrapToken{{AgentID: "node-a", Token: "token-a"}},
				ServerCertValidityHours: 24,
				ClientCertValidityHours: 24,
			},
		},
		Database: DatabaseConfig{URL: "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable"},
		StateDir: "var/controlplane",
		Ingress:  IngressConfig{PublicAddr: "platform.example.test"},
		GitHub: GitHubAppConfig{
			Enabled:       true,
			AppID:         123,
			WebhookSecret: "secret",
			PrivateKeyPEM: "pem",
			APIBaseURL:    "https://api.github.com",
		},
		Registry: RegistryConfig{
			Host:                 "registry.example.test",
			CredentialTTLSeconds: 300,
		},
		Mesh: ControlPlaneMeshConfig{
			InterfaceName:    "wg0",
			ListenPort:       51820,
			NetworkCIDR:      "fd00:44::/64",
			WorkloadPoolCIDR: "fd00:200::/48",
		},
	}
}

func TestFinalizeBuilderAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg := BuilderConfig{
		Profile: ProfileDevelopment,
		ID:      "builder-1",
		Name:    "builder-1",
		ControlPlane: BuilderControlPlaneConfig{
			Address: "127.0.0.1:9443",
			TLS: InternalClientTLSConfig{
				CAFile:     "ca.crt",
				CertFile:   "client.crt",
				KeyFile:    "client.key",
				ServerName: "controlplane",
			},
		},
	}

	if err := FinalizeBuilder(&cfg); err != nil {
		t.Fatalf("FinalizeBuilder: %v", err)
	}
	if cfg.WorkDir != "var/builder" {
		t.Fatalf("unexpected work dir %q", cfg.WorkDir)
	}
	if cfg.BuildctlBinary != "buildctl" {
		t.Fatalf("unexpected buildctl binary %q", cfg.BuildctlBinary)
	}
	if cfg.PollIntervalSeconds <= 0 || cfg.HeartbeatIntervalSeconds <= 0 {
		t.Fatalf("expected positive intervals, got poll=%d heartbeat=%d", cfg.PollIntervalSeconds, cfg.HeartbeatIntervalSeconds)
	}
}
