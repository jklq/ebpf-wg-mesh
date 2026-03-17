package controlplane

import (
	"log/slog"
	"sync"
)

type Notifier struct {
	mu       sync.Mutex
	watchers map[string][]chan struct{}
}

func NewNotifier() *Notifier {
	return &Notifier{watchers: make(map[string][]chan struct{})}
}

func (n *Notifier) Watch(agentID string) (<-chan struct{}, func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	ch := make(chan struct{}, 1)
	n.watchers[agentID] = append(n.watchers[agentID], ch)
	slog.Info("watch registered", "agent_id", agentID, "watchers", len(n.watchers[agentID]))
	return ch, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		w := n.watchers[agentID]
		for i := range w {
			if w[i] == ch {
				n.watchers[agentID] = append(w[:i], w[i+1:]...)
				slog.Info("watch removed", "agent_id", agentID, "watchers", len(n.watchers[agentID]))
				close(ch)
				break
			}
		}
	}
}

func (n *Notifier) Notify(agentID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	slog.Info("notify agent", "agent_id", agentID, "watchers", len(n.watchers[agentID]))
	for _, ch := range n.watchers[agentID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (n *Notifier) NotifyAll(agentIDs []string) {
	for _, id := range agentIDs {
		n.Notify(id)
	}
}
