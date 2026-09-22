package xds

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	routerfilter "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	cachetypes "github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	// RouteConfigName is the single RDS resource every ingress listener uses.
	RouteConfigName = "ingress_routes"

	// ClusterPrefix prefixes per-domain EDS clusters; EndpointsPrefix prefixes
	// their ClusterLoadAssignments.
	ClusterPrefix   = "backend/"
	EndpointsPrefix = "endpoints/"
)

// Counts summarizes a snapshot for publication records and status views.
type Counts struct {
	Listeners int
	Clusters  int
	Endpoints int
	Domains   int
}

// Snapshot is a fully built, consistency-checked xDS snapshot with its
// content version. The version is the hex SHA-256 of the canonical inputs,
// so two replicas computing from the same state produce identical versions.
type Snapshot struct {
	Version string
	Hash    string
	Counts  Counts
	cache   *cachev3.Snapshot
}

// CacheSnapshot returns the underlying go-control-plane snapshot for serving.
func (s *Snapshot) CacheSnapshot() *cachev3.Snapshot {
	if s == nil {
		return nil
	}
	return s.cache
}

// BuildInput is everything a snapshot derives from.
type BuildInput struct {
	Backends    []Backend
	Static      []StaticRoute
	ListenAddrs []string
}

// Build computes a versioned snapshot from control-plane state. Construction
// is fully deterministic: sorted domains, sorted endpoints, fixed resource
// names. Domains without a valid endpoint are omitted, so only ready,
// non-draining allocations are ever routed. A bad listen address fails the
// whole build (operator config error: retain last-known-good instead of
// serving nothing); malformed backends are skipped.
func Build(input BuildInput) (*Snapshot, error) {
	listeners, ports, err := buildListeners(input.ListenAddrs)
	if err != nil {
		return nil, err
	}

	grouped := GroupBackends(input.Backends)
	sort.Slice(grouped, func(i, j int) bool { return grouped[i].Hosts[0] < grouped[j].Hosts[0] })

	type domainEndpoints struct {
		domain    string
		endpoints []netip.AddrPort
	}
	var domains []domainEndpoints
	for _, group := range grouped {
		domain := group.Hosts[0]
		if sanitizeResourceName(domain) == "" {
			continue
		}
		var endpoints []netip.AddrPort
		for _, upstream := range NormalizeUpstreams(group.Upstreams) {
			addrPort, err := netip.ParseAddrPort(upstream)
			if err != nil || !addrPort.IsValid() {
				continue
			}
			endpoints = append(endpoints, addrPort)
		}
		if len(endpoints) == 0 {
			continue
		}
		domains = append(domains, domainEndpoints{domain: domain, endpoints: endpoints})
	}

	type staticEntry struct {
		hosts    []string
		upstream string
		host     string
		port     uint32
	}
	var statics []staticEntry
	for _, route := range input.Static {
		hosts := NormalizeHosts(route.Hosts)
		upstream := strings.TrimSpace(route.Upstream)
		if len(hosts) == 0 || upstream == "" {
			continue
		}
		host, portStr, err := net.SplitHostPort(upstream)
		if err != nil || strings.TrimSpace(host) == "" {
			continue
		}
		port, err := parsePort(portStr)
		if err != nil {
			continue
		}
		statics = append(statics, staticEntry{hosts: hosts, upstream: upstream, host: host, port: port})
	}
	sort.Slice(statics, func(i, j int) bool {
		if statics[i].upstream != statics[j].upstream {
			return statics[i].upstream < statics[j].upstream
		}
		return strings.Join(statics[i].hosts, ",") < strings.Join(statics[j].hosts, ",")
	})

	canonical := canonicalInput{
		Listeners: normalizedListenAddrs(input.ListenAddrs),
	}
	for _, domain := range domains {
		entry := canonicalDomain{Name: domain.domain}
		for _, addrPort := range domain.endpoints {
			entry.Endpoints = append(entry.Endpoints, addrPort.String())
		}
		canonical.Domains = append(canonical.Domains, entry)
	}
	for _, static := range statics {
		canonical.Statics = append(canonical.Statics, canonicalStatic{
			Hosts:    append([]string(nil), static.hosts...),
			Upstream: static.upstream,
		})
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical snapshot input: %w", err)
	}
	sum := sha256.Sum256(raw)
	version := hex.EncodeToString(sum[:])

	var clusters []clusterv3.Cluster
	var endpoints []*endpointv3.ClusterLoadAssignment
	var vhosts []*routev3.VirtualHost
	endpointCount := 0
	for _, domain := range domains {
		clusterName := ClusterPrefix + domain.domain
		claName := EndpointsPrefix + domain.domain
		clusters = append(clusters, edsCluster(clusterName, claName))
		cla := &endpointv3.ClusterLoadAssignment{ClusterName: claName}
		var lbEndpoints []*endpointv3.LbEndpoint
		for _, addrPort := range domain.endpoints {
			lbEndpoints = append(lbEndpoints, socketEndpoint(addrPort))
			endpointCount++
		}
		cla.Endpoints = []*endpointv3.LocalityLbEndpoints{{
			LbEndpoints: lbEndpoints,
		}}
		endpoints = append(endpoints, cla)
		vhosts = append(vhosts, virtualHost("vhost/"+domain.domain, matchDomains(domain.domain, ports), clusterName))
	}
	for i, static := range statics {
		clusterName := fmt.Sprintf("static/%d-%s", i, sanitizeResourceName(static.hosts[0]))
		clusters = append(clusters, strictDNSCluster(clusterName, static.host, static.port))
		vhosts = append(vhosts, virtualHost(
			fmt.Sprintf("static-vhost/%d-%s", i, sanitizeResourceName(static.hosts[0])),
			staticMatchDomains(static.hosts, ports),
			clusterName,
		))
	}
	sort.Slice(vhosts, func(i, j int) bool { return vhosts[i].Name < vhosts[j].Name })

	routeConfig := &routev3.RouteConfiguration{
		Name:         RouteConfigName,
		VirtualHosts: vhosts,
	}

	resources := map[resourcev3.Type][]cachetypes.Resource{}
	addResources := func(typ resourcev3.Type, items []cachetypes.Resource) {
		resources[typ] = items
	}
	listenerItems := make([]cachetypes.Resource, 0, len(listeners))
	for i := range listeners {
		listenerItems = append(listenerItems, &listeners[i])
	}
	addResources(resourcev3.ListenerType, listenerItems)
	clusterItems := make([]cachetypes.Resource, 0, len(clusters))
	for i := range clusters {
		clusterItems = append(clusterItems, &clusters[i])
	}
	addResources(resourcev3.ClusterType, clusterItems)
	routeItems := []cachetypes.Resource{routeConfig}
	addResources(resourcev3.RouteType, routeItems)
	endpointItems := make([]cachetypes.Resource, 0, len(endpoints))
	for _, cla := range endpoints {
		endpointItems = append(endpointItems, cla)
	}
	addResources(resourcev3.EndpointType, endpointItems)

	cacheSnapshot, err := cachev3.NewSnapshot(version, resources)
	if err != nil {
		return nil, fmt.Errorf("build xds snapshot: %w", err)
	}
	if err := cacheSnapshot.Consistent(); err != nil {
		return nil, fmt.Errorf("xds snapshot inconsistent: %w", err)
	}

	return &Snapshot{
		Version: version,
		Hash:    version,
		Counts: Counts{
			Listeners: len(listeners),
			Clusters:  len(clusters),
			Endpoints: endpointCount,
			Domains:   len(domains) + len(statics),
		},
		cache: cacheSnapshot,
	}, nil
}

