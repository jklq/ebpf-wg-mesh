package controlplane

import (
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"log/slog"
)

type Notifier struct {
	live *deliverycore.Live
}

func NewNotifier(live *deliverycore.Live) *Notifier {
	return &Notifier{live: live}
}

func (n *Notifier) Watch(agentID string) (<-chan struct{}, func()) {
	if n == nil || n.live == nil {
		ch := make(chan struct{})
		close(ch)
		return ch, func() {}
	}
	ch, stop := n.live.Watch(agentID)
	slog.Info("watch registered", "agent_id", agentID)
	return ch, func() {
		stop()
		slog.Info("watch removed", "agent_id", agentID)
	}
}

func (n *Notifier) Notify(agentID string) {
	if n == nil || n.live == nil {
		return
	}
	n.live.Notify(agentID)
}

func (n *Notifier) NotifyAll(agentIDs []string) {
	if n == nil || n.live == nil {
		return
	}
	if len(agentIDs) == 0 {
		n.live.Notify("")
		return
	}
	for _, id := range agentIDs {
		n.live.Notify(id)
	}
}
