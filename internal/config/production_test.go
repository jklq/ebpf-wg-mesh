package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFinalizeRejectsUnknownProfile(t *testing.T) {
	t.Parallel()

	cfg := validMinimalProductionControlPlane(t)
	cfg.Profile = "staging"
	if err := FinalizeControlPlane(&cfg); err == nil || !strings.Contains(err.Error(), "profile must be development or production") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestFinalizeTreatsMissingProfileAsProduction(t *testing.T) {
	t.Parallel()

	cfg := validMinimalProductionControlPlane(t)
	cfg.Profile = ""
	if err := FinalizeControlPlane(&cfg); err != nil {
		t.Fatalf("FinalizeControlPlane: %v", err)
	}
	if cfg.Profile != ProfileProduction {
		t.Fatalf("expected missing profile to become production, got %q", cfg.Profile)
	}
}

func TestFinalizeAcceptsValidMinimalProductionConfigs(t *testing.T) {
	t.Parallel()

	cp := validMinimalProductionControlPlane(t)
	if err := FinalizeControlPlane(&cp); err != nil {
		t.Fatalf("FinalizeControlPlane: %v", err)
	}
	contract := ControlPlaneStartupContract(cp).String()
	for _, part := range []string{
		"component=controlplane",
		"profile=production",
		"features=ingress,registry_auth,source_storage,sandbox_production",
		"database=durable",
		"source_storage=durable",
		"ingress_admin=loopback",
	} {
		if !strings.Contains(contract, part) {
			t.Fatalf("startup contract %q missing %q", contract, part)
		}
	}
	if strings.Contains(contract, "postgresql://") || strings.Contains(contract, "secret") {
		t.Fatalf("startup contract leaked private data: %q", contract)
	}

	agent := validMinimalProductionAgent()
	if err := FinalizeAgent(&agent); err != nil {
		t.Fatalf("FinalizeAgent: %v", err)
	}
	if contract := AgentStartupContract(agent).String(); strings.Contains(contract, "token-a") {
		t.Fatalf("agent startup contract leaked private data: %q", contract)
	}
	builder := validMinimalProductionBuilder()
	if err := FinalizeBuilder(&builder); err != nil {
		t.Fatalf("FinalizeBuilder: %v", err)
	}
	if contract := BuilderStartupContract(builder).String(); strings.Contains(contract, "builder.key") || strings.Contains(contract, "builder.crt") {
		t.Fatalf("builder startup contract leaked private data: %q", contract)
	}
}

func TestFinalizeProductionRejectsInsecureSettings(t *testing.T) {
	t.Parallel()

	t.Run("development users", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionControlPlane(t)
		cfg.Dashboard.DevUsers = []BootstrapUser{{ID: "dev", Email: "dev@example.test"}}
		mustReject(t, FinalizeControlPlane(&cfg), "devUsers")
	})
	t.Run("insecure cookies", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionControlPlane(t)
		cfg.Dashboard.Enabled = true
		cfg.Dashboard.Image = "ghcr.io/example/dashboard:latest"
		cfg.Dashboard.TrustedAgentID = "node-a"
		cfg.Dashboard.Env = map[string]string{
			"DASHBOARD_LOCAL_DOMAIN_SUFFIX": "localtest.me",
		}
		mustReject(t, FinalizeControlPlane(&cfg), "insecure cookies")
	})
	t.Run("wildcard tls identity", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionControlPlane(t)
		cfg.InternalGRPC.TLS.ServerNames = []string{"*.example.test"}
		mustReject(t, FinalizeControlPlane(&cfg), "wildcard identity")
	})
	t.Run("missing tls identity", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionControlPlane(t)
		cfg.InternalGRPC.TLS.ServerNames = []string{" "}
		mustReject(t, FinalizeControlPlane(&cfg), "required in production")
	})
	t.Run("loopback database", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionControlPlane(t)
		cfg.Database.URL = "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable"
		mustReject(t, FinalizeControlPlane(&cfg), "loopback host")
	})
	t.Run("loopback clickhouse", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionControlPlane(t)
		cfg.Logs.ClickHouse.URL = "clickhouse://127.0.0.1:9000/default"
		mustReject(t, FinalizeControlPlane(&cfg), "loopback host")
	})
	t.Run("loopback registry host", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionControlPlane(t)
		cfg.Registry.Host = "127.0.0.1:5000"
		mustReject(t, FinalizeControlPlane(&cfg), "loopback host")
	})
	t.Run("default application secret", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionControlPlane(t)
		cfg.UserAssertions.HMACSecret = defaultUserAssertionHMACSecret
		mustReject(t, FinalizeControlPlane(&cfg), "default or generated secret")
	})
	t.Run("generated registry signing identity", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionControlPlane(t)
		cfg.Registry.SigningCertFile = ""
		cfg.Registry.SigningKeyFile = ""
		mustReject(t, FinalizeControlPlane(&cfg), "signing certificate and key files")
	})
	t.Run("unprotected remote caddy admin", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionControlPlane(t)
		cfg.Ingress.AdminURL = "http://10.0.0.2:2019/load"
		cfg.Ingress.AdminListen = "10.0.0.2:2019"
		cfg.Ingress.AllowNonLoopbackAdmin = true
		mustReject(t, FinalizeControlPlane(&cfg), "remote Caddy administration")
	})
	t.Run("ephemeral source storage", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionControlPlane(t)
		cfg.SourceArchives.Directory = filepath.Join(os.TempDir(), "source-archives")
		mustReject(t, FinalizeControlPlane(&cfg), "ephemeral storage")
	})
	t.Run("relative source storage", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionControlPlane(t)
		cfg.SourceArchives.Directory = "var/controlplane/source-archives"
		mustReject(t, FinalizeControlPlane(&cfg), "ephemeral storage")
	})
	t.Run("incomplete public url", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionControlPlane(t)
		cfg.Ingress.PublicAddr = "platform.local"
		mustReject(t, FinalizeControlPlane(&cfg), "complete public DNS name")
	})
	t.Run("incomplete public certificates", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionControlPlane(t)
		cfg.Ingress.DisableAutomaticHTTPS = true
		mustReject(t, FinalizeControlPlane(&cfg), "public certificates incomplete")
	})
	t.Run("missing health listen", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionControlPlane(t)
		cfg.Health.Listen = ""
		mustReject(t, FinalizeControlPlane(&cfg), "health.listen")
	})
	t.Run("disabled cgroups", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionAgent()
		cfg.Runtime.DisableCgroups = true
		mustReject(t, FinalizeAgent(&cfg), "disableCgroups")
	})
	t.Run("loopback control plane for agent", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionAgent()
		cfg.ControlPlane.Addresses = []string{"127.0.0.1:9443"}
		mustReject(t, FinalizeAgent(&cfg), "loopback host")
	})
	t.Run("wildcard agent tls identity", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionAgent()
		cfg.ControlPlane.TLS.ServerName = "*.controlplane.example.test"
		mustReject(t, FinalizeAgent(&cfg), "wildcard identity")
	})
	t.Run("loopback control plane for builder", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionBuilder()
		cfg.ControlPlane.Address = "127.0.0.1:9443"
		mustReject(t, FinalizeBuilder(&cfg), "loopback host")
	})
	t.Run("missing builder tls identity", func(t *testing.T) {
		t.Parallel()
		cfg := validMinimalProductionBuilder()
		cfg.ControlPlane.TLS.ServerName = " "
		mustReject(t, FinalizeBuilder(&cfg), "required in production")
	})
}

