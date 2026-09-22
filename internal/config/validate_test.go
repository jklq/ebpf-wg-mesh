package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestFinalizeControlPlaneAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg := ControlPlaneConfig{
		Profile: ProfileDevelopment,
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
	if got := cfg.Ingress.XDSListen; got != "127.0.0.1:18000" {
		t.Fatalf("expected loopback xDS listener, got %q", got)
	}
	if got, want := cfg.InternalGRPC.TLS.RevokedClientCertSerialsFile, filepath.Join(cfg.StateDir, "pki", "revoked-client-cert-serials.txt"); got != want {
		t.Fatalf("unexpected default client certificate revocation file %q, want %q", got, want)
	}
	if cfg.Failover.ReconcileIntervalSeconds != 60 || cfg.Failover.UnhealthyThresholdSeconds != 30 {
		t.Fatalf("unexpected failover defaults: %+v", cfg.Failover)
	}
}

func TestFinalizeControlPlaneRequiresAdvertiseAddrForMultipleReplicas(t *testing.T) {
	t.Parallel()

	cfg := ControlPlaneConfig{
		Profile:  ProfileDevelopment,
		Database: DatabaseConfig{URL: "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable"},
		InternalGRPC: ListenerConfig{TLS: ServerTLSConfig{
			BootstrapTokens: []AgentBootstrapToken{{AgentID: "node-a", Token: "token-a"}},
		}},
		ReplicaAddresses: []string{"replica-a:9443", "replica-b:9443"},
	}
	if err := FinalizeControlPlane(&cfg); err == nil || !strings.Contains(err.Error(), "advertiseAddr") {
		t.Fatalf("expected advertiseAddr requirement, got %v", err)
	}

	cfg.AdvertiseAddr = "replica-a:9443"
	if err := FinalizeControlPlane(&cfg); err != nil {
		t.Fatalf("FinalizeControlPlane: %v", err)
	}
	if got := strings.Join(cfg.ReplicaAddresses, ","); got != "replica-a:9443,replica-b:9443" {
		t.Fatalf("replica addresses = %q", got)
	}
}

func TestFinalizeControlPlaneValidatesXDSListen(t *testing.T) {
	t.Parallel()

	base := ControlPlaneConfig{
		Profile:  ProfileDevelopment,
		Database: DatabaseConfig{URL: "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable"},
		InternalGRPC: ListenerConfig{TLS: ServerTLSConfig{
			BootstrapTokens: []AgentBootstrapToken{{AgentID: "node-a", Token: "token-a"}},
		}},
		Ingress: IngressConfig{
			XDSListen: "not-an-addr",
		},
	}

	if err := FinalizeControlPlane(&base); err == nil {
		t.Fatal("expected malformed xDS listen address to be rejected")
	}
	base.Ingress.XDSListen = "0.0.0.0:18000"
	if err := FinalizeControlPlane(&base); err != nil {
		t.Fatalf("expected explicit xDS listen address to pass: %v", err)
	}
}

func TestFinalizeControlPlaneValidatesDashboardConfig(t *testing.T) {
	t.Parallel()

	cfg := ControlPlaneConfig{
		Profile: ProfileDevelopment,
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
		Profile: ProfileDevelopment,
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
			Addresses: []string{"controlplane:9443"},
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
			Addresses: []string{"controlplane:9443"},
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
					Endpoint:   "node-2.example:51820",
					AllowedIPs: []string{"fd00:44::/128"},
				}},
			},
		},
	}

	err := FinalizeAgent(&cfg)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if got := err.Error(); got != `agent.mesh.wireguard peer "node-2" invalid endpoint: host must be an IP:port endpoint: "node-2.example:51820"` {
		t.Fatalf("unexpected error %q", got)
	}
}

