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
}

func TestLoadLocalStackConfigRejectsRemovedRuntimeOverride(t *testing.T) {
	t.Parallel()

	_, err := loadLocalStackConfig(func(key string) string {
		if key == "LOCALTESTSTACK_RUNTIME" {
			return "synthetic"
		}
		return ""
	})
	if err == nil {
		t.Fatal("expected removed runtime override to fail")
	}
}
