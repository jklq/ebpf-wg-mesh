package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
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
	requestCh        chan struct{}
	loaded           bool
	lastPayload      []byte
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
		requestCh:       make(chan struct{}, 1),
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

	select {
	case i.requestCh <- struct{}{}:
	default:
	}
}

// Run owns asynchronous ingress writes under the server's singleton lease.
// Periodic convergence also catches requests received by a non-owner replica.
func (i *IngressSyncer) Run(ctx context.Context) error {
	if i == nil || i.adminURL == "" {
		<-ctx.Done()
		return nil
	}
	syncNow := func() {
		if err := i.Sync(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("ingress sync failed", "error", err)
		}
	}
	syncNow()
	ticker := time.NewTicker(i.minSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			syncNow()
		case <-i.requestCh:
			syncNow()
		}
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

	// Render before taking the lease-row lock so database reads cannot deadlock
	// behind a concurrent lease renewal. Only the external write needs fencing:
	// takeover waits for it, and a former owner cannot enter this section.
	return i.store.withLeaseGuard(ctx, func() error {
		if bytes.Equal(body, i.lastPayload) {
			return nil
		}

		// POST /load replaces Caddy's HTTP server and drops live connections,
		// including the dashboard Vite HMR websocket. After the initial load,
		// only swap the route list.
		if i.loaded {
			if err := i.pushRoutes(ctx, cfg); err != nil {
				slog.Warn("ingress route patch failed; falling back to full load", "error", err)
				if err := i.pushJSON(ctx, http.MethodPost, i.adminURL, body); err != nil {
					return err
				}
			}
		} else {
			if err := i.pushJSON(ctx, http.MethodPost, i.adminURL, body); err != nil {
				return err
			}
			i.loaded = true
		}
		i.lastPayload = bytes.Clone(body)
		return nil
	})
}

func (i *IngressSyncer) pushRoutes(ctx context.Context, cfg *caddyConfig) error {
	server, ok := cfg.Apps.HTTP.Servers["srv0"]
	if !ok {
		return fmt.Errorf("missing srv0 in rendered ingress config")
	}
	routes := server.Routes
	if routes == nil {
		routes = []caddyRoute{}
	}
	body, err := json.Marshal(routes)
	if err != nil {
		return err
	}
	endpoint := ingressConfigAPIURL(i.adminURL, "/config/apps/http/servers/srv0/routes")
	if endpoint == "" {
		return fmt.Errorf("invalid ingress admin url %q", i.adminURL)
	}
	return i.pushJSON(ctx, http.MethodPatch, endpoint, body)
}

func (i *IngressSyncer) pushJSON(ctx context.Context, method, endpoint string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
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

func ingressConfigAPIURL(adminURL, path string) string {
	parsed, err := url.Parse(adminURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.Path = path
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
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
	for _, backend := range groupIngressBackends(backends) {
		route, ok := ingressRoute(backend.Hosts, backend.Upstreams...)
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

func ingressRoute(hosts []string, upstreams ...string) (caddyRoute, bool) {
	normalizedHosts := normalizeIngressHosts(hosts)
	if len(normalizedHosts) == 0 {
		return caddyRoute{}, false
	}
	dials := make([]caddyUpstream, 0, len(upstreams))
	seen := make(map[string]struct{}, len(upstreams))
	for _, upstream := range upstreams {
		upstream = strings.TrimSpace(upstream)
		if upstream == "" {
			continue
		}
		if _, exists := seen[upstream]; exists {
			continue
		}
		seen[upstream] = struct{}{}
		dials = append(dials, caddyUpstream{Dial: upstream})
	}
	if len(dials) == 0 {
		return caddyRoute{}, false
	}
	return caddyRoute{
		Match: []caddyRouteMatch{{Host: normalizedHosts}},
		Handle: []caddyRouteHandle{{
			Handler:   "reverse_proxy",
			Upstreams: dials,
		}},
		Terminal: true,
	}, true
}

func groupIngressBackends(backends []ingressBackend) []groupedIngressBackend {
	order := make([]string, 0, len(backends))
	grouped := make(map[string]*groupedIngressBackend, len(backends))
	for _, backend := range backends {
		domain := strings.ToLower(strings.TrimSpace(backend.Domain))
		if domain == "" {
			continue
		}
		entry, ok := grouped[domain]
		if !ok {
			entry = &groupedIngressBackend{Hosts: []string{domain}}
			grouped[domain] = entry
			order = append(order, domain)
		}
		entry.Upstreams = append(entry.Upstreams, backend.Upstream)
	}
	out := make([]groupedIngressBackend, 0, len(order))
	for _, domain := range order {
		out = append(out, *grouped[domain])
	}
	return out
}

type groupedIngressBackend struct {
	Hosts     []string
	Upstreams []string
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
