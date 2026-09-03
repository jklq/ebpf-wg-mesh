package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultNotifierPollInterval = 250 * time.Millisecond

type Notifier struct {
	mu        sync.Mutex
	watchers  map[string][]chan struct{}
	indexMap  map[chan struct{}]int
	revisions map[string]int64
	store     *Store
}

func NewNotifier(ctx context.Context, store *Store, pollInterval time.Duration) *Notifier {
	if pollInterval <= 0 {
		pollInterval = defaultNotifierPollInterval
	}
	n := &Notifier{
		watchers:  make(map[string][]chan struct{}),
		indexMap:  make(map[chan struct{}]int),
		revisions: make(map[string]int64),
		store:     store,
	}
	if store != nil {
		go n.poll(ctx, pollInterval)
	}
	return n
}

func (n *Notifier) Watch(agentID string) (<-chan struct{}, func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	ch := make(chan struct{}, 1)
	n.watchers[agentID] = append(n.watchers[agentID], ch)
	n.indexMap[ch] = len(n.watchers[agentID]) - 1
	if _, ok := n.revisions[agentID]; !ok {
		n.revisions[agentID] = -1
	}
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
		if len(n.watchers[agentID]) == 0 {
			delete(n.watchers, agentID)
			delete(n.revisions, agentID)
		}
		delete(n.indexMap, ch)
		slog.Info("watch removed", "agent_id", agentID, "watchers", len(n.watchers[agentID]))
		close(ch)
	}
}

// poll makes process-local wake channels an optimization rather than a
// correctness boundary. Desired revisions change transactionally with desired
// state, so every replica eventually observes every committed update.
func (n *Notifier) poll(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		n.mu.Lock()
		agentIDs := make([]string, 0, len(n.watchers))
		for agentID := range n.watchers {
			agentIDs = append(agentIDs, agentID)
		}
		n.mu.Unlock()
		revisions, err := n.store.desiredRevisionsForAgents(ctx, agentIDs)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("poll desired revisions failed", "agents", len(agentIDs), "error", err)
			}
			continue
		}
		for agentID, revision := range revisions {
			n.mu.Lock()
			previous, watched := n.revisions[agentID]
			if watched && revision != previous {
				n.revisions[agentID] = revision
				n.notifyLocked(agentID)
			}
			n.mu.Unlock()
		}
	}
}

func (s *Store) desiredRevisionsForAgents(ctx context.Context, agentIDs []string) (map[string]int64, error) {
	out := make(map[string]int64, len(agentIDs))
	if len(agentIDs) == 0 {
		return out, nil
	}
	sort.Strings(agentIDs)
	args := make([]any, len(agentIDs))
	placeholders := make([]string, len(agentIDs))
	for i, agentID := range agentIDs {
		args[i] = agentID
		placeholders[i] = "$" + strconv.Itoa(i+1)
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT id, desired_revision FROM agents WHERE id IN (%s)`, strings.Join(placeholders, ", "),
	), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var agentID string
		var revision int64
		if err := rows.Scan(&agentID, &revision); err != nil {
			return nil, err
		}
		out[agentID] = revision
	}
	return out, rows.Err()
}

func (n *Notifier) Notify(agentID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notifyLocked(agentID)
}

func (n *Notifier) notifyLocked(agentID string) {
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
