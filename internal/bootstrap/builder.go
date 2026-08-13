package bootstrap

import (
	"flag"
	"fmt"

	"ebof-wg-mesh/internal/config"
)

func Builder(args []string) (config.BuilderConfig, error) {
	var cfg config.BuilderConfig
	var profile string

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
	stringFlag(fs, &cfg.GitBinary, "git-binary", "BUILDER_GIT_BINARY", "git", "")
	stringFlag(fs, &cfg.BuildctlBinary, "buildctl-binary", "BUILDER_BUILDCTL_BINARY", "buildctl", "")
	stringFlag(fs, &cfg.BuildkitAddress, "buildkit-address", "BUILDER_BUILDKIT_ADDRESS", "unix:///run/buildkit/buildkitd.sock", "")
	boolFlag(fs, &cfg.CleanupWorkDir, "cleanup-work-dir", "BUILDER_CLEANUP_WORK_DIR", true, "")

	if err := fs.Parse(args); err != nil {
		return config.BuilderConfig{}, err
	}
	normalized, err := config.NormalizeProfile(profile)
	if err != nil {
		return config.BuilderConfig{}, err
	}
	cfg.Profile = normalized
	if err := config.FinalizeBuilder(&cfg); err != nil {
		return config.BuilderConfig{}, fmt.Errorf("bootstrap builder: %w", err)
	}
	return cfg, nil
}
