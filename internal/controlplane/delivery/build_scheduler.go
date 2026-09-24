package delivery

import (
	"time"

	"ebof-wg-mesh/internal/config"
)

// BuildSchedulerConfig tunes build leases and queue limits.
type BuildSchedulerConfig struct {
	// LeaseTTL is how long a claimed build stays owned without a heartbeat.
	LeaseTTL time.Duration
	// AttemptLimit bounds total claims including takeovers.
	AttemptLimit int64
	// MaxConcurrentGlobal caps running builds cluster-wide.
	MaxConcurrentGlobal int
	// MaxConcurrentPerProject caps running builds per project.
	MaxConcurrentPerProject int
	// BuildTimeout bounds one claim from claim to completion.
	BuildTimeout time.Duration
	// MaxQueueAge bounds how long a build may wait in queued state.
	MaxQueueAge time.Duration
}

// DefaultBuildSchedulerConfig returns the default limits.
func DefaultBuildSchedulerConfig() BuildSchedulerConfig {
	defaults := config.DefaultControlPlaneBuilderConfig()
	return BuildSchedulerConfig{
		LeaseTTL:                time.Duration(defaults.LeaseTTLSeconds) * time.Second,
		AttemptLimit:            int64(defaults.MaxAttempts),
		MaxConcurrentGlobal:     defaults.MaxConcurrentGlobal,
		MaxConcurrentPerProject: defaults.MaxConcurrentPerProject,
		BuildTimeout:            time.Duration(defaults.BuildTimeoutSeconds) * time.Second,
		MaxQueueAge:             time.Duration(defaults.MaxQueueAgeSeconds) * time.Second,
	}
}

// WithDefaults fills zero fields from DefaultBuildSchedulerConfig.
func (c BuildSchedulerConfig) WithDefaults() BuildSchedulerConfig {
	def := DefaultBuildSchedulerConfig()
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = def.LeaseTTL
	}
	if c.AttemptLimit <= 0 {
		c.AttemptLimit = def.AttemptLimit
	}
	if c.MaxConcurrentGlobal <= 0 {
		c.MaxConcurrentGlobal = def.MaxConcurrentGlobal
	}
	if c.MaxConcurrentPerProject <= 0 {
		c.MaxConcurrentPerProject = def.MaxConcurrentPerProject
	}
	if c.BuildTimeout <= 0 {
		c.BuildTimeout = def.BuildTimeout
	}
	if c.MaxQueueAge <= 0 {
		c.MaxQueueAge = def.MaxQueueAge
	}
	return c
}
