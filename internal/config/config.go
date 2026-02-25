package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	NodeName  string         `yaml:"nodeName"`
	WireGuard WireGuard      `yaml:"wireguard"`
	Firewall  FirewallConfig `yaml:"firewall"`
	Sync      SyncConfig     `yaml:"sync"`
}

type WireGuard struct {
	InterfaceName string       `yaml:"interfaceName"`
	PrivateKey    string       `yaml:"privateKey"`
	ListenPort    int          `yaml:"listenPort"`
	Addresses     []string     `yaml:"addresses"`
	Peers         []PeerConfig `yaml:"peers"`
}

type PeerConfig struct {
	Name                 string   `yaml:"name"`
	PublicKey            string   `yaml:"publicKey"`
	Endpoint             string   `yaml:"endpoint"`
	AllowedIPs           []string `yaml:"allowedIPs"`
	PersistentKeepaliveS int      `yaml:"persistentKeepaliveSeconds"`
	TrustCIDRs           []string `yaml:"trustCIDRs"`
}

type FirewallConfig struct {
	ConntrackEntries int `yaml:"conntrackEntries"`
	TrustEntries     int `yaml:"trustEntries"`
}

type SyncConfig struct {
	Enabled             bool     `yaml:"enabled"`
	Listen              string   `yaml:"listen"`
	Peers               []string `yaml:"peers"`
	AuthKey             string   `yaml:"authKey"`
	ReplayWindowSeconds int      `yaml:"replayWindowSeconds"`
}

func Load(path string) (Config, error) {
	if path != "" {
		return loadFile(path)
	}
	if fromEnv := strings.TrimSpace(os.Getenv("RIG_CONFIG_FILE")); fromEnv != "" {
		return loadFile(fromEnv)
	}
	return loadEnv()
}

func loadFile(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse yaml %s: %w", path, err)
	}
	applyDefaults(&cfg)
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func loadEnv() (Config, error) {
	cfg := Config{
		NodeName: strings.TrimSpace(os.Getenv("RIG_NODE_NAME")),
		WireGuard: WireGuard{
			InterfaceName: strings.TrimSpace(os.Getenv("RIG_WG_IFACE")),
			PrivateKey:    strings.TrimSpace(os.Getenv("RIG_WG_PRIVATE_KEY")),
			Addresses:     splitCSV(os.Getenv("RIG_WG_ADDRESSES")),
		},
		Sync: SyncConfig{
			Enabled: strings.EqualFold(strings.TrimSpace(os.Getenv("RIG_SYNC_ENABLED")), "true"),
			Listen:  strings.TrimSpace(os.Getenv("RIG_SYNC_LISTEN")),
			Peers:   splitCSV(os.Getenv("RIG_SYNC_PEERS")),
			AuthKey: strings.TrimSpace(os.Getenv("RIG_SYNC_AUTH_KEY")),
		},
	}
	if w := strings.TrimSpace(os.Getenv("RIG_SYNC_REPLAY_WINDOW_SECONDS")); w != "" {
		v, err := strconv.Atoi(w)
		if err != nil {
			return Config{}, fmt.Errorf("parse RIG_SYNC_REPLAY_WINDOW_SECONDS: %w", err)
		}
		cfg.Sync.ReplayWindowSeconds = v
	}
	if p := strings.TrimSpace(os.Getenv("RIG_WG_PORT")); p != "" {
		v, err := strconv.Atoi(p)
		if err != nil {
			return Config{}, fmt.Errorf("parse RIG_WG_PORT: %w", err)
		}
		cfg.WireGuard.ListenPort = v
	}
	if rawPeers := strings.TrimSpace(os.Getenv("RIG_WG_PEERS_JSON")); rawPeers != "" {
		if err := json.Unmarshal([]byte(rawPeers), &cfg.WireGuard.Peers); err != nil {
			return Config{}, fmt.Errorf("parse RIG_WG_PEERS_JSON: %w", err)
		}
	}
	applyDefaults(&cfg)
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.WireGuard.InterfaceName == "" {
		cfg.WireGuard.InterfaceName = "wg0"
	}
	if cfg.Firewall.ConntrackEntries <= 0 {
		cfg.Firewall.ConntrackEntries = 131072
	}
	if cfg.Firewall.TrustEntries <= 0 {
		cfg.Firewall.TrustEntries = 8192
	}
	if cfg.Sync.ReplayWindowSeconds <= 0 {
		cfg.Sync.ReplayWindowSeconds = 120
	}
}

func validate(cfg Config) error {
	if cfg.NodeName == "" {
		return errors.New("nodeName is required")
	}
	if cfg.WireGuard.PrivateKey == "" {
		return errors.New("wireguard.privateKey is required")
	}
	if cfg.WireGuard.ListenPort <= 0 || cfg.WireGuard.ListenPort > 65535 {
		return errors.New("wireguard.listenPort must be 1-65535")
	}
	if len(cfg.WireGuard.Addresses) == 0 {
		return errors.New("wireguard.addresses must contain at least one CIDR")
	}
	for _, addr := range cfg.WireGuard.Addresses {
		if _, _, err := net.ParseCIDR(addr); err != nil {
			return fmt.Errorf("invalid wireguard address %q: %w", addr, err)
		}
	}
	for _, p := range cfg.WireGuard.Peers {
		if p.PublicKey == "" {
			return fmt.Errorf("peer %q missing publicKey", p.Name)
		}
		if p.Endpoint == "" {
			return fmt.Errorf("peer %q missing endpoint", p.Name)
		}
		if _, err := net.ResolveUDPAddr("udp", p.Endpoint); err != nil {
			return fmt.Errorf("peer %q invalid endpoint %q: %w", p.Name, p.Endpoint, err)
		}
		if len(p.AllowedIPs) == 0 {
			return fmt.Errorf("peer %q must include at least one allowedIPs entry", p.Name)
		}
		for _, cidr := range p.AllowedIPs {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return fmt.Errorf("peer %q invalid allowed IP %q: %w", p.Name, cidr, err)
			}
		}
		for _, cidr := range p.TrustCIDRs {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return fmt.Errorf("peer %q invalid trust CIDR %q: %w", p.Name, cidr, err)
			}
		}
	}
	if cfg.Sync.Enabled {
		if cfg.Sync.Listen == "" {
			return errors.New("sync.listen is required when sync is enabled")
		}
		if cfg.Sync.AuthKey == "" {
			return errors.New("sync.authKey is required when sync is enabled")
		}
		if _, err := net.ResolveUDPAddr("udp", cfg.Sync.Listen); err != nil {
			return fmt.Errorf("sync.listen invalid: %w", err)
		}
		for _, peer := range cfg.Sync.Peers {
			if _, err := net.ResolveUDPAddr("udp", peer); err != nil {
				return fmt.Errorf("sync peer %q invalid: %w", peer, err)
			}
		}
	}
	return nil
}

func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
