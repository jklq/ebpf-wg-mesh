package agent

import (
	"context"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

type Runtime interface {
	// DiscoverRuntimeResources must enumerate every resource owned by this
	// runtime before supervision starts. ReconcileWithCleanup must honor a
	// false allowCleanup value by leaving resources absent from desired state
	// untouched.
	DiscoverRuntimeResources(context.Context) ([]RuntimeResource, error)
	ReconcileWithCleanup(context.Context, *agentv1.DesiredNodeState, bool) (*agentv1.StatusReport, error)
	Close() error
}

type RuntimeEventSource interface {
	ReconcileEvents(context.Context) (<-chan struct{}, <-chan error)
}

type managedDashboardRestartRuntime interface {
	RestartManagedDashboard(context.Context, *agentv1.DesiredNodeState) error
}
