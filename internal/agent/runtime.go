package agent

import (
	"context"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

type Runtime interface {
	Reconcile(context.Context, *agentv1.DesiredNodeState) (*agentv1.StatusReport, error)
	Close() error
}

type RuntimeEventSource interface {
	ReconcileEvents(context.Context) (<-chan struct{}, <-chan error)
}

type managedDashboardRestartRuntime interface {
	RestartManagedDashboard(context.Context, *agentv1.DesiredNodeState) error
}