func TestFinalizeAgentRejectsLoopbackAdvertiseAddr(t *testing.T) {
	t.Parallel()
	cfg := validMinimalProductionAgent()
	cfg.Node.AdvertiseAddr = "::1"
	cfg.Mesh.WireGuard.AdvertiseEndpoint = ""
	err := FinalizeAgent(&cfg)
	if err == nil {
		t.Fatal("expected loopback advertiseAddr to fail")
	}
	if got := err.Error(); !strings.Contains(got, "advertiseAddr") {
		t.Fatalf("error should name advertiseAddr, got %q", got)
	}
}

func TestFinalizeAgentRejectsInvalidOrMismatchedMeshHostIPv6(t *testing.T) {
	t.Parallel()
	for name, host := range map[string]string{"loopback": "::1", "mismatch": "fd00:30::11"} {
		t.Run(name, func(t *testing.T) {
			cfg := validMinimalProductionAgent()
			cfg.Mesh.Host.IPv6 = host
			if err := FinalizeAgent(&cfg); err == nil {
				t.Fatalf("mesh host %q unexpectedly accepted", host)
			}
		})
	}
}

func TestFinalizeAgentAcceptsEquivalentMeshHostIPv6Spelling(t *testing.T) {
	t.Parallel()
	cfg := validMinimalProductionAgent()
	cfg.Mesh.Host.IPv6 = strings.ToUpper(cfg.Node.AdvertiseAddr)
	if err := FinalizeAgent(&cfg); err != nil {
		t.Fatalf("equivalent IPv6 spelling rejected: %v", err)
	}
}

func TestFinalizeAgentAcceptsIPv4WireGuardEndpoint(t *testing.T) {
	t.Parallel()

	cfg := validMinimalProductionAgent()
	cfg.Mesh.WireGuard.AdvertiseEndpoint = "192.0.2.10:51820"
	if err := FinalizeAgent(&cfg); err != nil {
		t.Fatalf("FinalizeAgent: %v", err)
	}
}

func TestFinalizeAgentTrimsWireGuardEndpoint(t *testing.T) {
	t.Parallel()
	cfg := validMinimalProductionAgent()
	cfg.Mesh.WireGuard.AdvertiseEndpoint = " 192.0.2.10:51820\t"
	if err := FinalizeAgent(&cfg); err != nil {
		t.Fatalf("FinalizeAgent: %v", err)
	}
	if got := cfg.Mesh.WireGuard.AdvertiseEndpoint; got != "192.0.2.10:51820" {
		t.Fatalf("advertise endpoint = %q", got)
	}
}

func TestFinalizeAgentRejectsSignedWireGuardPort(t *testing.T) {
	t.Parallel()
	cfg := validMinimalProductionAgent()
	cfg.Mesh.WireGuard.AdvertiseEndpoint = "[2001:db8::10]:+51820"
	if err := FinalizeAgent(&cfg); err == nil {
		t.Fatal("signed WireGuard port was accepted")
	}
}

func TestFinalizeAgentRejectsIPv4LimitedBroadcastEndpoint(t *testing.T) {
	t.Parallel()
	cfg := validMinimalProductionAgent()
	cfg.Mesh.WireGuard.AdvertiseEndpoint = "255.255.255.255:51820"
	if err := FinalizeAgent(&cfg); err == nil {
		t.Fatal("limited broadcast WireGuard endpoint was accepted")
	}
}

