package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const defaultIngressMinSyncInterval = 2 * time.Second

var routeSlicePool = sync.Pool{
	New: func() interface{} {
		return make([]caddyRoute, 0, 16)
	},
}

type caddyRoute struct {
	Match    []caddyRouteMatch  `json:"match"`
	Handle   []caddyRouteHandle `json:"handle"`
	Terminal bool               `json:"terminal"`
}

type caddyRouteMatch struct {
	Host []string `json:"host"`
}

type caddyRouteHandle struct {
	Handler   string          `json:"handler"`
	Upstreams []caddyUpstream `json:"upstreams"`
}

type caddyUpstream struct {
	Dial string `json:"dial"`
}

type caddyServer struct {
	Listen         []string             `json:"listen"`
	Routes         []caddyRoute         `json:"routes"`
	AutomaticHTTPS *caddyAutomaticHTTPS `json:"automatic_https,omitempty"`
}

type caddyAutomaticHTTPS struct {
	Disable bool `json:"disable,omitempty"`
}

type caddyHTTPApp struct {
	Servers map[string]caddyServer `json:"servers"`
}

type caddyApps struct {
	HTTP caddyHTTPApp `json:"http"`
}

type caddyConfig struct {
	Admin *caddyAdmin `json:"admin,omitempty"`
	Apps  caddyApps   `json:"apps"`
}

type caddyAdmin struct {
	Listen string `json:"listen,omitempty"`
}

type IngressStaticRoute struct {
	Hosts    []string
	Upstream string
}

type IngressSyncerOption func(*IngressSyncer)

type IngressSyncer struct {
	adminURL         string
	client           *http.Client
	store            *Store
	staticRoutes     []IngressStaticRoute
	listenAddrs      []string
	adminListen      string
	disableAutoHTTPS bool
	minSyncInterval  time.Duration
	pushMu           sync.Mutex
	mu               sync.Mutex
	timer            *time.Timer
	dirty            bool
}

func WithIngressStaticRoutes(routes []IngressStaticRoute) IngressSyncerOption {
	return func(i *IngressSyncer) {
		i.staticRoutes = append([]IngressStaticRoute(nil), routes...)
	}
}

func WithIngressListenAddrs(addrs []string) IngressSyncerOption {
	return func(i *IngressSyncer) {
		i.listenAddrs = append([]string(nil), addrs...)
	}
}

func WithIngressAdminListen(addr string) IngressSyncerOption {
	return func(i *IngressSyncer) {
		i.adminListen = strings.TrimSpace(addr)
	}
}

func WithIngressAutomaticHTTPSDisabled(disable bool) IngressSyncerOption {
	return func(i *IngressSyncer) {
		i.disableAutoHTTPS = disable
	}
}

func NewIngressSyncer(adminURL string, store *Store, opts ...IngressSyncerOption) *IngressSyncer {
	syncer := &IngressSyncer{
		adminURL:        adminURL,
		client:          &http.Client{},
		store:           store,
		listenAddrs:     []string{":80", ":443"},
		minSyncInterval: defaultIngressMinSyncInterval,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(syncer)
		}
	}
	if len(syncer.listenAddrs) == 0 {
		syncer.listenAddrs = []string{":80", ":443"}
	}
	return syncer
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

func (i *IngressSyncer) render(ctx context.Context) (*caddyConfig, error) {
	backends, err := i.store.listHealthyIngressBackends(ctx)
	if err != nil {
		return nil, err
	}
	routes := routeSlicePool.Get().([]caddyRoute)
	defer func() {
		routes = routes[:0]
		routeSlicePool.Put(routes)
	}()
	for _, staticRoute := range i.staticRoutes {
		route, ok := ingressRoute(staticRoute.Hosts, staticRoute.Upstream)
		if !ok {
			continue
		}
		routes = append(routes, route)
	}
	for _, backend := range backends {
		route, ok := ingressRoute([]string{backend.Domain}, backend.EndpointAddr)
		if !ok {
			continue
		}
		routes = append(routes, route)
	}
	server := caddyServer{
		Listen: append([]string(nil), i.listenAddrs...),
		Routes: append([]caddyRoute(nil), routes...),
	}
	if i.disableAutoHTTPS {
		server.AutomaticHTTPS = &caddyAutomaticHTTPS{Disable: true}
	}
	cfg := &caddyConfig{
		Apps: caddyApps{
			HTTP: caddyHTTPApp{
				Servers: map[string]caddyServer{
					"srv0": server,
				},
			},
		},
	}
	if i.adminListen != "" {
		cfg.Admin = &caddyAdmin{Listen: i.adminListen}
	}
	return cfg, nil
}

func ingressRoute(hosts []string, upstream string) (caddyRoute, bool) {
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		return caddyRoute{}, false
	}
	normalizedHosts := normalizeIngressHosts(hosts)
	if len(normalizedHosts) == 0 {
		return caddyRoute{}, false
	}
	return caddyRoute{
		Match: []caddyRouteMatch{{Host: normalizedHosts}},
		Handle: []caddyRouteHandle{{
			Handler:   "reverse_proxy",
			Upstreams: []caddyUpstream{{Dial: upstream}},
		}},
		Terminal: true,
	}, true
}

func normalizeIngressHosts(hosts []string) []string {
	out := make([]string, 0, len(hosts))
	seen := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		host = strings.TrimSpace(strings.ToLower(host))
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	slices.Sort(out)
	return out
}