type canonicalDomain struct {
	Name      string   `json:"name"`
	Endpoints []string `json:"endpoints"`
}

type canonicalStatic struct {
	Hosts    []string `json:"hosts"`
	Upstream string   `json:"upstream"`
}

type canonicalInput struct {
	Domains   []canonicalDomain `json:"domains"`
	Statics   []canonicalStatic `json:"statics"`
	Listeners []string          `json:"listeners"`
}

func normalizedListenAddrs(addrs []string) []string {
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if strings.TrimSpace(addr) == "" {
			continue
		}
		out = append(out, strings.TrimSpace(addr))
	}
	sort.Strings(out)
	return out
}

func buildListeners(addrs []string) ([]listenerv3.Listener, []uint32, error) {
	normalized := normalizedListenAddrs(addrs)
	if len(normalized) == 0 {
		return nil, nil, fmt.Errorf("xds requires at least one ingress listen address")
	}
	var listeners []listenerv3.Listener
	var ports []uint32
	seen := make(map[string]struct{})
	for _, addr := range normalized {
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		host, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid ingress listen address %q: %w", addr, err)
		}
		if strings.TrimSpace(host) == "" {
			host = "0.0.0.0"
		}
		if ip := net.ParseIP(strings.Trim(host, "[]")); ip == nil {
			return nil, nil, fmt.Errorf("invalid ingress listen address %q: host must be an IP", addr)
		}
		host = strings.Trim(host, "[]")
		port, err := parsePort(portStr)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid ingress listen address %q: %w", addr, err)
		}
		manager, err := anypb.New(httpConnectionManager())
		if err != nil {
			return nil, nil, fmt.Errorf("marshal http connection manager: %w", err)
		}
		listeners = append(listeners, listenerv3.Listener{
			Name: "ingress-http-" + sanitizeResourceName(addr),
			Address: &corev3.Address{
				Address: &corev3.Address_SocketAddress{
					SocketAddress: &corev3.SocketAddress{
						Protocol: corev3.SocketAddress_TCP,
						Address:  host,
						PortSpecifier: &corev3.SocketAddress_PortValue{
							PortValue: port,
						},
					},
				},
			},
			FilterChains: []*listenerv3.FilterChain{{
				Filters: []*listenerv3.Filter{{
					Name: wellknown.HTTPConnectionManager,
					ConfigType: &listenerv3.Filter_TypedConfig{
						TypedConfig: manager,
					},
				}},
			}},
		})
		ports = append(ports, port)
	}
	return listeners, ports, nil
}

