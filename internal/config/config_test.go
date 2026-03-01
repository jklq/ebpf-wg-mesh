package config

import "testing"

func TestApplyDefaultsSyncWindow(t *testing.T) {
	cfg := Config{}
	applyDefaults(&cfg)
	if cfg.Sync.ReplayWindowSeconds <= 0 {
		t.Fatalf("expected replay window default > 0")
	}
}

func TestValidateSyncRequiresAuthKey(t *testing.T) {
	cfg := Config{
		NodeName: "n1",
		Host: HostConfig{
			IPv4: "10.0.0.1",
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
		Sync: SyncConfig{
			Enabled: true,
			Listen:  "0.0.0.0:7001",
			Peers:   []string{"127.0.0.1:7002"},
		},
	}
	applyDefaults(&cfg)
	if err := validate(cfg); err == nil {
		t.Fatalf("expected sync auth key validation error")
	}

	cfg.Sync.AuthKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
	if err := validate(cfg); err != nil {
		t.Fatalf("expected config to validate with auth key, got %v", err)
	}
}
