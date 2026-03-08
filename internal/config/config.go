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
	NodeName   string           `yaml:"nodeName"`
	Host       HostConfig       `yaml:"host"`
	Containerd ContainerdConfig `yaml:"containerd"`
	WireGuard  WireGuard        `yaml:"wireguard"`
	Firewall   FirewallConfig   `yaml:"firewall"`
}

type HostConfig struct {
	IPv4 string `yaml:"ipv4"`
}

type ContainerdConfig struct {
	Socket            string                `yaml:"socket"`
	Namespace         string                `yaml:"namespace"`
	ProjectLabel      string                `yaml:"projectLabel"`
	IPv6Label         string                `yaml:"ipv6Label"`
	IdentitySeeds     []IdentitySeed        `yaml:"identitySeeds"`
	StaticAssignments []ContainerAssignment `yaml:"staticAssignments"`
}

type IdentitySeed struct {
	IPv6      string `yaml:"ipv6" json:"ipv6"`
	HostIPv4  string `yaml:"hostIPv4" json:"hostIPv4"`
	ProjectID uint32 `yaml:"projectID" json:"projectID"`
}

type ContainerAssignment struct {
	ContainerID string `yaml:"containerID" json:"containerID"`
	ProjectID   uint32 `yaml:"projectID" json:"projectID"`
	IPv6        string `yaml:"ipv6" json:"ipv6"`
}

type WireGuard struct {
	InterfaceName string       `yaml:"interfaceName"`
	PrivateKey    string       `yaml:"privateKey"`
	ListenPort    int          `yaml:"listenPort"`
	Addresses     []string     `yaml:"addresses"`
	Peers         []PeerConfig `yaml:"peers"`
}

type PeerConfig struct {
	Name                 string   `yaml:"name" json:"name"`
	PublicKey            string   `yaml:"publicKey" json:"publicKey"`
	Endpoint             string   `yaml:"endpoint" json:"endpoint"`
	AllowedIPs           []string `yaml:"allowedIPs" json:"allowedIPs"`
	PersistentKeepaliveS int      `yaml:"persistentKeepaliveSeconds" json:"persistentKeepaliveSeconds"`
	TrustCIDRs           []string `yaml:"trustCIDRs" json:"trustCIDRs"`
}

type FirewallConfig struct {
	ConntrackEntries       int `yaml:"conntrackEntries"`
	ConntrackInnerEntries  int `yaml:"conntrackInnerEntries"`
	TrustEntries           int `yaml:"trustEntries"`
	MaxContainers          int `yaml:"maxContainers"`
	ClusterIdentityEntries int `yaml:"clusterIdentityEntries"`
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
		Host: HostConfig{
			IPv4: strings.TrimSpace(os.Getenv("RIG_HOST_IPV4")),
		},
		Containerd: ContainerdConfig{
			Socket:       strings.TrimSpace(os.Getenv("RIG_CONTAINERD_SOCKET")),
			Namespace:    strings.TrimSpace(os.Getenv("RIG_CONTAINERD_NAMESPACE")),
			ProjectLabel: strings.TrimSpace(os.Getenv("RIG_CONTAINERD_PROJECT_LABEL")),
			IPv6Label:    strings.TrimSpace(os.Getenv("RIG_CONTAINERD_IPV6_LABEL")),
		},
		WireGuard: WireGuard{
			InterfaceName: strings.TrimSpace(os.Getenv("RIG_WG_IFACE")),
			PrivateKey:    strings.TrimSpace(os.Getenv("RIG_WG_PRIVATE_KEY")),
			Addresses:     splitCSV(os.Getenv("RIG_WG_ADDRESSES")),
		},
	}

	if raw := strings.TrimSpace(os.Getenv("RIG_CONTAINERD_ASSIGNMENTS_JSON")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.Containerd.StaticAssignments); err != nil {
			return Config{}, fmt.Errorf("parse RIG_CONTAINERD_ASSIGNMENTS_JSON: %w", err)
		}
	}
	if raw := strings.TrimSpace(os.Getenv("RIG_CONTAINERD_IDENTITY_SEEDS_JSON")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.Containerd.IdentitySeeds); err != nil {
			return Config{}, fmt.Errorf("parse RIG_CONTAINERD_IDENTITY_SEEDS_JSON: %w", err)
		}
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
	if cfg.Containerd.Socket == "" {
		cfg.Containerd.Socket = "/run/containerd/containerd.sock"
	}
	if cfg.Containerd.Namespace == "" {
		cfg.Containerd.Namespace = "default"
	}
	if cfg.Containerd.ProjectLabel == "" {
		cfg.Containerd.ProjectLabel = "mesh.project_id"
	}
	if cfg.Containerd.IPv6Label == "" {
		cfg.Containerd.IPv6Label = "mesh.ipv6"
	}
	if cfg.Firewall.ConntrackEntries <= 0 {
		cfg.Firewall.ConntrackEntries = 131072
	}
	if cfg.Firewall.ConntrackInnerEntries <= 0 {
		cfg.Firewall.ConntrackInnerEntries = 10000
	}
	if cfg.Firewall.TrustEntries <= 0 {
		cfg.Firewall.TrustEntries = 8192
	}
	if cfg.Firewall.MaxContainers <= 0 {
		cfg.Firewall.MaxContainers = 1024
	}
	if cfg.Firewall.ClusterIdentityEntries <= 0 {
		cfg.Firewall.ClusterIdentityEntries = 65536
	}
}

func validate(cfg Config) error {
	if cfg.NodeName == "" {
		return errors.New("nodeName is required")
	}
	if cfg.Host.IPv4 == "" {
		return errors.New("host.ipv4 is required")
	}
	hostIP := net.ParseIP(cfg.Host.IPv4)
	if hostIP == nil || hostIP.To4() == nil {
		return fmt.Errorf("host.ipv4 must be valid IPv4: %q", cfg.Host.IPv4)
	}
	if cfg.Containerd.Socket == "" {
		return errors.New("containerd.socket is required")
	}
	if cfg.Containerd.Namespace == "" {
		return errors.New("containerd.namespace is required")
	}
	if cfg.Containerd.ProjectLabel == "" {
		return errors.New("containerd.projectLabel is required")
	}
	if cfg.Containerd.IPv6Label == "" {
		return errors.New("containerd.ipv6Label is required")
	}
	for _, assignment := range cfg.Containerd.StaticAssignments {
		if assignment.ContainerID == "" {
			return errors.New("containerd.staticAssignments.containerID is required")
		}
		if assignment.ProjectID == 0 {
			return fmt.Errorf("containerd.staticAssignments for %q requires non-zero projectID", assignment.ContainerID)
		}
		ip := net.ParseIP(assignment.IPv6)
		if ip == nil || ip.To16() == nil || ip.To4() != nil {
			return fmt.Errorf("containerd.staticAssignments for %q has invalid ipv6 %q", assignment.ContainerID, assignment.IPv6)
		}
	}
	for _, seed := range cfg.Containerd.IdentitySeeds {
		if seed.ProjectID == 0 {
			return errors.New("containerd.identitySeeds.projectID must be non-zero")
		}
		ip := net.ParseIP(seed.IPv6)
		if ip == nil || ip.To16() == nil || ip.To4() != nil {
			return fmt.Errorf("containerd.identitySeeds has invalid ipv6 %q", seed.IPv6)
		}
		hostIP := net.ParseIP(seed.HostIPv4)
		if hostIP == nil || hostIP.To4() == nil {
			return fmt.Errorf("containerd.identitySeeds has invalid hostIPv4 %q", seed.HostIPv4)
		}
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