func parsePort(raw string) (uint32, error) {
	port, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("port %q must be between 1 and 65535", raw)
	}
	return uint32(port), nil
}

func httpConnectionManager() *hcm.HttpConnectionManager {
	routerConfig, err := anypb.New(&routerfilter.Router{})
	if err != nil {
		panic(fmt.Sprintf("marshal router filter: %v", err))
	}
	return &hcm.HttpConnectionManager{
		CodecType:  hcm.HttpConnectionManager_AUTO,
		StatPrefix: "ingress_http",
		RouteSpecifier: &hcm.HttpConnectionManager_Rds{
			Rds: &hcm.Rds{
				ConfigSource:    adsConfigSource(),
				RouteConfigName: RouteConfigName,
			},
		},
		HttpFilters: []*hcm.HttpFilter{{
			Name:       "http-router",
			ConfigType: &hcm.HttpFilter_TypedConfig{TypedConfig: routerConfig},
		}},
		UpgradeConfigs: []*hcm.HttpConnectionManager_UpgradeConfig{{
			UpgradeType: "websocket",
		}},
	}
}

func adsConfigSource() *corev3.ConfigSource {
	return &corev3.ConfigSource{
		ResourceApiVersion: resourcev3.DefaultAPIVersion,
		ConfigSourceSpecifier: &corev3.ConfigSource_Ads{
			Ads: &corev3.AggregatedConfigSource{},
		},
	}
}

func edsCluster(name, claName string) clusterv3.Cluster {
	return clusterv3.Cluster{
		Name:           name,
		ConnectTimeout: durationpb.New(5 * time.Second),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_EDS,
		},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			ServiceName: claName,
			EdsConfig:   adsConfigSource(),
		},
		LbPolicy: clusterv3.Cluster_ROUND_ROBIN,
	}
}

func strictDNSCluster(name, host string, port uint32) clusterv3.Cluster {
	return clusterv3.Cluster{
		Name:           name,
		ConnectTimeout: durationpb.New(5 * time.Second),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_STRICT_DNS,
		},
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*endpointv3.LocalityLbEndpoints{{
				LbEndpoints: []*endpointv3.LbEndpoint{{
					HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
						Endpoint: &endpointv3.Endpoint{
							Address: &corev3.Address{
								Address: &corev3.Address_SocketAddress{
									SocketAddress: &corev3.SocketAddress{
										Address: host,
										PortSpecifier: &corev3.SocketAddress_PortValue{
											PortValue: port,
										},
									},
								},
							},
						},
					},
				}},
			}},
		},
		LbPolicy: clusterv3.Cluster_ROUND_ROBIN,
	}
}

func socketEndpoint(addrPort netip.AddrPort) *endpointv3.LbEndpoint {
	return &endpointv3.LbEndpoint{
		HealthStatus: corev3.HealthStatus_HEALTHY,
		HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
			Endpoint: &endpointv3.Endpoint{
				Address: &corev3.Address{
					Address: &corev3.Address_SocketAddress{
						SocketAddress: &corev3.SocketAddress{
							Protocol: corev3.SocketAddress_TCP,
							Address:  addrPort.Addr().String(),
							PortSpecifier: &corev3.SocketAddress_PortValue{
								PortValue: uint32(addrPort.Port()),
							},
						},
					},
				},
			},
		},
	}
}

func virtualHost(name string, domains []string, cluster string) *routev3.VirtualHost {
	return &routev3.VirtualHost{
		Name:    name,
		Domains: domains,
		Routes: []*routev3.Route{{
			Match: &routev3.RouteMatch{
				PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"},
			},
			Action: &routev3.Route_Route{
				Route: &routev3.RouteAction{
					ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: cluster},
					// No L7 timeout policy yet; a 15s default would break
					// long-lived responses the previous proxy served fine.
					Timeout: durationpb.New(0),
				},
			},
		}},
	}
}

// matchDomains matches the bare hostname plus explicit host:port forms for
// every listener port. Envoy does not strip ports from Host headers itself,
// and browsers send the port on non-default listeners.
func matchDomains(domain string, ports []uint32) []string {
	seen := map[string]struct{}{domain: {}}
	out := []string{domain}
	for _, port := range ports {
		candidate := net.JoinHostPort(domain, strconv.FormatUint(uint64(port), 10))
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}
	sort.Strings(out[1:])
	return out
}

func staticMatchDomains(hosts []string, ports []uint32) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, host := range hosts {
		for _, candidate := range matchDomains(host, ports) {
			if _, ok := seen[candidate]; ok {
				continue
			}
			seen[candidate] = struct{}{}
			out = append(out, candidate)
		}
	}
	sort.Strings(out)
	return out
}

func sanitizeResourceName(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
