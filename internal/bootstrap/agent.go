package bootstrap

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/mesh"
	"ebof-wg-mesh/internal/meshlabels"
)

func Agent(args []string) (config.AgentConfig, error) {
	var cfg config.AgentConfig
	var profile string
	var underlayInterface string
	var advertiseAddr string

	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	stringFlag(fs, &profile, "profile", "AGENT_PROFILE", "", "development or production; empty defaults to production")
	stringFlag(fs, &cfg.Health.Listen, "health-listen", "AGENT_HEALTH_LISTEN", "", "liveness and readiness listen address")
	stringFlag(fs, &cfg.Node.ID, "node-id", "AGENT_NODE_ID", "", "")
	stringFlag(fs, &cfg.Node.Name, "node-name", "AGENT_NODE_NAME", "", "")
	stringFlag(fs, &advertiseAddr, "advertise-addr", "AGENT_ADVERTISE_ADDR", "", "")
	stringFlag(fs, &underlayInterface, "underlay-interface", "AGENT_UNDERLAY_INTERFACE", "", "")
	int64Flag(fs, &cfg.Node.Resources.CPUMillis, "cpu-millis", "AGENT_CPU_MILLIS", detectedCPUMillis(), "")
	int64Flag(fs, &cfg.Node.Resources.MemoryMebibytes, "memory-mebibytes", "AGENT_MEMORY_MEBIBYTES", detectedMemoryMebibytes(), "")
	stringFlag(fs, &cfg.ControlPlane.Address, "controlplane-address", "AGENT_CONTROLPLANE_ADDRESS", "", "")
	stringFlag(fs, &cfg.ControlPlane.TLS.CAFile, "ca-file", "AGENT_CA_FILE", "", "")
	stringFlag(fs, &cfg.ControlPlane.TLS.ServerName, "server-name", "AGENT_SERVER_NAME", "controlplane", "")
	stringFlag(fs, &cfg.ControlPlane.TLS.BootstrapToken, "bootstrap-token", "AGENT_BOOTSTRAP_TOKEN", "", "")
	intFlag(fs, &cfg.ControlPlane.TLS.RenewBeforeMinutes, "tls-renew-before-minutes", "AGENT_TLS_RENEW_BEFORE_MINUTES", 30, "")
	stringFlag(fs, &cfg.Runtime.DataDir, "data-dir", "AGENT_DATA_DIR", "var/agent", "")
	stringFlag(fs, &cfg.Runtime.VolumesDir, "volumes-dir", "AGENT_VOLUMES_DIR", "", "")
	stringFlag(fs, &cfg.Runtime.ManagedDashboardSecretsDir, "managed-dashboard-secrets-dir", "AGENT_MANAGED_DASHBOARD_SECRETS_DIR", "", "")
	stringFlag(fs, &cfg.Runtime.Snapshotter, "snapshotter", "AGENT_SNAPSHOTTER", "native", "")
	boolFlag(fs, &cfg.Runtime.DisableCgroups, "disable-cgroups", "AGENT_DISABLE_CGROUPS", false, "")
	stringFlag(fs, &cfg.Containerd.Socket, "containerd-socket", "AGENT_CONTAINERD_SOCKET", "/run/containerd/containerd.sock", "")
	stringFlag(fs, &cfg.Containerd.Namespace, "containerd-namespace", "AGENT_CONTAINERD_NAMESPACE", "default", "")
	stringFlag(fs, &cfg.Containerd.EnvironmentLabel, "containerd-environment-label", "AGENT_CONTAINERD_ENVIRONMENT_LABEL", meshlabels.DefaultEnvironmentKey, "")
	stringFlag(fs, &cfg.Containerd.IPv6Label, "containerd-ipv6-label", "AGENT_CONTAINERD_IPV6_LABEL", meshlabels.DefaultIPv6Key, "")
	stringFlag(fs, &cfg.Containerd.IPv4Label, "containerd-ipv4-label", "AGENT_CONTAINERD_IPV4_LABEL", meshlabels.DefaultIPv4Key, "")
	stringFlag(fs, &cfg.Mesh.WireGuard.InterfaceName, "mesh-interface-name", "AGENT_MESH_INTERFACE_NAME", "wg0", "")
	intFlag(fs, &cfg.Mesh.WireGuard.ListenPort, "mesh-listen-port", "AGENT_MESH_LISTEN_PORT", 51820, "")
	intFlag(fs, &cfg.Mesh.Firewall.ConntrackInnerEntries, "firewall-conntrack-inner-entries", "AGENT_FIREWALL_CONNTRACK_INNER_ENTRIES", 10000, "")
	intFlag(fs, &cfg.Mesh.Firewall.MaxContainers, "firewall-max-containers", "AGENT_FIREWALL_MAX_CONTAINERS", 1024, "")
	intFlag(fs, &cfg.Mesh.Firewall.ClusterIdentityEntries, "firewall-cluster-identity-entries", "AGENT_FIREWALL_CLUSTER_IDENTITY_ENTRIES", 65536, "")

	if err := fs.Parse(args); err != nil {
		return config.AgentConfig{}, err
	}
	normalized, err := config.NormalizeProfile(profile)
	if err != nil {
		return config.AgentConfig{}, err
	}
	cfg.Profile = normalized

	hostName, err := os.Hostname()
	if err != nil {
		return config.AgentConfig{}, fmt.Errorf("read hostname: %w", err)
	}
	if cfg.Node.ID == "" {
		cfg.Node.ID = hostName
	}
	if cfg.Node.Name == "" {
		cfg.Node.Name = cfg.Node.ID
	}
	if advertiseAddr == "" {
		advertiseAddr, err = discoverAdvertiseAddr(underlayInterface)
		if err != nil {
			return config.AgentConfig{}, err
		}
	}
	cfg.Node.AdvertiseAddr = advertiseAddr
	cfg.Mesh.Host.IPv6 = advertiseAddr

	if err := os.MkdirAll(cfg.Runtime.DataDir, 0o755); err != nil {
		return config.AgentConfig{}, fmt.Errorf("mkdir data dir: %w", err)
	}
	privateKey, err := loadOrCreateWireGuardKey(filepath.Join(cfg.Runtime.DataDir, "wireguard.key"))
	if err != nil {
		return config.AgentConfig{}, err
	}
	cfg.Mesh.WireGuard.PrivateKey = privateKey

	if err := config.FinalizeAgent(&cfg); err != nil {
		return config.AgentConfig{}, fmt.Errorf("bootstrap agent: %w", err)
	}
	return cfg, nil
}

