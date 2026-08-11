package config

import (
	"runtime"

	"ebof-wg-mesh/internal/meshlabels"
)

func applyControlPlaneDefaults(cfg *ControlPlaneConfig) {
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
	if cfg.Database.MaxOpenConns <= 0 {
		cfg.Database.MaxOpenConns = maxInt(32, runtime.GOMAXPROCS(0)*8)
	}
	if cfg.Database.MaxIdleConns <= 0 {
		cfg.Database.MaxIdleConns = minInt(cfg.Database.MaxOpenConns, maxInt(16, runtime.GOMAXPROCS(0)*4))
	}
	if cfg.Database.MaxIdleConns > cfg.Database.MaxOpenConns {
		cfg.Database.MaxIdleConns = cfg.Database.MaxOpenConns
	}
	if cfg.Logs.RetentionDays <= 0 {
		cfg.Logs.RetentionDays = 14
	}
	if cfg.Logs.ClickHouse.URL != "" {
		if cfg.Logs.ClickHouse.MaxOpenConns <= 0 {
			cfg.Logs.ClickHouse.MaxOpenConns = maxInt(8, runtime.GOMAXPROCS(0)*2)
		}
		if cfg.Logs.ClickHouse.MaxIdleConns <= 0 {
			cfg.Logs.ClickHouse.MaxIdleConns = minInt(cfg.Logs.ClickHouse.MaxOpenConns, maxInt(4, runtime.GOMAXPROCS(0)))
		}
		if cfg.Logs.ClickHouse.MaxIdleConns > cfg.Logs.ClickHouse.MaxOpenConns {
			cfg.Logs.ClickHouse.MaxIdleConns = cfg.Logs.ClickHouse.MaxOpenConns
		}
	}
	if cfg.Ingress.AdminURL == "" {
		cfg.Ingress.AdminURL = "http://127.0.0.1:2019/load"
	}
	if cfg.Ingress.AdminListen == "" {
		cfg.Ingress.AdminListen = ":2019"
	}
	if len(cfg.Ingress.ListenAddrs) == 0 {
		cfg.Ingress.ListenAddrs = []string{":80", ":443"}
	}
	if cfg.Ingress.PublicAddr == "" {
		cfg.Ingress.PublicAddr = "platform.local"
	}
	if cfg.Ingress.ControlPlaneHTTPUpstream == "" {
		cfg.Ingress.ControlPlaneHTTPUpstream = "127.0.0.1:8080"
	}
	if cfg.Dashboard.ProjectName == "" {
		cfg.Dashboard.ProjectName = "Platform Dashboard"
	}
	if cfg.Dashboard.ProjectSystemKey == "" {
		cfg.Dashboard.ProjectSystemKey = "dashboard"
	}
	if cfg.Dashboard.ServiceName == "" {
		cfg.Dashboard.ServiceName = "dashboard"
	}
	if cfg.Dashboard.ServiceCallerID == "" {
		cfg.Dashboard.ServiceCallerID = "dashboard"
	}
	if cfg.Dashboard.PublicDomain == "" {
		cfg.Dashboard.PublicDomain = cfg.Ingress.PublicAddr
	}
	if cfg.Dashboard.IngressTargetHost == "" {
		cfg.Dashboard.IngressTargetHost = cfg.Ingress.PublicAddr
	}
	if cfg.Dashboard.ControlPlaneAddr == "" {
		cfg.Dashboard.ControlPlaneAddr = "controlplane:9443"
	}
	if cfg.Dashboard.ControlPlaneSNI == "" {
		cfg.Dashboard.ControlPlaneSNI = "controlplane"
	}
	if cfg.Dashboard.ContainerPort <= 0 {
		cfg.Dashboard.ContainerPort = 3000
	}
	if cfg.Dashboard.HealthPath == "" {
		cfg.Dashboard.HealthPath = "/healthz"
	}
	if cfg.Dashboard.CPUMillis <= 0 {
		cfg.Dashboard.CPUMillis = 250
	}
	if cfg.Dashboard.MemoryMebibytes <= 0 {
		cfg.Dashboard.MemoryMebibytes = 256
	}
	if cfg.Dashboard.DatabaseSchema == "" {
		cfg.Dashboard.DatabaseSchema = "dashboard"
	}
	if cfg.Dashboard.SessionCookieName == "" {
		cfg.Dashboard.SessionCookieName = "dashboard_session"
	}
	if cfg.GitHub.APIBaseURL == "" {
		cfg.GitHub.APIBaseURL = "https://api.github.com"
	}
	if cfg.GitHub.WebBaseURL == "" {
		cfg.GitHub.WebBaseURL = "https://github.com"
	}
	if cfg.GitHub.WebhookPath == "" {
		cfg.GitHub.WebhookPath = "/webhooks/github"
	}
	if cfg.Builder.HeartbeatTimeoutSeconds <= 0 {
		cfg.Builder.HeartbeatTimeoutSeconds = 120
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
		cfg.Containerd.ProjectLabel = meshlabels.DefaultProjectKey
	}
	if cfg.Containerd.IPv6Label == "" {
		cfg.Containerd.IPv6Label = meshlabels.DefaultIPv6Key
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

func applyBuilderDefaults(cfg *BuilderConfig) {
	if cfg.Name == "" {
		cfg.Name = cfg.ID
	}
	if cfg.ControlPlane.TLS.ServerName == "" {
		cfg.ControlPlane.TLS.ServerName = "controlplane"
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = "var/builder"
	}
	if cfg.PollIntervalSeconds <= 0 {
		cfg.PollIntervalSeconds = 5
	}
	if cfg.HeartbeatIntervalSeconds <= 0 {
		cfg.HeartbeatIntervalSeconds = 10
	}
	if cfg.GitBinary == "" {
		cfg.GitBinary = "git"
	}
	if cfg.BuildctlBinary == "" {
		cfg.BuildctlBinary = "buildctl"
	}
	if cfg.BuildkitAddress == "" {
		cfg.BuildkitAddress = "unix:///run/buildkit/buildkitd.sock"
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

func FinalizeBuilder(cfg *BuilderConfig) error {
	applyBuilderDefaults(cfg)
	return validateBuilder(*cfg)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
