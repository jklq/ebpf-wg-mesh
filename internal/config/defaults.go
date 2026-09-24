package config

import (
	"net"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

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
	applySourceArchiveDefaults(cfg)
	if cfg.SecretKeys.KeyringPath == "" {
		cfg.SecretKeys.KeyringPath = filepath.Join(cfg.StateDir, "secret-keys", "keys.json")
	}
	if cfg.Deletion.GracePeriodDays <= 0 {
		cfg.Deletion.GracePeriodDays = 7
	}
	if cfg.Deletion.GCIntervalSeconds <= 0 {
		cfg.Deletion.GCIntervalSeconds = 60
	}
	if cfg.InternalGRPC.TLS.RevokedClientCertSerialsFile == "" {
		cfg.InternalGRPC.TLS.RevokedClientCertSerialsFile = filepath.Join(cfg.StateDir, "pki", "revoked-client-cert-serials.txt")
	}
	if cfg.Database.MaxOpenConns <= 0 {
		cfg.Database.MaxOpenConns = max(32, runtime.GOMAXPROCS(0)*8)
	}
	if cfg.Database.MaxIdleConns <= 0 {
		cfg.Database.MaxIdleConns = min(cfg.Database.MaxOpenConns, max(16, runtime.GOMAXPROCS(0)*4))
	}
	if cfg.Database.MaxIdleConns > cfg.Database.MaxOpenConns {
		cfg.Database.MaxIdleConns = cfg.Database.MaxOpenConns
	}
	if cfg.Logs.RetentionDays <= 0 {
		cfg.Logs.RetentionDays = 14
	}
	if cfg.Logs.ClickHouse.URL != "" {
		if cfg.Logs.ClickHouse.MaxOpenConns <= 0 {
			cfg.Logs.ClickHouse.MaxOpenConns = max(8, runtime.GOMAXPROCS(0)*2)
		}
		if cfg.Logs.ClickHouse.MaxIdleConns <= 0 {
			cfg.Logs.ClickHouse.MaxIdleConns = min(cfg.Logs.ClickHouse.MaxOpenConns, max(4, runtime.GOMAXPROCS(0)))
		}
		if cfg.Logs.ClickHouse.MaxIdleConns > cfg.Logs.ClickHouse.MaxOpenConns {
			cfg.Logs.ClickHouse.MaxIdleConns = cfg.Logs.ClickHouse.MaxOpenConns
		}
	}
	if cfg.Ingress.XDSListen == "" {
		cfg.Ingress.XDSListen = "127.0.0.1:18000"
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
	if cfg.GitHub.WebhookPath == "" {
		cfg.GitHub.WebhookPath = "/webhooks/github"
	}
	if cfg.Registry.CredentialTTLSeconds <= 0 {
		cfg.Registry.CredentialTTLSeconds = 300
	}
	if cfg.Registry.PullCredentialTTLSeconds <= 0 {
		cfg.Registry.PullCredentialTTLSeconds = 48 * 3600
	}
	if cfg.Registry.Host != "" {
		if cfg.Registry.AuthListen == "" {
			cfg.Registry.AuthListen = "127.0.0.1:9444"
		}
		if cfg.Registry.TokenIssuer == "" {
			cfg.Registry.TokenIssuer = "ebpf-wg-mesh"
		}
		if cfg.Registry.TokenService == "" {
			cfg.Registry.TokenService = cfg.Registry.Host
		}
	}
	builderDefaults := DefaultControlPlaneBuilderConfig()
	if cfg.Builder.HeartbeatTimeoutSeconds <= 0 {
		cfg.Builder.HeartbeatTimeoutSeconds = builderDefaults.HeartbeatTimeoutSeconds
	}
	if cfg.Builder.LeaseTTLSeconds <= 0 {
		cfg.Builder.LeaseTTLSeconds = builderDefaults.LeaseTTLSeconds
	}
	if cfg.Builder.MaxAttempts <= 0 {
		cfg.Builder.MaxAttempts = builderDefaults.MaxAttempts
	}
	if cfg.Builder.MaxConcurrentGlobal <= 0 {
		cfg.Builder.MaxConcurrentGlobal = builderDefaults.MaxConcurrentGlobal
	}
	if cfg.Builder.MaxConcurrentPerProject <= 0 {
		cfg.Builder.MaxConcurrentPerProject = builderDefaults.MaxConcurrentPerProject
	}
	if cfg.Builder.BuildTimeoutSeconds <= 0 {
		cfg.Builder.BuildTimeoutSeconds = builderDefaults.BuildTimeoutSeconds
	}
	if cfg.Builder.MaxQueueAgeSeconds <= 0 {
		cfg.Builder.MaxQueueAgeSeconds = builderDefaults.MaxQueueAgeSeconds
	}
	if cfg.Failover.ReconcileIntervalSeconds <= 0 {
		cfg.Failover.ReconcileIntervalSeconds = 60
	}
	if cfg.Failover.UnhealthyThresholdSeconds <= 0 {
		cfg.Failover.UnhealthyThresholdSeconds = 30
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
	if cfg.Mesh.WorkloadIPv4PoolCIDR == "" {
		cfg.Mesh.WorkloadIPv4PoolCIDR = "10.200.0.0/16"
	}
	if cfg.Mesh.WorkloadIPv4NodePrefixBits == 0 {
		cfg.Mesh.WorkloadIPv4NodePrefixBits = 24
	}
	if cfg.Mesh.PersistentKeepaliveSeconds <= 0 {
		cfg.Mesh.PersistentKeepaliveSeconds = 5
	}
	cfg.AdvertiseAddr = strings.TrimSpace(cfg.AdvertiseAddr)
	cfg.ReplicaAddresses = uniqueAddresses(append([]string{cfg.AdvertiseAddr}, cfg.ReplicaAddresses...))
	if cfg.AdvertiseAddr == "" && len(cfg.ReplicaAddresses) == 1 {
		cfg.AdvertiseAddr = cfg.ReplicaAddresses[0]
	}
}

func DefaultControlPlaneBuilderConfig() ControlPlaneBuilderConfig {
	return ControlPlaneBuilderConfig{
		HeartbeatTimeoutSeconds: 120, LeaseTTLSeconds: 120, MaxAttempts: 3,
		MaxConcurrentGlobal: 20, MaxConcurrentPerProject: 5,
		BuildTimeoutSeconds: 1800, MaxQueueAgeSeconds: 7200,
	}
}

func applySourceArchiveDefaults(cfg *ControlPlaneConfig) {
	cfg.SourceArchives.Provider = strings.ToLower(strings.TrimSpace(cfg.SourceArchives.Provider))
	if cfg.SourceArchives.Provider == "" {
		if strings.TrimSpace(cfg.SourceArchives.S3.Bucket) != "" ||
			strings.TrimSpace(cfg.SourceArchives.S3.Endpoint) != "" ||
			strings.TrimSpace(cfg.SourceArchives.S3.Region) != "" {
			cfg.SourceArchives.Provider = SourceArchiveProviderS3
		} else {
			cfg.SourceArchives.Provider = SourceArchiveProviderFile
		}
	}
	if cfg.SourceArchives.Provider == SourceArchiveProviderFile && cfg.SourceArchives.Directory == "" {
		cfg.SourceArchives.Directory = filepath.Join(cfg.StateDir, "source-archives")
	}
	if cfg.SourceArchives.RetentionDays <= 0 {
		cfg.SourceArchives.RetentionDays = 30
	}
	if cfg.SourceArchives.Provider == SourceArchiveProviderS3 {
		cfg.SourceArchives.S3.ServerSideEncryption = strings.TrimSpace(cfg.SourceArchives.S3.ServerSideEncryption)
		if cfg.SourceArchives.S3.RequestTimeoutSeconds <= 0 {
			cfg.SourceArchives.S3.RequestTimeoutSeconds = 30
		}
		if cfg.SourceArchives.S3.MaxRetries <= 0 {
			cfg.SourceArchives.S3.MaxRetries = 3
		}
	}
}

func uniqueAddresses(addresses []string) []string {
	result := make([]string, 0, len(addresses))
	seen := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		if _, ok := seen[address]; ok {
			continue
		}
		seen[address] = struct{}{}
		result = append(result, address)
	}
	return result
}

func applyAgentDefaults(cfg *AgentConfig) {
	if cfg.Node.Resources.CPUMillis <= 0 {
		cfg.Node.Resources.CPUMillis = 2000
	}
	if cfg.Node.Resources.MemoryMebibytes <= 0 {
		cfg.Node.Resources.MemoryMebibytes = 4096
	}
	if cfg.Node.Resources.ReservedCPUMillis <= 0 {
		cfg.Node.Resources.ReservedCPUMillis = 500
	}
	if cfg.Node.Resources.ReservedMemoryMebibytes <= 0 {
		cfg.Node.Resources.ReservedMemoryMebibytes = 512
	}
	cfg.ControlPlane.Addresses = uniqueAddresses(cfg.ControlPlane.Addresses)
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
	if cfg.Containerd.EnvironmentLabel == "" {
		cfg.Containerd.EnvironmentLabel = meshlabels.DefaultEnvironmentKey
	}
	if cfg.Containerd.IPv6Label == "" {
		cfg.Containerd.IPv6Label = meshlabels.DefaultIPv6Key
	}
	if cfg.Containerd.IPv4Label == "" {
		cfg.Containerd.IPv4Label = meshlabels.DefaultIPv4Key
	}
	if cfg.Mesh.WireGuard.InterfaceName == "" {
		cfg.Mesh.WireGuard.InterfaceName = "wg0"
	}
	if cfg.Mesh.WireGuard.ListenPort <= 0 {
		cfg.Mesh.WireGuard.ListenPort = 51820
	}
	cfg.Mesh.WireGuard.AdvertiseEndpoint = strings.TrimSpace(cfg.Mesh.WireGuard.AdvertiseEndpoint)
	if cfg.Mesh.WireGuard.AdvertiseEndpoint == "" && cfg.Node.AdvertiseAddr != "" {
		cfg.Mesh.WireGuard.AdvertiseEndpoint = net.JoinHostPort(cfg.Node.AdvertiseAddr, strconv.Itoa(cfg.Mesh.WireGuard.ListenPort))
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
	limits := DefaultBuilderLimits()
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
	if cfg.BuildctlBinary == "" {
		cfg.BuildctlBinary = "buildctl"
	}
	if cfg.BuildkitAddress == "" {
		cfg.BuildkitAddress = "unix:///run/buildkit/buildkitd.sock"
	}
	if cfg.RailpackBinary == "" {
		cfg.RailpackBinary = "railpack"
	}
	if cfg.RailpackFrontendImage == "" {
		cfg.RailpackFrontendImage = "ghcr.io/railwayapp/railpack-frontend:latest"
	}
	if cfg.Executor == "" {
		// Production defaults to the isolating backend; development
		// keeps the host-process executor so unprivileged checkouts
		// can still build.
		if cfg.Profile.IsProduction() {
			cfg.Executor = "hardened"
		} else {
			cfg.Executor = "development"
		}
	}
	applyBuilderSandboxDefaults(&cfg.Sandbox)
	if cfg.Limits.TimeoutSeconds <= 0 {
		cfg.Limits.TimeoutSeconds = limits.TimeoutSeconds
	}
	if cfg.Limits.MemoryBytes <= 0 {
		cfg.Limits.MemoryBytes = limits.MemoryBytes
	}
	if cfg.Limits.CPUSeconds <= 0 {
		cfg.Limits.CPUSeconds = limits.CPUSeconds
	}
	if cfg.Limits.MaxFileBytes <= 0 {
		cfg.Limits.MaxFileBytes = limits.MaxFileBytes
	}
	if cfg.Limits.MaxProcesses <= 0 {
		cfg.Limits.MaxProcesses = limits.MaxProcesses
	}
	if cfg.Limits.MaxWorkspaceBytes <= 0 {
		cfg.Limits.MaxWorkspaceBytes = limits.MaxWorkspaceBytes
	}
	if len(cfg.Network.DeniedCIDRs) == 0 {
		cfg.Network.DeniedCIDRs = DefaultBuilderDeniedCIDRs()
	}
	if cfg.Cache.Mode == "" {
		cfg.Cache.Mode = "none"
	}
}

func DefaultBuilderLimits() BuilderLimitsConfig {
	return BuilderLimitsConfig{
		TimeoutSeconds: 1800, MemoryBytes: 8 << 30, CPUSeconds: 3600,
		MaxFileBytes: 10 << 30, MaxProcesses: 4096, MaxWorkspaceBytes: 20 << 30,
	}
}

func DefaultBuilderDeniedCIDRs() []string {
	return []string{"169.254.169.254/32", "100.100.100.200/32", "fd00:ec2::254/128"}
}

func applyBuilderSandboxDefaults(cfg *BuilderSandboxConfig) {
	if cfg.Backend == "" {
		cfg.Backend = "containerd"
	}
	if cfg.Socket == "" {
		cfg.Socket = "/run/containerd/containerd.sock"
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "builder"
	}
	// Image has no default: the operator chooses the image builds
	// run in, and validation requires it for the hardened executor.
	if cfg.Runtime == "" {
		cfg.Runtime = "io.containerd.runc.v2"
	}
	if cfg.Snapshotter == "" {
		cfg.Snapshotter = "overlayfs"
	}
	if cfg.CNIPluginDir == "" {
		cfg.CNIPluginDir = "/usr/lib/cni"
	}
	if cfg.CNIConfDir == "" {
		cfg.CNIConfDir = "/etc/cni/net.d"
	}
	if cfg.CNINetwork == "" {
		cfg.CNINetwork = "build-sandbox"
	}
	if cfg.BuildkitdBinary == "" {
		cfg.BuildkitdBinary = "buildkitd"
	}
}

func FinalizeControlPlane(cfg *ControlPlaneConfig) error {
	profile, err := applyProfile(cfg.Profile)
	if err != nil {
		return err
	}
	cfg.Profile = profile
	applyControlPlaneDefaults(cfg)
	return validateControlPlane(*cfg)
}

func FinalizeAgent(cfg *AgentConfig) error {
	profile, err := applyProfile(cfg.Profile)
	if err != nil {
		return err
	}
	cfg.Profile = profile
	applyAgentDefaults(cfg)
	return validateAgent(*cfg)
}

func FinalizeBuilder(cfg *BuilderConfig) error {
	profile, err := applyProfile(cfg.Profile)
	if err != nil {
		return err
	}
	cfg.Profile = profile
	applyBuilderDefaults(cfg)
	return validateBuilder(*cfg)
}
