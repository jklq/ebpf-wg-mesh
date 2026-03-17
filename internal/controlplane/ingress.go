package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

type IngressSyncer struct {
	adminURL        string
	publicAddr      string
	apiHTTPUpstream string
	client          *http.Client
	store           *Store
	mu              sync.Mutex
}

func NewIngressSyncer(adminURL, publicAddr, apiHTTPUpstream string, store *Store) *IngressSyncer {
	return &IngressSyncer{
		adminURL:        adminURL,
		publicAddr:      publicAddr,
		apiHTTPUpstream: apiHTTPUpstream,
		client:          &http.Client{},
		store:           store,
	}
}

func (i *IngressSyncer) Sync(ctx context.Context) error {
	if i == nil || i.adminURL == "" {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()

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
	routes := make([]map[string]any, 0, 1)
	if i.publicAddr != "" && i.apiHTTPUpstream != "" {
		routes = append(routes, map[string]any{
			"match": []map[string]any{{"host": []string{i.publicAddr}}},
			"handle": []map[string]any{{
				"handler": "reverse_proxy",
				"upstreams": []map[string]string{{
					"dial": i.apiHTTPUpstream,
				}},
			}},
			"terminal": true,
		})
	}
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
