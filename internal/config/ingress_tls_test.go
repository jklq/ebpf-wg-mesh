package config

import (
	"strings"
	"testing"
)

func TestFinalizeControlPlaneDefaultsIngressListeners(t *testing.T) {
	t.Parallel()

	cfg := validControlPlaneConfigForRegistryTest()
	if err := FinalizeControlPlane(&cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Ingress.HTTPListenAddrs, ",") != ":80" || strings.Join(cfg.Ingress.HTTPSListenAddrs, ",") != ":443" {
		t.Fatalf("listeners = %v %v, want :80 and :443", cfg.Ingress.HTTPListenAddrs, cfg.Ingress.HTTPSListenAddrs)
	}
	if cfg.Ingress.TLS.ACME.DirectoryURL != "" {
		t.Fatalf("development must not default to a public CA, got %q", cfg.Ingress.TLS.ACME.DirectoryURL)
	}
}

func TestFinalizeProductionDefaultsToLetsEncrypt(t *testing.T) {
	t.Parallel()

	cfg := validMinimalProductionControlPlane(t)
	if err := FinalizeControlPlane(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Ingress.TLS.ACME.DirectoryURL != LetsEncryptDirectoryURL {
		t.Fatalf("directory = %q, want Let's Encrypt", cfg.Ingress.TLS.ACME.DirectoryURL)
	}
}

func TestFinalizeControlPlaneValidatesIngressTLS(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		mutate func(*ControlPlaneConfig)
		want   string
	}{
		{"plain http directory", func(cfg *ControlPlaneConfig) {
			cfg.Ingress.TLS.ACME.DirectoryURL = "http://acme.example.com/directory"
		}, "directoryUrl"},
		{"shared listener", func(cfg *ControlPlaneConfig) {
			cfg.Ingress.HTTPListenAddrs = []string{":8080"}
			cfg.Ingress.HTTPSListenAddrs = []string{":8080"}
		}, "both HTTP and HTTPS"},
		{"bad listener", func(cfg *ControlPlaneConfig) {
			cfg.Ingress.HTTPSListenAddrs = []string{"443"}
		}, "httpsListenAddrs"},
		{"half EAB", func(cfg *ControlPlaneConfig) {
			cfg.Ingress.TLS.ACME.EABKeyID = "kid"
		}, "set together"},
		{"bad EAB key", func(cfg *ControlPlaneConfig) {
			cfg.Ingress.TLS.ACME.EABKeyID = "kid"
			cfg.Ingress.TLS.ACME.EABHMACKey = "not base64!"
		}, "base64url"},
		{"bad email", func(cfg *ControlPlaneConfig) {
			cfg.Ingress.TLS.ACME.Email = "not-an-email"
		}, "email"},
		{"half platform certificate", func(cfg *ControlPlaneConfig) {
			cfg.Ingress.TLS.PlatformCertFile = "/etc/tls/platform.crt"
		}, "set together"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := validControlPlaneConfigForRegistryTest()
			tc.mutate(&cfg)
			if err := FinalizeControlPlane(&cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("FinalizeControlPlane = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestProductionRequiresPlatformWildcardFiles(t *testing.T) {
	t.Parallel()
	cfg := validMinimalProductionControlPlane(t)
	cfg.Ingress.TLS.PlatformCertFile, cfg.Ingress.TLS.PlatformKeyFile = "", ""
	if err := FinalizeControlPlane(&cfg); err == nil || !strings.Contains(err.Error(), "platformCertFile") {
		t.Fatalf("missing platform wildcard = %v", err)
	}
}
