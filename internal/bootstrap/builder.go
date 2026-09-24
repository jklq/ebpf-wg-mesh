package bootstrap

import (
	"flag"
	"fmt"

	"ebof-wg-mesh/internal/config"
)

func Builder(args []string) (config.BuilderConfig, error) {
	var cfg config.BuilderConfig
	var profile string
	var deniedCIDRs string
	var nameservers string

	fs := flag.NewFlagSet("builder", flag.ContinueOnError)
	stringFlag(fs, &profile, "profile", "BUILDER_PROFILE", "", "development or production; empty defaults to production")
	stringFlag(fs, &cfg.Health.Listen, "health-listen", "BUILDER_HEALTH_LISTEN", "", "liveness and readiness listen address")
	stringFlag(fs, &cfg.ID, "builder-id", "BUILDER_ID", "", "")
	stringFlag(fs, &cfg.Name, "builder-name", "BUILDER_NAME", "", "")
	stringFlag(fs, &cfg.ControlPlane.Address, "controlplane-address", "BUILDER_CONTROLPLANE_ADDRESS", "", "")
	stringFlag(fs, &cfg.ControlPlane.TLS.CAFile, "ca-file", "BUILDER_CA_FILE", "", "")
	stringFlag(fs, &cfg.ControlPlane.TLS.CertFile, "cert-file", "BUILDER_CERT_FILE", "", "")
	stringFlag(fs, &cfg.ControlPlane.TLS.KeyFile, "key-file", "BUILDER_KEY_FILE", "", "")
	stringFlag(fs, &cfg.ControlPlane.TLS.ServerName, "server-name", "BUILDER_SERVER_NAME", "controlplane", "")
	stringFlag(fs, &cfg.WorkDir, "work-dir", "BUILDER_WORK_DIR", "var/builder", "")
	intFlag(fs, &cfg.PollIntervalSeconds, "poll-interval-seconds", "BUILDER_POLL_INTERVAL_SECONDS", 5, "")
	intFlag(fs, &cfg.HeartbeatIntervalSeconds, "heartbeat-interval-seconds", "BUILDER_HEARTBEAT_INTERVAL_SECONDS", 10, "")
	int64Flag(fs, &cfg.Logs.SpoolMaxBytes, "logs-spool-max-bytes", "BUILDER_LOGS_SPOOL_MAX_BYTES", 64<<20, "per-attempt disk spool cap under the work dir")
	intFlag(fs, &cfg.Logs.RatePerSec, "logs-rate-per-sec", "BUILDER_LOGS_RATE_PER_SEC", 200, "accepted log lines per build per second")
	intFlag(fs, &cfg.Logs.Burst, "logs-burst", "BUILDER_LOGS_BURST", 1000, "")
	intFlag(fs, &cfg.Logs.FlushBatchSize, "logs-flush-batch-size", "BUILDER_LOGS_FLUSH_BATCH_SIZE", 100, "")
	intFlag(fs, &cfg.Logs.FlushIntervalSeconds, "logs-flush-interval-seconds", "BUILDER_LOGS_FLUSH_INTERVAL_SECONDS", 1, "")
	stringFlag(fs, &cfg.BuildctlBinary, "buildctl-binary", "BUILDER_BUILDCTL_BINARY", "buildctl", "")
	stringFlag(fs, &cfg.BuildkitAddress, "buildkit-address", "BUILDER_BUILDKIT_ADDRESS", "unix:///run/buildkit/buildkitd.sock", "")
	stringFlag(fs, &cfg.RailpackBinary, "railpack-binary", "BUILDER_RAILPACK_BINARY", "railpack", "")
	stringFlag(fs, &cfg.RailpackFrontendImage, "railpack-frontend-image", "BUILDER_RAILPACK_FRONTEND_IMAGE", "ghcr.io/railwayapp/railpack-frontend:latest", "")
	stringFlag(fs, &cfg.Executor, "executor", "BUILDER_EXECUTOR", "", "build executor backend: development or hardened; empty defaults by profile")
	stringFlag(fs, &cfg.Sandbox.Backend, "sandbox-backend", "BUILDER_SANDBOX_BACKEND", "containerd", "hardened executor sandbox backend")
	stringFlag(fs, &cfg.Sandbox.Socket, "sandbox-socket", "BUILDER_SANDBOX_SOCKET", "/run/containerd/containerd.sock", "containerd socket for build sandboxes")
	stringFlag(fs, &cfg.Sandbox.Namespace, "sandbox-namespace", "BUILDER_SANDBOX_NAMESPACE", "builder", "containerd namespace for build sandboxes")
	stringFlag(fs, &cfg.Sandbox.Image, "sandbox-image", "BUILDER_SANDBOX_IMAGE", "", "sandbox image carrying the build toolchain (required for hardened)")
	stringFlag(fs, &cfg.Sandbox.Runtime, "sandbox-runtime", "BUILDER_SANDBOX_RUNTIME", "io.containerd.runc.v2", "OCI runtime for build sandboxes")
	stringFlag(fs, &cfg.Sandbox.Snapshotter, "sandbox-snapshotter", "BUILDER_SANDBOX_SNAPSHOTTER", "overlayfs", "containerd snapshotter for sandbox roots")
	stringFlag(fs, &cfg.Sandbox.CNIPluginDir, "sandbox-cni-plugin-dir", "BUILDER_SANDBOX_CNI_PLUGIN_DIR", "/usr/lib/cni", "CNI plugin directory for build networking")
	stringFlag(fs, &cfg.Sandbox.CNIConfDir, "sandbox-cni-conf-dir", "BUILDER_SANDBOX_CNI_CONF_DIR", "/etc/cni/net.d", "CNI configuration directory for build networking")
	stringFlag(fs, &cfg.Sandbox.CNINetwork, "sandbox-cni-network", "BUILDER_SANDBOX_CNI_NETWORK", "build-sandbox", "CNI network name for build sandboxes")
	stringFlag(fs, &cfg.Sandbox.BuildkitdBinary, "sandbox-buildkitd-binary", "BUILDER_SANDBOX_BUILDKITD_BINARY", "buildkitd", "per-execution BuildKit daemon binary")
	stringFlag(fs, &nameservers, "sandbox-nameservers", "BUILDER_SANDBOX_NAMESERVERS", "", "comma-separated sandbox resolver IPs; empty inherits host non-loopback resolvers")
	stringFlag(fs, &cfg.Cache.Mode, "cache-mode", "BUILDER_CACHE_MODE", "none", "build cache mode: none or content-addressed")
	intFlag(fs, &cfg.Limits.TimeoutSeconds, "build-timeout-seconds", "BUILDER_BUILD_TIMEOUT_SECONDS", 1800, "")
	int64Flag(fs, &cfg.Limits.MemoryBytes, "build-memory-bytes", "BUILDER_BUILD_MEMORY_BYTES", 8<<30, "")
	int64Flag(fs, &cfg.Limits.CPUSeconds, "build-cpu-seconds", "BUILDER_BUILD_CPU_SECONDS", 3600, "")
	int64Flag(fs, &cfg.Limits.MaxFileBytes, "build-max-file-bytes", "BUILDER_BUILD_MAX_FILE_BYTES", 10<<30, "")
	int64Flag(fs, &cfg.Limits.MaxProcesses, "build-max-processes", "BUILDER_BUILD_MAX_PROCESSES", 4096, "")
	int64Flag(fs, &cfg.Limits.MaxWorkspaceBytes, "build-max-workspace-bytes", "BUILDER_BUILD_MAX_WORKSPACE_BYTES", 20<<30, "")
	boolFlag(fs, &cfg.Network.DenyGeneralEgress, "build-deny-general-egress", "BUILDER_BUILD_DENY_GENERAL_EGRESS", false, "")
	stringFlag(fs, &deniedCIDRs, "build-denied-cidrs", "BUILDER_BUILD_DENIED_CIDRS", "", "comma-separated CIDRs denied to builds")
	boolFlag(fs, &cfg.CleanupWorkDir, "cleanup-work-dir", "BUILDER_CLEANUP_WORK_DIR", true, "")

	if err := fs.Parse(args); err != nil {
		return config.BuilderConfig{}, err
	}
	normalized, err := config.NormalizeProfile(profile)
	if err != nil {
		return config.BuilderConfig{}, err
	}
	cfg.Profile = normalized
	if cidrs := splitCommaList(deniedCIDRs); len(cidrs) > 0 {
		cfg.Network.DeniedCIDRs = cidrs
	}
	if servers := splitCommaList(nameservers); len(servers) > 0 {
		cfg.Sandbox.Nameservers = servers
	}
	if err := config.FinalizeBuilder(&cfg); err != nil {
		return config.BuilderConfig{}, fmt.Errorf("bootstrap builder: %w", err)
	}
	return cfg, nil
}
