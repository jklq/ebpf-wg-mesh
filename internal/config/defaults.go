package config

func applyControlPlaneDefaults(cfg *ControlPlaneConfig) {
	if cfg.PublicHTTP.Listen == "" {
		cfg.PublicHTTP.Listen = "0.0.0.0:8080"
	}
	if cfg.InternalGRPC.Listen == "" {
		cfg.InternalGRPC.Listen = "0.0.0.0:9443"
	}
	if len(cfg.InternalGRPC.TLS.ServerNames) == 0 {
		cfg.InternalGRPC.TLS.ServerNames = []string{"controlplane", "controlplane-internal", "localhost"}
	}
	if cfg.InternalGRPC.TLS.ServerCertValidityHours <= 0 {
		cfg.InternalGRPC.TLS.ServerCertValidityHours = 24 * 30
	}
	if cfg.InternalGRPC.TLS.ClientCertValidityHours <= 0 {
		cfg.InternalGRPC.TLS.ClientCertValidityHours = 24
	}
	if cfg.StateDir == "" {
		cfg.StateDir = "var/controlplane"
	}
	if cfg.Ingress.AdminURL == "" {
		cfg.Ingress.AdminURL = "http://127.0.0.1:2019/load"
	}
	if cfg.Ingress.PublicAddr == "" {
		cfg.Ingress.PublicAddr = "platform.local"
	}
	if cfg.Ingress.ControlPlaneHTTPUpstream == "" {
		cfg.Ingress.ControlPlaneHTTPUpstream = "127.0.0.1:8080"
	}
	if cfg.Mesh.InterfaceName == "" {
		cfg.Mesh.InterfaceName = "wg0"
	}
	if cfg.Mesh.ListenPort <= 0 {
		cfg.Mesh.ListenPort = 51820
	}
	if cfg.Mesh.NetworkCIDR == "" {
		cfg.Mesh.NetworkCIDR = "fd00:44::/64"
	}
	if cfg.Mesh.WorkloadPoolCIDR == "" {
		cfg.Mesh.WorkloadPoolCIDR = "fd00:200::/48"
	}
	if cfg.Mesh.PersistentKeepaliveSeconds <= 0 {
		cfg.Mesh.PersistentKeepaliveSeconds = 5
	}
}

func applyAgentDefaults(cfg *AgentConfig) {
	if cfg.Node.Resources.CPUMillis <= 0 {
		cfg.Node.Resources.CPUMillis = 2000
	}
	if cfg.Node.Resources.MemoryMebibytes <= 0 {
		cfg.Node.Resources.MemoryMebibytes = 4096
	}
	if cfg.ControlPlane.TLS.ServerName == "" {
		cfg.ControlPlane.TLS.ServerName = "controlplane"
	}
	if cfg.ControlPlane.TLS.RenewBeforeMinutes <= 0 {
		cfg.ControlPlane.TLS.RenewBeforeMinutes = 30
	}
	if cfg.Runtime.DataDir == "" {
		cfg.Runtime.DataDir = "var/agent"
	}
	if cfg.Runtime.VolumesDir == "" {
		cfg.Runtime.VolumesDir = cfg.Runtime.DataDir + "/volumes"
	}
	if cfg.Runtime.Snapshotter == "" {
		cfg.Runtime.Snapshotter = "native"
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
	if cfg.Mesh.WireGuard.InterfaceName == "" {
		cfg.Mesh.WireGuard.InterfaceName = "wg0"
	}
	if cfg.Mesh.WireGuard.ListenPort <= 0 {
		cfg.Mesh.WireGuard.ListenPort = 51820
	}
	if cfg.Mesh.Firewall.ConntrackInnerEntries <= 0 {
		cfg.Mesh.Firewall.ConntrackInnerEntries = 10000
	}
	if cfg.Mesh.Firewall.MaxContainers <= 0 {
		cfg.Mesh.Firewall.MaxContainers = 1024
	}
	if cfg.Mesh.Firewall.ClusterIdentityEntries <= 0 {
		cfg.Mesh.Firewall.ClusterIdentityEntries = 65536
	}
}

func FinalizeControlPlane(cfg *ControlPlaneConfig) error {
	applyControlPlaneDefaults(cfg)
	return validateControlPlane(*cfg)
}

func FinalizeAgent(cfg *AgentConfig) error {
	applyAgentDefaults(cfg)
	return validateAgent(*cfg)
}