func discoverAdvertiseAddr(ifaceName string) (string, error) {
	if ifaceName != "" {
		iface, err := net.InterfaceByName(ifaceName)
		if err != nil {
			return "", fmt.Errorf("lookup underlay interface %s: %w", ifaceName, err)
		}
		if addr := firstGlobalIPv6(iface); addr != "" {
			return addr, nil
		}
		return "", fmt.Errorf("interface %s has no global IPv6 address", ifaceName)
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("list interfaces: %w", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		if addr := firstGlobalIPv6(&iface); addr != "" {
			return addr, nil
		}
	}
	return "", errors.New("no non-loopback global IPv6 address found")
}

func firstGlobalIPv6(iface *net.Interface) string {
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		prefix, ok := addr.(*net.IPNet)
		if !ok || prefix.IP == nil {
			continue
		}
		ip := prefix.IP
		if ip.To4() != nil || ip.To16() == nil {
			continue
		}
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
			continue
		}
		return ip.String()
	}
	return ""
}

func detectedCPUMillis() int64 {
	return int64(runtime.NumCPU()) * 1000
}

func detectedMemoryMebibytes() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 4096
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kib, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			break
		}
		if mib := kib / 1024; mib > 0 {
			return mib
		}
	}
	return 4096
}

func loadOrCreateWireGuardKey(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(data)), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read wireguard key %s: %w", path, err)
	}
	key, err := mesh.GeneratePrivateKey()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write wireguard key %s: %w", path, err)
	}
	return key, nil
}
