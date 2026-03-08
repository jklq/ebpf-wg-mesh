package config

import "testing"

func TestApplyDefaultsSetsCoreValues(t *testing.T) {
	cfg := Config{}
	applyDefaults(&cfg)

	if cfg.WireGuard.InterfaceName != "wg0" {
		t.Fatalf("default interface mismatch: %q", cfg.WireGuard.InterfaceName)
	}
	if cfg.Containerd.Socket != "/run/containerd/containerd.sock" {
		t.Fatalf("default containerd socket mismatch: %q", cfg.Containerd.Socket)
	}
	if cfg.Containerd.ProjectLabel != "mesh.project_id" {
		t.Fatalf("default project label mismatch: %q", cfg.Containerd.ProjectLabel)
	}
	if cfg.Containerd.IPv6Label != "mesh.ipv6" {
		t.Fatalf("default ipv6 label mismatch: %q", cfg.Containerd.IPv6Label)
	}
}

func TestValidateAcceptsMinimalRuntimeConfig(t *testing.T) {
	cfg := Config{
		NodeName: "n1",
		Host: HostConfig{
			IPv4: "10.0.0.1",
		},
		Containerd: ContainerdConfig{
			Socket:       "/run/containerd/containerd.sock",
			Namespace:    "default",
			ProjectLabel: "mesh.project_id",
			IPv6Label:    "mesh.ipv6",
		},
		WireGuard: WireGuard{
			PrivateKey: "k",
			ListenPort: 51820,
			Addresses:  []string{"10.0.0.1/24"},
			Peers: []PeerConfig{
				{
					Name:       "p1",
					PublicKey:  "pk",
					Endpoint:   "127.0.0.1:51820",
					AllowedIPs: []string{"10.0.0.2/32"},
				},
			},
		},
	}
	applyDefaults(&cfg)
	if err := validate(cfg); err != nil {
		t.Fatalf("expected config to validate, got %v", err)
	}
}
