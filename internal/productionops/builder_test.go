package productionops

import (
	"strings"
	"testing"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/deploy"
)

func TestBuilderConfigurationRequiresGVisorReleaseTools(t *testing.T) {
	installation := deploy.Installation{
		ID:    "production",
		Hosts: []deploy.Host{{ID: "a", Architecture: "amd64", Network: deploy.Network{Address: "10.0.0.1"}}},
	}
	release := deploy.Release{
		ID: "r43",
		Images: map[string]string{
			"builder-sandbox":   "registry.example.test/builder-sandbox@sha256:" + strings.Repeat("1", 64),
			"railpack-frontend": "registry.example.test/railpack-frontend@sha256:" + strings.Repeat("2", 64),
		},
		Tools: map[string]map[string]deploy.Artifact{
			"buildkitd": {"amd64": {SHA256: strings.Repeat("3", 64)}},
		},
	}
	runner := &Runner{
		Plan: deploy.Plan{
			Installation: installation,
			Release:      release,
			Placements:   []deploy.Placement{{Role: deploy.ControlPlane, Host: "a", Instance: "cp-a"}},
		},
		Config: Config{InternalServerName: "core.example.invalid", BuilderSandbox: config.BuilderSandboxConfig{Snapshotter: "overlayfs"}},
	}
	builder := deploy.Placement{Role: deploy.Builder, Host: "a", Instance: "builder-a"}

	if _, err := runner.builderConfiguration(builder); err == nil || !strings.Contains(err.Error(), "must package runsc") {
		t.Fatalf("gVisor builder accepted a release without runsc: %v", err)
	}
	for _, name := range []string{"runsc", "containerd-shim-runsc-v1", "runsc-buildkit"} {
		runner.Plan.Release.Tools[name] = map[string]deploy.Artifact{"amd64": {SHA256: strings.Repeat("4", 64)}}
	}
	cfg, err := runner.builderConfiguration(builder)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox.Isolation != "gvisor" {
		t.Fatalf("builder isolation = %q, want gvisor", cfg.Sandbox.Isolation)
	}

	// A Kata builder needs no gVisor tools.
	delete(runner.Plan.Release.Tools, "runsc")
	runner.Config.BuilderSandbox.Isolation = "kata"
	if _, err := runner.builderConfiguration(builder); err != nil {
		t.Fatalf("kata builder rejected for missing gVisor tools: %v", err)
	}
}
