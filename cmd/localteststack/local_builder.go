package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"ebof-wg-mesh/internal/builder"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane"
)

const localBuilderID = "localteststack-builder"

func startLocalBuilder(ctx context.Context, stateDir string, controlPlaneAddr string, server *controlplane.Server) (*builder.App, <-chan error, error) {
	identity, err := server.EnsureBuilderClientIdentity(ctx, localBuilderID)
	if err != nil {
		return nil, nil, fmt.Errorf("mint builder client identity: %w", err)
	}

	builderDir := filepath.Join(stateDir, "local-builder")
	if err := os.MkdirAll(builderDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("mkdir local builder dir: %w", err)
	}
	caPath := filepath.Join(builderDir, "controlplane-ca.pem")
	certPath := filepath.Join(builderDir, "builder.crt")
	keyPath := filepath.Join(builderDir, "builder.key")
	if err := os.WriteFile(caPath, identity.CAPEM, 0o644); err != nil {
		return nil, nil, fmt.Errorf("write builder ca: %w", err)
	}
	if err := os.WriteFile(certPath, identity.CertPEM, 0o644); err != nil {
		return nil, nil, fmt.Errorf("write builder cert: %w", err)
	}
	if err := os.WriteFile(keyPath, identity.KeyPEM, 0o600); err != nil {
		return nil, nil, fmt.Errorf("write builder key: %w", err)
	}

	buildBinary, buildkitAddress, err := resolveLocalBuilderBuildConfig()
	if err != nil {
		return nil, nil, err
	}
	cfg := config.BuilderConfig{
		Profile: config.ProfileDevelopment,
		ID:      localBuilderID,
		Name:    "Local Teststack Builder",
		ControlPlane: config.BuilderControlPlaneConfig{
			Address: controlPlaneAddr,
			TLS: config.InternalClientTLSConfig{
				CAFile:     caPath,
				CertFile:   certPath,
				KeyFile:    keyPath,
				ServerName: "localhost",
			},
		},
		WorkDir:                  filepath.Join(builderDir, "work"),
		PollIntervalSeconds:      1,
		HeartbeatIntervalSeconds: 5,
		BuildctlBinary:           buildBinary,
		BuildkitAddress:          buildkitAddress,
		CleanupWorkDir:           true,
	}
	if err := config.FinalizeBuilder(&cfg); err != nil {
		return nil, nil, fmt.Errorf("finalize local builder config: %w", err)
	}
	fmt.Println(config.BuilderStartupContract(cfg).String())
	if _, err := exec.LookPath(cfg.BuildctlBinary); err != nil {
		return nil, nil, fmt.Errorf("find %s: %w", cfg.BuildctlBinary, err)
	}

	app, err := builder.New(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create local builder: %w", err)
	}
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- app.Run(ctx)
	}()
	return app, runErrCh, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func resolveLocalBuilderBuildConfig() (string, string, error) {
	const defaultBuildkitAddress = "unix:///run/buildkit/buildkitd.sock"

	if binary := strings.TrimSpace(os.Getenv("BUILDER_BUILDCTL_BINARY")); binary != "" {
		return binary, firstNonEmpty(os.Getenv("BUILDER_BUILDKIT_ADDRESS"), defaultBuildkitAddress), nil
	}
	if address := strings.TrimSpace(os.Getenv("BUILDER_BUILDKIT_ADDRESS")); address != "" {
		return "buildctl", address, nil
	}
	if _, err := os.Stat("/run/buildkit/buildkitd.sock"); err == nil {
		return "buildctl", defaultBuildkitAddress, nil
	}
	if _, err := exec.LookPath("docker"); err == nil {
		return "docker", "docker-buildx", nil
	}
	return "buildctl", defaultBuildkitAddress, nil
}