func TestFinalizeControlPlaneRejectsEnabledGitHubWithoutRegistryConfig(t *testing.T) {
	t.Parallel()

	cfg := ControlPlaneConfig{
		Profile: ProfileDevelopment,
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

func TestFinalizeControlPlaneValidatesSourceArchiveS3(t *testing.T) {
	t.Parallel()

	base := func() ControlPlaneConfig {
		cfg := validControlPlaneConfigForRegistryTest()
		cfg.SourceArchives = SourceArchiveConfig{
			Provider: SourceArchiveProviderS3,
			S3: SourceArchiveS3Config{
				Endpoint:              "https://s3.us-east-1.amazonaws.com",
				Region:                "us-east-1",
				Bucket:                "platform-source-archives",
				RequestTimeoutSeconds: 30,
				MaxRetries:            3,
			},
		}
		return cfg
	}
	if err := FinalizeControlPlane(func() *ControlPlaneConfig { cfg := base(); return &cfg }()); err != nil {
		t.Fatalf("valid S3 config rejected: %v", err)
	}

	cases := map[string]func(*ControlPlaneConfig){
		"unknown provider":  func(cfg *ControlPlaneConfig) { cfg.SourceArchives.Provider = "gcs" },
		"s3 with directory": func(cfg *ControlPlaneConfig) { cfg.SourceArchives.Directory = "/var/lib/archives" },
		"file with s3 fields": func(cfg *ControlPlaneConfig) {
			cfg.SourceArchives.Provider = SourceArchiveProviderFile
			cfg.SourceArchives.Directory = "/var/lib/archives"
		},
		"missing endpoint":  func(cfg *ControlPlaneConfig) { cfg.SourceArchives.S3.Endpoint = "" },
		"relative endpoint": func(cfg *ControlPlaneConfig) { cfg.SourceArchives.S3.Endpoint = "s3.us-east-1.amazonaws.com" },
		"missing region":    func(cfg *ControlPlaneConfig) { cfg.SourceArchives.S3.Region = "" },
		"short bucket":      func(cfg *ControlPlaneConfig) { cfg.SourceArchives.S3.Bucket = "ab" },
		"uppercase bucket":  func(cfg *ControlPlaneConfig) { cfg.SourceArchives.S3.Bucket = "Platform-Archives" },
		"absolute prefix":   func(cfg *ControlPlaneConfig) { cfg.SourceArchives.S3.Prefix = "/tenant" },
		"unknown sse":       func(cfg *ControlPlaneConfig) { cfg.SourceArchives.S3.ServerSideEncryption = "AES128" },
		"kms key without kms": func(cfg *ControlPlaneConfig) {
			cfg.SourceArchives.S3.ServerSideEncryption = "AES256"
			cfg.SourceArchives.S3.SSEKMSKeyID = "key"
		},
		"timeout too large": func(cfg *ControlPlaneConfig) { cfg.SourceArchives.S3.RequestTimeoutSeconds = 301 },
		"retries too many":  func(cfg *ControlPlaneConfig) { cfg.SourceArchives.S3.MaxRetries = 11 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := base()
			mutate(&cfg)
			if err := FinalizeControlPlane(&cfg); err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
		})
	}
}

func TestFinalizeControlPlaneDefaultsSourceArchiveProvider(t *testing.T) {
	t.Parallel()

	cfg := validControlPlaneConfigForRegistryTest()
	cfg.SourceArchives = SourceArchiveConfig{}
	if err := FinalizeControlPlane(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.SourceArchives.Provider != SourceArchiveProviderFile {
		t.Fatalf("provider = %q, want file", cfg.SourceArchives.Provider)
	}

	cfg = validControlPlaneConfigForRegistryTest()
	cfg.SourceArchives = SourceArchiveConfig{S3: SourceArchiveS3Config{
		Endpoint: "https://s3.us-east-1.amazonaws.com",
		Region:   "us-east-1",
		Bucket:   "platform-source-archives",
	}}
	if err := FinalizeControlPlane(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.SourceArchives.Provider != SourceArchiveProviderS3 {
		t.Fatalf("provider = %q, want s3", cfg.SourceArchives.Provider)
	}
	if cfg.SourceArchives.S3.RequestTimeoutSeconds != 30 || cfg.SourceArchives.S3.MaxRetries != 3 {
		t.Fatalf("s3 defaults = %+v", cfg.SourceArchives.S3)
	}
}

func validControlPlaneConfigForRegistryTest() ControlPlaneConfig {
	return ControlPlaneConfig{
		Profile: ProfileDevelopment,
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
	if cfg.RailpackBinary != "railpack" {
		t.Fatalf("unexpected railpack binary %q", cfg.RailpackBinary)
	}
	if cfg.RailpackFrontendImage != "ghcr.io/railwayapp/railpack-frontend:latest" {
		t.Fatalf("unexpected railpack frontend image %q", cfg.RailpackFrontendImage)
	}
	if cfg.PollIntervalSeconds <= 0 || cfg.HeartbeatIntervalSeconds <= 0 {
		t.Fatalf("expected positive intervals, got poll=%d heartbeat=%d", cfg.PollIntervalSeconds, cfg.HeartbeatIntervalSeconds)
	}
	if cfg.Executor != "development" {
		t.Fatalf("unexpected executor %q", cfg.Executor)
	}
	if cfg.Limits.TimeoutSeconds <= 0 || cfg.Limits.MemoryBytes <= 0 || cfg.Limits.CPUSeconds <= 0 ||
		cfg.Limits.MaxFileBytes <= 0 || cfg.Limits.MaxProcesses <= 0 || cfg.Limits.MaxWorkspaceBytes <= 0 {
		t.Fatalf("expected positive execution limits, got %+v", cfg.Limits)
	}
	if len(cfg.Network.DeniedCIDRs) == 0 {
		t.Fatal("expected default denied egress CIDRs")
	}
}

func validBuilderConfigForTest() BuilderConfig {
	return BuilderConfig{
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
}

func TestFinalizeBuilderRejectsUnknownExecutor(t *testing.T) {
	t.Parallel()

	cfg := validBuilderConfigForTest()
	cfg.Executor = "microvm"
	if err := FinalizeBuilder(&cfg); err == nil || !strings.Contains(err.Error(), "builder.executor") {
		t.Fatalf("expected executor error, got %v", err)
	}
}

func TestFinalizeBuilderCacheMode(t *testing.T) {
	t.Parallel()

	cfg := validBuilderConfigForTest()
	if err := FinalizeBuilder(&cfg); err != nil {
		t.Fatalf("FinalizeBuilder: %v", err)
	}
	if cfg.Cache.Mode != "none" {
		t.Fatalf("cache mode must default to none, got %q", cfg.Cache.Mode)
	}
	cfg.Cache.Mode = "content-addressed"
	if err := FinalizeBuilder(&cfg); err != nil {
		t.Fatalf("content-addressed mode must validate: %v", err)
	}
	cfg.Cache.Mode = "shared"
	if err := FinalizeBuilder(&cfg); err == nil || !strings.Contains(err.Error(), "builder.cache.mode") {
		t.Fatalf("expected cache mode error, got %v", err)
	}
}

func TestFinalizeBuilderRejectsBadLimitsAndNetwork(t *testing.T) {
	t.Parallel()

	// Defaults fill unset limits, so validate an explicitly cleared
	// limit directly.
	cfg := validBuilderConfigForTest()
	if err := FinalizeBuilder(&cfg); err != nil {
		t.Fatalf("FinalizeBuilder: %v", err)
	}
	cfg.Limits.MaxProcesses = 0
	if err := validateBuilder(cfg); err == nil || !strings.Contains(err.Error(), "maxProcesses") {
		t.Fatalf("expected maxProcesses error, got %v", err)
	}

	bad := validBuilderConfigForTest()
	bad.Network.DeniedCIDRs = []string{"not-a-cidr"}
	if err := FinalizeBuilder(&bad); err == nil || !strings.Contains(err.Error(), "deniedCidrs") {
		t.Fatalf("expected deniedCidrs error, got %v", err)
	}
}

func TestBuilderStartupContractLabelsDevelopmentExecutor(t *testing.T) {
	t.Parallel()

	cfg := validBuilderConfigForTest()
	cfg.Executor = "development"
	contract := BuilderStartupContract(cfg).String()
	if !strings.Contains(contract, "executor_development") {
		t.Fatalf("expected executor selection in contract %q", contract)
	}
	if !strings.Contains(contract, "executor_non_isolating") {
		t.Fatalf("expected non-isolating label in contract %q", contract)
	}
}

func TestBuilderStartupContractHardenedIsIsolating(t *testing.T) {
	t.Parallel()

	cfg := validBuilderConfigForTest()
	cfg.Executor = "hardened"
	contract := BuilderStartupContract(cfg).String()
	if !strings.Contains(contract, "executor_hardened") {
		t.Fatalf("expected executor selection in contract %q", contract)
	}
	if strings.Contains(contract, "executor_non_isolating") {
		t.Fatalf("hardened executor must not be labeled non-isolating: %q", contract)
	}
}

func TestFinalizeBuilderDefaultsExecutorByProfile(t *testing.T) {
	t.Parallel()

	dev := validBuilderConfigForTest()
	if err := FinalizeBuilder(&dev); err != nil {
		t.Fatalf("FinalizeBuilder development: %v", err)
	}
	if dev.Executor != "development" {
		t.Fatalf("development must default to the development executor, got %q", dev.Executor)
	}

	prod := validBuilderConfigForTest()
	prod.Profile = ProfileProduction
	prod.Health.Listen = "127.0.0.1:18082"
	prod.ControlPlane.Address = "controlplane.example.test:9443"
	prod.ControlPlane.TLS.ServerName = "controlplane.example.test"
	prod.Sandbox.Image = "registry.example.test/platform/build-sandbox:1"
	if err := FinalizeBuilder(&prod); err != nil {
		t.Fatalf("FinalizeBuilder production: %v", err)
	}
	if prod.Executor != "hardened" {
		t.Fatalf("production must default to the hardened executor, got %q", prod.Executor)
	}
}

func TestFinalizeBuilderProductionRefusesDevelopmentExecutor(t *testing.T) {
	t.Parallel()

	cfg := validBuilderConfigForTest()
	cfg.Profile = ProfileProduction
	cfg.Health.Listen = "127.0.0.1:18082"
	cfg.ControlPlane.Address = "controlplane.example.test:9443"
	cfg.ControlPlane.TLS.ServerName = "controlplane.example.test"
	cfg.Executor = "development"
	if err := FinalizeBuilder(&cfg); err == nil || !strings.Contains(err.Error(), "must not be development") {
		t.Fatalf("expected development refusal in production, got %v", err)
	}
}

func TestFinalizeBuilderHardenedRequiresSandbox(t *testing.T) {
	t.Parallel()

	cfg := validBuilderConfigForTest()
	cfg.Executor = "hardened"
	if err := FinalizeBuilder(&cfg); err == nil || !strings.Contains(err.Error(), "builder.sandbox.image") {
		t.Fatalf("expected sandbox image error, got %v", err)
	}

	cfg.Sandbox.Image = "registry.example.test/platform/build-sandbox:1"
	cfg.Sandbox.Backend = "microvm"
	if err := FinalizeBuilder(&cfg); err == nil || !strings.Contains(err.Error(), "builder.sandbox.backend") {
		t.Fatalf("expected sandbox backend error, got %v", err)
	}

	cfg.Sandbox.Backend = "containerd"
	cfg.Sandbox.Nameservers = []string{"not-an-ip"}
	if err := FinalizeBuilder(&cfg); err == nil || !strings.Contains(err.Error(), "builder.sandbox.nameservers") {
		t.Fatalf("expected sandbox nameserver error, got %v", err)
	}

	cfg.Sandbox.Nameservers = []string{"10.0.0.53"}
	if err := FinalizeBuilder(&cfg); err != nil {
		t.Fatalf("FinalizeBuilder hardened: %v", err)
	}
	if cfg.Sandbox.Socket == "" || cfg.Sandbox.Namespace == "" || cfg.Sandbox.Runtime == "" ||
		cfg.Sandbox.Snapshotter == "" || cfg.Sandbox.CNIPluginDir == "" || cfg.Sandbox.CNIConfDir == "" ||
		cfg.Sandbox.CNINetwork == "" || cfg.Sandbox.BuildkitdBinary == "" {
		t.Fatalf("expected sandbox defaults, got %+v", cfg.Sandbox)
	}
}
