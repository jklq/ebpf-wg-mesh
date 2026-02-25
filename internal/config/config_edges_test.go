package config

import (
	"os"
	"strings"
	"testing"
)

func validConfigForTests() Config {
	return Config{
		NodeName: "node-a",
		WireGuard: WireGuard{
			InterfaceName: "wg0",
			PrivateKey:    "private-key",
			ListenPort:    51820,
			Addresses:     []string{"10.0.0.1/24"},
			Peers: []PeerConfig{
				{
					Name:       "node-b",
					PublicKey:  "public-key",
					Endpoint:   "127.0.0.1:51820",
					AllowedIPs: []string{"10.0.0.2/32"},
					TrustCIDRs: []string{"192.168.0.0/16"},
				},
			},
		},
		Firewall: FirewallConfig{
			ConntrackEntries: 1024,
			TrustEntries:     128,
		},
		Sync: SyncConfig{
			Enabled:             true,
			Listen:              "0.0.0.0:7001",
			Peers:               []string{"127.0.0.1:7002"},
			AuthKey:             "some-auth-key",
			ReplayWindowSeconds: 60,
		},
	}
}

func setValidConfigEnv(t *testing.T) {
	t.Helper()
	t.Setenv("RIG_CONFIG_FILE", "")
	t.Setenv("RIG_NODE_NAME", "node-env")
	t.Setenv("RIG_WG_IFACE", "")
	t.Setenv("RIG_WG_PRIVATE_KEY", "private-key")
	t.Setenv("RIG_WG_PORT", "51820")
	t.Setenv("RIG_WG_ADDRESSES", "10.10.0.1/24, 10.11.0.1/24")
	t.Setenv("RIG_WG_PEERS_JSON", `[{"name":"node-b","publicKey":"public-key","endpoint":"127.0.0.1:51820","allowedIPs":["10.10.0.2/32"],"trustCIDRs":["192.168.0.0/16"]}]`)
	t.Setenv("RIG_SYNC_ENABLED", "true")
	t.Setenv("RIG_SYNC_LISTEN", "0.0.0.0:7010")
	t.Setenv("RIG_SYNC_PEERS", "127.0.0.1:7011,127.0.0.1:7012")
	t.Setenv("RIG_SYNC_AUTH_KEY", "env-auth-key")
	t.Setenv("RIG_SYNC_REPLAY_WINDOW_SECONDS", "45")
}

func TestApplyDefaultsValues(t *testing.T) {
	cfg := Config{}
	applyDefaults(&cfg)

	if cfg.WireGuard.InterfaceName != "wg0" {
		t.Fatalf("default interface mismatch: %q", cfg.WireGuard.InterfaceName)
	}
	if cfg.Firewall.ConntrackEntries != 131072 {
		t.Fatalf("default conntrack entries mismatch: %d", cfg.Firewall.ConntrackEntries)
	}
	if cfg.Firewall.TrustEntries != 8192 {
		t.Fatalf("default trust entries mismatch: %d", cfg.Firewall.TrustEntries)
	}
	if cfg.Sync.ReplayWindowSeconds != 120 {
		t.Fatalf("default replay window mismatch: %d", cfg.Sync.ReplayWindowSeconds)
	}
}

func TestApplyDefaultsPreservesPositiveValues(t *testing.T) {
	cfg := Config{
		WireGuard: WireGuard{
			InterfaceName: "wg99",
		},
		Firewall: FirewallConfig{
			ConntrackEntries: 2048,
			TrustEntries:     4096,
		},
		Sync: SyncConfig{
			ReplayWindowSeconds: 9,
		},
	}
	applyDefaults(&cfg)

	if cfg.WireGuard.InterfaceName != "wg99" {
		t.Fatalf("expected explicit interface name to be preserved")
	}
	if cfg.Firewall.ConntrackEntries != 2048 {
		t.Fatalf("expected explicit conntrack entries to be preserved")
	}
	if cfg.Firewall.TrustEntries != 4096 {
		t.Fatalf("expected explicit trust entries to be preserved")
	}
	if cfg.Sync.ReplayWindowSeconds != 9 {
		t.Fatalf("expected explicit replay window to be preserved")
	}
}

