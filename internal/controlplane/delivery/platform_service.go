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
}
