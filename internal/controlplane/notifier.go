package controlplane

import (
	"log/slog"
)

type liveNotifications interface {
	Watch(string) (<-chan struct{}, func())
	Notify(string)
}

type Notifier struct {
	live liveNotifications
}

func NewNotifier(live liveNotifications) *Notifier {
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
