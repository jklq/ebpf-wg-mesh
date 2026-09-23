package xds

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"slices"
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
	RouteConfigName = "ingress_routes"
	ClusterPrefix   = "backend/"
	EndpointsPrefix = "endpoints/"
)

type Counts struct {
	Listeners int
	Clusters  int
	Endpoints int
	Domains   int
}

type Snapshot struct {
	Version string
	Counts  Counts
	// Inputs is the canonical hash preimage stored so any replica can
	// rebuild this exact snapshot with BuildFromInputs.
	Inputs []byte
	cache  *cachev3.Snapshot
}

func (s *Snapshot) CacheSnapshot() *cachev3.Snapshot {
	if s == nil {
		return nil
	}
	return s.cache
}

type BuildInput struct {
	Backends    []Backend
	Static      []StaticRoute
	ListenAddrs []string
}

// RequiredTypes reports the xDS types whose ACK the drain barrier requires.
// CDS, LDS, and RDS always have standing Envoy subscriptions. EDS
// subscriptions exist only for the EDS clusters CDS announces: with no
// endpoints the dynamic clusters are gone from CDS, so requiring an EDS ACK
// would wait forever.
func (s *Snapshot) RequiredTypes() []string {
	types := []string{resourcev3.ListenerType, resourcev3.ClusterType, resourcev3.RouteType}
	if s != nil && s.cache != nil {
		if items := s.cache.Resources[cachev3.GetResponseType(resourcev3.EndpointType)].Items; len(items) > 0 {
			types = append(types, resourcev3.EndpointType)
		}
	}
	return types
}

// Build computes a versioned snapshot from control-plane state. The version
// is the hex SHA-256 of the canonical inputs. A bad listen address fails the
// whole build; malformed backends are skipped.
func Build(input BuildInput) (*Snapshot, error) {
	canonical, err := canonicalize(input)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical snapshot input: %w", err)
	}
	return buildCanonical(canonical, raw)
}

// BuildFromInputs rebuilds the exact snapshot from its canonical inputs.
// Inputs from storage are treated as untrusted: anything malformed fails closed.
func BuildFromInputs(raw []byte) (*Snapshot, error) {
	var canonical canonicalInput
	if err := json.Unmarshal(raw, &canonical); err != nil {
		return nil, fmt.Errorf("unmarshal canonical snapshot input: %w", err)
	}
	return buildCanonical(canonical, raw)
}

type domainEndpoints struct {
	domain    string
	endpoints []netip.AddrPort
}

type staticEntry struct {
	hosts    []string
	upstream string
	host     string
	port     uint32
}

func canonicalize(input BuildInput) (canonicalInput, error) {
	byDomain := map[string][]string{}
	for _, b := range input.Backends {
		domain := strings.ToLower(strings.TrimSpace(b.Domain))
		if domain == "" {
			continue
		}
		byDomain[domain] = append(byDomain[domain], b.Upstream)
	}
	domainKeys := make([]string, 0, len(byDomain))
	for domain := range byDomain {
		domainKeys = append(domainKeys, domain)
	}
	sort.Strings(domainKeys)

	var domains []domainEndpoints
	for _, domain := range domainKeys {
		if sanitizeResourceName(domain) == "" {
			continue
		}
		var endpoints []netip.AddrPort
		for _, upstream := range normalizeUpstreams(byDomain[domain]) {
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

	var statics []staticEntry
	for _, route := range input.Static {
		hosts := normalizeHosts(route.Hosts)
		upstream := strings.TrimSpace(route.Upstream)
		if len(hosts) == 0 || upstream == "" {
			continue
		}
		host, portStr, err := net.SplitHostPort(upstream)
		if err != nil || strings.TrimSpace(host) == "" {
			continue
		}
		if _, err := parsePort(portStr); err != nil {
			continue
		}
		statics = append(statics, staticEntry{hosts: hosts, upstream: upstream})
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
	return canonical, nil
}

func buildCanonical(canonical canonicalInput, raw []byte) (*Snapshot, error) {
	listeners, ports, err := buildListeners(canonical.Listeners)
	if err != nil {
		return nil, err
	}

	domains := make([]domainEndpoints, 0, len(canonical.Domains))
	for _, entry := range canonical.Domains {
		item := domainEndpoints{domain: entry.Name}
		for _, raw := range entry.Endpoints {
			addrPort, err := netip.ParseAddrPort(raw)
			if err != nil || !addrPort.IsValid() {
				return nil, fmt.Errorf("snapshot input endpoint %q is invalid", raw)
			}
			item.endpoints = append(item.endpoints, addrPort)
		}
		domains = append(domains, item)
	}
	statics := make([]staticEntry, 0, len(canonical.Statics))
	for _, entry := range canonical.Statics {
		host, portStr, err := net.SplitHostPort(entry.Upstream)
		if err != nil || strings.TrimSpace(host) == "" {
			return nil, fmt.Errorf("snapshot input upstream %q is invalid", entry.Upstream)
		}
		port, err := parsePort(portStr)
		if err != nil {
			return nil, err
		}
		statics = append(statics, staticEntry{hosts: entry.Hosts, upstream: entry.Upstream, host: host, port: port})
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

	listenerItems := make([]cachetypes.Resource, 0, len(listeners))
	for i := range listeners {
		listenerItems = append(listenerItems, &listeners[i])
	}
	clusterItems := make([]cachetypes.Resource, 0, len(clusters))
	for i := range clusters {
		clusterItems = append(clusterItems, &clusters[i])
	}
	endpointItems := make([]cachetypes.Resource, 0, len(endpoints))
	for _, cla := range endpoints {
		endpointItems = append(endpointItems, cla)
	}
	resources := map[resourcev3.Type][]cachetypes.Resource{
		resourcev3.ListenerType: listenerItems,
		resourcev3.ClusterType:  clusterItems,
		resourcev3.RouteType:    {routeConfig},
		resourcev3.EndpointType: endpointItems,
	}

	cacheSnapshot, err := cachev3.NewSnapshot(version, resources)
	if err != nil {
		return nil, fmt.Errorf("build xds snapshot: %w", err)
	}
	if err := cacheSnapshot.Consistent(); err != nil {
		return nil, fmt.Errorf("xds snapshot inconsistent: %w", err)
	}

	return &Snapshot{
		Version: version,
		Inputs:  append([]byte(nil), raw...),
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

func normalizeUpstreams(upstreams []string) []string {
	out := make([]string, 0, len(upstreams))
	seen := make(map[string]struct{}, len(upstreams))
	for _, upstream := range upstreams {
		upstream = strings.TrimSpace(upstream)
		if upstream == "" {
			continue
		}
		if _, ok := seen[upstream]; ok {
			continue
		}
		seen[upstream] = struct{}{}
		out = append(out, upstream)
	}
	slices.Sort(out)
	return out
}

func normalizeHosts(hosts []string) []string {
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

func buildListeners(addrs []string) ([]listenerv3.Listener, []uint32, error) {
	if len(addrs) == 0 {
		return nil, nil, fmt.Errorf("xds requires at least one ingress listen address")
	}
	var listeners []listenerv3.Listener
	var ports []uint32
	seen := make(map[string]struct{})
	for _, addr := range addrs {
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
					// No L7 timeout policy: a 15s default would break
					// long-lived responses.
					Timeout: durationpb.New(0),
				},
			},
		}},
	}
}

// matchDomains matches the bare hostname plus explicit host:port forms for
// every listener port. Envoy does not strip ports from Host headers itself.
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
