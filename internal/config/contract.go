package config

import (
	"fmt"
	"strings"
)

const (
	DependencyDurable   = "durable"
	DependencyLoopback  = "loopback"
	DependencyEphemeral = "ephemeral"
	DependencyRemote    = "remote"
	DependencyUnset     = "unset"
)

type DependencyRef struct {
	Name  string
	Class string
}

type StartupContract struct {
	Component    string
	Profile      Profile
	Features     []string
	Dependencies []DependencyRef
}

func (c StartupContract) String() string {
	features := c.Features
	if len(features) == 0 {
		features = []string{"none"}
	}
	deps := make([]string, 0, len(c.Dependencies))
	for _, dep := range c.Dependencies {
		deps = append(deps, fmt.Sprintf("%s=%s", dep.Name, dep.Class))
	}
	if len(deps) == 0 {
		deps = []string{"none"}
	}
	return fmt.Sprintf(
		"startup contract component=%s profile=%s features=%s dependencies=%s",
		c.Component,
		c.Profile,
		strings.Join(features, ","),
		strings.Join(deps, ","),
	)
}

func ControlPlaneStartupContract(cfg ControlPlaneConfig) StartupContract {
	features := make([]string, 0, 6)
	if cfg.Dashboard.Enabled {
		features = append(features, "dashboard")
	}
	features = append(features, "ingress")
	if cfg.Registry.Host != "" {
		features = append(features, "registry_auth")
	}
	if cfg.GitHub.Enabled {
		features = append(features, "github")
	}
	features = append(features, "source_storage")
	if strings.TrimSpace(cfg.Logs.ClickHouse.URL) != "" {
		features = append(features, "logs")
	}
	features = append(features, "sandbox_production")
	return StartupContract{
		Component: "controlplane",
		Profile:   cfg.Profile,
		Features:  features,
		Dependencies: []DependencyRef{
			{Name: "database", Class: dependencyClassForURL(cfg.Database.URL)},
			{Name: "logs", Class: dependencyClassForURL(cfg.Logs.ClickHouse.URL)},
			{Name: "source_storage", Class: dependencyClassForSourceArchives(cfg.SourceArchives)},
			{Name: "ingress_admin", Class: dependencyClassForAdmin(cfg.Ingress)},
		},
	}
}

func AgentStartupContract(cfg AgentConfig) StartupContract {
	features := []string{"mesh"}
	if cfg.Runtime.DisableCgroups {
		features = append(features, "cgroups_disabled")
	} else {
		features = append(features, "cgroups")
	}
	features = append(features, "workload_sandbox")
	controlPlaneAddress := ""
	if len(cfg.ControlPlane.Addresses) > 0 {
		controlPlaneAddress = cfg.ControlPlane.Addresses[0]
	}
	return StartupContract{
		Component: "agent",
		Profile:   cfg.Profile,
		Features:  features,
		Dependencies: []DependencyRef{
			{Name: "control_plane", Class: dependencyClassForURL(controlPlaneAddress)},
		},
	}
}

func BuilderStartupContract(cfg BuilderConfig) StartupContract {
	features := []string{"builder"}
	if cfg.CleanupWorkDir {
		features = append(features, "cleanup_work_dir")
	}
	return StartupContract{
		Component: "builder",
		Profile:   cfg.Profile,
		Features:  features,
		Dependencies: []DependencyRef{
			{Name: "control_plane", Class: dependencyClassForURL(cfg.ControlPlane.Address)},
		},
	}
}