func TestDevelopmentProfileAllowsLocalConveniences(t *testing.T) {
	t.Parallel()

	cfg := ControlPlaneConfig{
		Profile:        ProfileDevelopment,
		UserAssertions: UserAssertionConfig{HMACSecret: testUserAssertionHMACSecret},
		Database:       DatabaseConfig{URL: "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable"},
		InternalGRPC: ListenerConfig{TLS: ServerTLSConfig{
			BootstrapTokens: []AgentBootstrapToken{{AgentID: "node-a", Token: "token-a"}},
		}},
		Ingress: IngressConfig{
			PublicAddr:            "platform.local",
			DisableAutomaticHTTPS: true,
			AllowNonLoopbackAdmin: true,
			AdminURL:              "http://10.0.0.2:2019/load",
			AdminListen:           "10.0.0.2:2019",
		},
		Dashboard: ManagedDashboardConfig{
			DevUsers: []BootstrapUser{{ID: "dev", Email: "dev@example.test"}},
		},
		SourceArchives: SourceArchiveConfig{Directory: filepath.Join(os.TempDir(), "source-archives")},
	}
	if err := FinalizeControlPlane(&cfg); err != nil {
		t.Fatalf("development profile should stay convenient: %v", err)
	}

	agent := AgentConfig{
		Profile: ProfileDevelopment,
		Node: NodeConfig{
			ID:            "node-1",
			Name:          "node-1",
			AdvertiseAddr: "fd00:30::10",
		},
		ControlPlane: ControlPlaneClientConfig{
			Addresses: []string{"127.0.0.1:9443"},
			TLS: ClientTLSConfig{
				CAFile:         "ca.crt",
				BootstrapToken: "token-a",
			},
		},
		Runtime: RuntimeConfig{DisableCgroups: true},
		Mesh:    MeshConfig{Host: HostConfig{IPv6: "fd00:30::10"}},
	}
	if err := FinalizeAgent(&agent); err != nil {
		t.Fatalf("development agent should allow local conveniences: %v", err)
	}
}

