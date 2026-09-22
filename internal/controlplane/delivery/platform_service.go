package delivery

import (
	"context"
)

type PlatformNotifier interface {
	Notify(agentID string)
}

type PlatformIngress interface {
	Sync(ctx context.Context) error
	RequestSync()
	// Converged reports whether every known ingress subscriber has fully
	// applied the latest synced state. A rollout must not destroy withdrawn
	// allocations before this: a disconnected or NACKing subscriber keeps
	// routing to them until it applies the withdrawal.
	Converged(ctx context.Context) (bool, error)
}
