package controlplane

import (
	"log/slog"
	"sync"
)

type Notifier struct {
	mu       sync.Mutex
	watchers map[string][]chan struct{}
	indexMap map[chan struct{}]int
}

func NewNotifier() *Notifier {
	return &Notifier{
		watchers: make(map[string][]chan struct{}),
		indexMap: make(map[chan struct{}]int),
	}
}

func (n *Notifier) Watch(agentID string) (<-chan struct{}, func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	ch := make(chan struct{}, 1)
	n.watchers[agentID] = append(n.watchers[agentID], ch)
	n.indexMap[ch] = len(n.watchers[agentID]) - 1
	slog.Info("watch registered", "agent_id", agentID, "watchers", len(n.watchers[agentID]))
	return ch, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		w := n.watchers[agentID]
		idx, ok := n.indexMap[ch]
		if !ok || idx >= len(w) || w[idx] != ch {
			return
		}
		last := len(w) - 1
		if idx != last {
			w[idx] = w[last]
			n.indexMap[w[idx]] = idx
		}
		n.watchers[agentID] = w[:last]
		delete(n.indexMap, ch)
		slog.Info("watch removed", "agent_id", agentID, "watchers", len(n.watchers[agentID]))
		close(ch)
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