func TestSplitCSVTrimsAndDropsEmpty(t *testing.T) {
	got := splitCSV(" , a, b ,, c ,\t,\n")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("unexpected split len: got=%d want=%d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("unexpected split item at %d: got=%q want=%q", i, got[i], want[i])
		}
	}

	if out := splitCSV(""); len(out) != 0 {
		t.Fatalf("expected empty output for empty input")
	}
}

func TestValidateRejectsInvalidConfigurations(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{
			name: "missing nodeName",
			edit: func(cfg *Config) { cfg.NodeName = "" },
			want: "nodeName is required",
		},
		{
			name: "missing private key",
			edit: func(cfg *Config) { cfg.WireGuard.PrivateKey = "" },
			want: "wireguard.privateKey is required",
		},
		{
			name: "invalid listen port",
			edit: func(cfg *Config) { cfg.WireGuard.ListenPort = 0 },
			want: "wireguard.listenPort must be 1-65535",
		},
		{
			name: "missing addresses",
			edit: func(cfg *Config) { cfg.WireGuard.Addresses = nil },
			want: "wireguard.addresses must contain at least one CIDR",
		},
		{
			name: "invalid interface CIDR",
			edit: func(cfg *Config) { cfg.WireGuard.Addresses = []string{"not-a-cidr"} },
			want: "invalid wireguard address",
		},
		{
			name: "peer missing public key",
			edit: func(cfg *Config) { cfg.WireGuard.Peers[0].PublicKey = "" },
			want: "missing publicKey",
		},
		{
			name: "peer missing endpoint",
			edit: func(cfg *Config) { cfg.WireGuard.Peers[0].Endpoint = "" },
			want: "missing endpoint",
		},
		{
			name: "peer invalid endpoint",
			edit: func(cfg *Config) { cfg.WireGuard.Peers[0].Endpoint = ":::bad:::" },
			want: "invalid endpoint",
		},
		{
			name: "peer missing allowed IPs",
			edit: func(cfg *Config) { cfg.WireGuard.Peers[0].AllowedIPs = nil },
			want: "must include at least one allowedIPs entry",
		},
		{
			name: "peer invalid allowed IP",
			edit: func(cfg *Config) { cfg.WireGuard.Peers[0].AllowedIPs = []string{"invalid"} },
			want: "invalid allowed IP",
		},
		{
			name: "peer invalid trust CIDR",
			edit: func(cfg *Config) { cfg.WireGuard.Peers[0].TrustCIDRs = []string{"invalid"} },
			want: "invalid trust CIDR",
		},
		{
			name: "sync enabled requires listen",
			edit: func(cfg *Config) { cfg.Sync.Listen = "" },
			want: "sync.listen is required",
		},
		{
			name: "sync enabled requires auth key",
			edit: func(cfg *Config) { cfg.Sync.AuthKey = "" },
			want: "sync.authKey is required",
		},
		{
			name: "sync invalid listen",
			edit: func(cfg *Config) { cfg.Sync.Listen = "invalid-listen-value" },
			want: "sync.listen invalid",
		},
		{
			name: "sync invalid peer",
			edit: func(cfg *Config) { cfg.Sync.Peers = []string{"invalid-peer"} },
			want: "sync peer",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfigForTests()
			tc.edit(&cfg)
			err := validate(cfg)
			if err == nil {
				t.Fatalf("expected validate to fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unexpected error: got=%v want substring=%q", err, tc.want)
			}
		})
	}
}

func TestValidateIgnoresSyncFieldsWhenDisabled(t *testing.T) {
	cfg := validConfigForTests()
	cfg.Sync = SyncConfig{
		Enabled: false,
		Listen:  "not-an-addr",
		Peers:   []string{"bad-peer"},
	}
	if err := validate(cfg); err != nil {
		t.Fatalf("validate should ignore sync listen/peers when disabled: %v", err)
	}
}

