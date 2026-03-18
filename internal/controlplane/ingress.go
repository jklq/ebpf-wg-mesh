package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const defaultIngressMinSyncInterval = 2 * time.Second

type IngressSyncer struct {
	adminURL        string
	client          *http.Client
	store           *Store
	minSyncInterval time.Duration
	pushMu          sync.Mutex
	mu              sync.Mutex
	timer           *time.Timer
	dirty           bool
}

func NewIngressSyncer(adminURL string, store *Store) *IngressSyncer {
	return &IngressSyncer{
		adminURL:        adminURL,
		client:          &http.Client{},
		store:           store,
		minSyncInterval: defaultIngressMinSyncInterval,
	}
}

func (i *IngressSyncer) Sync(ctx context.Context) error {
	if i == nil || i.adminURL == "" {
		return nil
	}
	i.pushMu.Lock()
	defer i.pushMu.Unlock()

	return i.syncLocked(ctx)
}

func (i *IngressSyncer) RequestSync() {
	if i == nil || i.adminURL == "" {
		return
	}

	i.mu.Lock()
	if i.timer == nil {
		i.timer = time.AfterFunc(i.minSyncInterval, i.onDebounceWindowEnd)
		i.mu.Unlock()
		go i.syncAsync()
		return
	}
	i.dirty = true
	i.mu.Unlock()
}

func (i *IngressSyncer) onDebounceWindowEnd() {
	i.mu.Lock()
	if !i.dirty {
		i.timer = nil
		i.mu.Unlock()
		return
	}
	i.dirty = false
	i.timer = time.AfterFunc(i.minSyncInterval, i.onDebounceWindowEnd)
	i.mu.Unlock()

	go i.syncAsync()
}

func (i *IngressSyncer) syncAsync() {
	if err := i.Sync(context.Background()); err != nil {
		slog.Warn("ingress sync failed", "error", err)
	}
}

func (i *IngressSyncer) syncLocked(ctx context.Context) error {
	cfg, err := i.render(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, i.adminURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := i.client.Do(req)
	if err != nil {
		return fmt.Errorf("push caddy config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("caddy admin returned %s", resp.Status)
	}
	return nil
}

func (i *IngressSyncer) render(ctx context.Context) (map[string]any, error) {
	backends, err := i.store.listHealthyIngressBackends(ctx)
	if err != nil {
		return nil, err
	}
	routes := make([]map[string]any, 0, len(backends))
	for _, backend := range backends {
		routes = append(routes, map[string]any{
			"match": []map[string]any{{"host": []string{backend.Domain}}},
			"handle": []map[string]any{{
				"handler": "reverse_proxy",
				"upstreams": []map[string]string{{
					"dial": backend.EndpointAddr,
				}},
			}},
			"terminal": true,
		})
	}
	return map[string]any{
		"apps": map[string]any{
			"http": map[string]any{
				"servers": map[string]any{
					"srv0": map[string]any{
						"listen": []string{":80", ":443"},
						"routes": routes,
					},
				},
			},
		},
	}, nil
}