func validMinimalProductionControlPlane(t *testing.T) ControlPlaneConfig {
	t.Helper()
	return ControlPlaneConfig{
		Profile: ProfileProduction,
		Health:  HealthConfig{Listen: "127.0.0.1:18080"},
		UserAssertions: UserAssertionConfig{
			HMACSecret: "production-user-assertion-secret-at-least-32",
		},
		InternalGRPC: ListenerConfig{
			Listen: "0.0.0.0:9443",
			TLS: ServerTLSConfig{
				ServerNames:             []string{"controlplane.example.test"},
				BootstrapTokens:         []AgentBootstrapToken{{AgentID: "node-a", Token: "token-a"}},
				ServerCertValidityHours: 24,
				ClientCertValidityHours: 24,
			},
		},
		Database: DatabaseConfig{
			URL: "postgresql://platform@cockroach.example.test:26257/defaultdb?sslmode=verify-full",
		},
		StateDir: "/var/lib/ebpf-wg-mesh/controlplane",
		SourceArchives: SourceArchiveConfig{
			Directory: "/var/lib/ebpf-wg-mesh/controlplane/source-archives",
		},
		Ingress: IngressConfig{
			PublicAddr: "platform.example.test",
		},
		Registry: RegistryConfig{
			Host:            "registry.example.test",
			SigningCertFile: "/etc/ebpf-wg-mesh/registry-auth.crt",
			SigningKeyFile:  "/etc/ebpf-wg-mesh/registry-auth.key",
		},
	}
}

func validMinimalProductionAgent() AgentConfig {
	return AgentConfig{
		Profile: ProfileProduction,
		Health:  HealthConfig{Listen: "127.0.0.1:18081"},
		Node: NodeConfig{
			ID:            "node-1",
			Name:          "node-1",
			AdvertiseAddr: "2001:db8::10",
		},
		ControlPlane: ControlPlaneClientConfig{
			Addresses: []string{"controlplane.example.test:9443"},
			TLS: ClientTLSConfig{
				CAFile:         "/etc/ebpf-wg-mesh/ca.crt",
				ServerName:     "controlplane.example.test",
				BootstrapToken: "token-a",
			},
		},
		Mesh: MeshConfig{Host: HostConfig{IPv6: "2001:db8::10"}},
	}
}

func validMinimalProductionBuilder() BuilderConfig {
	return BuilderConfig{
		Profile: ProfileProduction,
		Health:  HealthConfig{Listen: "127.0.0.1:18082"},
		ID:      "builder-1",
		Name:    "builder-1",
		ControlPlane: BuilderControlPlaneConfig{
			Address: "controlplane.example.test:9443",
			TLS: InternalClientTLSConfig{
				CAFile:     "/etc/ebpf-wg-mesh/ca.crt",
				CertFile:   "/etc/ebpf-wg-mesh/builder.crt",
				KeyFile:    "/etc/ebpf-wg-mesh/builder.key",
				ServerName: "controlplane.example.test",
			},
		},
		WorkDir: "/var/lib/ebpf-wg-mesh/builder",
	}
}

func mustReject(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("expected error containing %q, got %v", want, err)
	}
}
