package main

import "testing"

func TestLoadLocalStackConfigDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := loadLocalStackConfig(func(string) string { return "" })
	if err != nil {
		t.Fatalf("loadLocalStackConfig: %v", err)
	}
	if cfg.IngressHost != defaultLocalIngressHost {
		t.Fatalf("unexpected ingress host %q", cfg.IngressHost)
	}
	if cfg.IngressPort != defaultLocalIngressPort {
		t.Fatalf("unexpected ingress port %d", cfg.IngressPort)
	}
	if cfg.DockerNetwork != defaultLocalDockerNetwork {
		t.Fatalf("unexpected docker network %q", cfg.DockerNetwork)
	}
	if cfg.LocalDomainSuffix != defaultLocalDomainSuffix {
		t.Fatalf("unexpected local domain suffix %q", cfg.LocalDomainSuffix)
	}
	if cfg.ConsoleBindAddress != "127.0.0.1" {
		t.Fatalf("unexpected console bind address %q", cfg.ConsoleBindAddress)
	}
	if cfg.EnablePublicTunnel {
		t.Fatal("public tunnel must be opt-in")
	}
}

func TestLoadLocalStackConfigRequiresExplicitValidConsoleBind(t *testing.T) {
	t.Parallel()

	cfg, err := loadLocalStackConfig(func(key string) string {
		switch key {
		case "LOCALTESTSTACK_CONSOLE_BIND_ADDRESS":
			return "0.0.0.0"
		case "LOCALTESTSTACK_ENABLE_PUBLIC_TUNNEL":
			return "1"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("loadLocalStackConfig: %v", err)
	}
	if cfg.ConsoleBindAddress != "0.0.0.0" || !cfg.EnablePublicTunnel {
		t.Fatalf("unexpected explicit config: %+v", cfg)
	}
}

func TestLoadLocalStackConfigRejectsPublicTunnelWithLoopbackConsole(t *testing.T) {
	t.Parallel()

	_, err := loadLocalStackConfig(func(key string) string {
		if key == "LOCALTESTSTACK_ENABLE_PUBLIC_TUNNEL" {
			return "true"
		}
		return ""
	})
	if err == nil {
		t.Fatal("expected public tunnel with loopback console to be rejected")
	}
}