func TestLoadEnvHappyPath(t *testing.T) {
	setValidConfigEnv(t)

	cfg, err := loadEnv()
	if err != nil {
		t.Fatalf("loadEnv: %v", err)
	}

	if cfg.NodeName != "node-env" {
		t.Fatalf("unexpected node name: %q", cfg.NodeName)
	}
	if cfg.WireGuard.InterfaceName != "wg0" {
		t.Fatalf("expected default interface name, got %q", cfg.WireGuard.InterfaceName)
	}
	if len(cfg.WireGuard.Addresses) != 2 {
		t.Fatalf("unexpected addresses: %v", cfg.WireGuard.Addresses)
	}
	if len(cfg.WireGuard.Peers) != 1 {
		t.Fatalf("unexpected peers len: %d", len(cfg.WireGuard.Peers))
	}
	if cfg.Sync.ReplayWindowSeconds != 45 {
		t.Fatalf("unexpected sync replay window: %d", cfg.Sync.ReplayWindowSeconds)
	}
	if cfg.Firewall.ConntrackEntries != 131072 || cfg.Firewall.TrustEntries != 8192 {
		t.Fatalf("expected firewall defaults to be applied")
	}
}

func TestLoadEnvParsingErrors(t *testing.T) {
	t.Run("invalid port", func(t *testing.T) {
		setValidConfigEnv(t)
		t.Setenv("RIG_WG_PORT", "not-a-number")
		if _, err := loadEnv(); err == nil || !strings.Contains(err.Error(), "parse RIG_WG_PORT") {
			t.Fatalf("expected RIG_WG_PORT parse error, got %v", err)
		}
	})

	t.Run("invalid replay window", func(t *testing.T) {
		setValidConfigEnv(t)
		t.Setenv("RIG_SYNC_REPLAY_WINDOW_SECONDS", "NaN")
		if _, err := loadEnv(); err == nil || !strings.Contains(err.Error(), "parse RIG_SYNC_REPLAY_WINDOW_SECONDS") {
			t.Fatalf("expected replay window parse error, got %v", err)
		}
	})

	t.Run("invalid peers json", func(t *testing.T) {
		setValidConfigEnv(t)
		t.Setenv("RIG_WG_PEERS_JSON", "{not-json")
		if _, err := loadEnv(); err == nil || !strings.Contains(err.Error(), "parse RIG_WG_PEERS_JSON") {
			t.Fatalf("expected peers json parse error, got %v", err)
		}
	})
}

func TestLoadFileAndEnvPrecedence(t *testing.T) {
	tmp := t.TempDir()

	fileFromEnv := tmp + "/from-env.yaml"
	fileFromArg := tmp + "/from-arg.yaml"
	invalidFile := tmp + "/invalid.yaml"

	yamlTemplate := func(nodeName string) string {
		return strings.TrimSpace(`
nodeName: ` + nodeName + `
wireguard:
  privateKey: private-key
  listenPort: 51820
  addresses: ["10.20.0.1/24"]
  peers:
    - name: node-b
      publicKey: public-key
      endpoint: "127.0.0.1:51820"
      allowedIPs: ["10.20.0.2/32"]
sync:
  enabled: false
`) + "\n"
	}

	if err := os.WriteFile(fileFromEnv, []byte(yamlTemplate("env-node")), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	if err := os.WriteFile(fileFromArg, []byte(yamlTemplate("arg-node")), 0o600); err != nil {
		t.Fatalf("write arg file: %v", err)
	}
	if err := os.WriteFile(invalidFile, []byte("nodeName: ["), 0o600); err != nil {
		t.Fatalf("write invalid file: %v", err)
	}

	t.Setenv("RIG_CONFIG_FILE", fileFromEnv)

	cfg, err := Load(fileFromArg)
	if err != nil {
		t.Fatalf("Load(path): %v", err)
	}
	if cfg.NodeName != "arg-node" {
		t.Fatalf("expected explicit path to win over env config, got %q", cfg.NodeName)
	}

	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load(env): %v", err)
	}
	if cfg.NodeName != "env-node" {
		t.Fatalf("expected env config file to load, got %q", cfg.NodeName)
	}

	if _, err := loadFile(invalidFile); err == nil || !strings.Contains(err.Error(), "parse yaml") {
		t.Fatalf("expected yaml parse error, got %v", err)
	}
}
