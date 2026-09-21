package delivery

import "time"

// BuildSchedulerConfig tunes the durable lease-based build scheduler (2.5).
// Zero values select Defaults via WithDefaults.
type BuildSchedulerConfig struct {
	// LeaseTTL is how long a claimed build stays owned without a heartbeat.
	LeaseTTL time.Duration
	// AttemptLimit bounds total claims including takeovers.
	AttemptLimit int64
	// MaxConcurrentGlobal caps running builds cluster-wide.
	MaxConcurrentGlobal int
	// MaxConcurrentPerProject caps running builds per project. Projects are
	// the workspace proxy until 3.1 introduces workspaces.
	MaxConcurrentPerProject int
	// BuildTimeout bounds one claim from claim to completion.
	BuildTimeout time.Duration
	// MaxQueueAge bounds how long a build may wait in queued state.
	MaxQueueAge time.Duration
}

// DefaultBuildSchedulerConfig is the production floor. Fairness (3.19) and
// quotas (3.3) layer on top; removing them degrades to these flat caps.
func DefaultBuildSchedulerConfig() BuildSchedulerConfig {
	return BuildSchedulerConfig{
		LeaseTTL:                120 * time.Second,
		AttemptLimit:            3,
		MaxConcurrentGlobal:     20,
		MaxConcurrentPerProject: 5,
		BuildTimeout:            30 * time.Minute,
		MaxQueueAge:             2 * time.Hour,
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
