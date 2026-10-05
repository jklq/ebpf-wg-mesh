package xds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
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
	tlsinspector "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/tls_inspector/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	cachetypes "github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	HTTPRouteConfigName  = "ingress_http"
	HTTPSRouteConfigName = "ingress_https"
	ClusterPrefix        = "backend/"
	EndpointsPrefix      = "endpoints/"
	SecretPrefix         = "tls/"
	// ACMEChallengePathPrefix is the HTTP-01 path that Envoy answers from the snapshot.
	ACMEChallengePathPrefix = "/.well-known/acme-challenge/"
)

type Counts struct {
	Listeners    int
	Clusters     int
	Endpoints    int
	Domains      int
	Certificates int
	Challenges   int
}

type Snapshot struct {
	Version string
	Counts  Counts
	// Inputs is the canonical hash preimage, so any replica can rebuild this exact snapshot.
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
	Backends []Backend
	Static   []StaticRoute
	// HTTPListenAddrs serve challenges, redirects, and hostnames without a certificate.
	HTTPListenAddrs []string
	// HTTPSListenAddrs terminate TLS. They exist in the snapshot only while it holds a certificate.
	HTTPSListenAddrs []string
	Certificates     []Certificate
	Challenges       []Challenge
	// Keys resolves certificate key pairs. A certificate whose key pair does not
	// resolve is left out, so one bad certificate cannot block publication.
	Keys KeyPairSource
}

// RequiredTypes reports the xDS types whose ACK the drain barrier requires. CDS, LDS, and RDS
// always have standing subscriptions; EDS and SDS subscriptions exist only for announced
// clusters and listener filter chains, so without them no ACK is required (it would wait forever).
func (s *Snapshot) RequiredTypes() []string {
	types := []string{resourcev3.ListenerType, resourcev3.ClusterType, resourcev3.RouteType}
	if s != nil && s.cache != nil {
		if items := s.cache.Resources[cachev3.GetResponseType(resourcev3.EndpointType)].Items; len(items) > 0 {
			types = append(types, resourcev3.EndpointType)
		}
		if items := s.cache.Resources[cachev3.GetResponseType(resourcev3.SecretType)].Items; len(items) > 0 {
			types = append(types, resourcev3.SecretType)
		}
	}
	return types
}

// Build computes a versioned snapshot from control-plane state. The version is the hex SHA-256
// of the canonical inputs. A bad listen address fails the build; malformed backends, challenges,
// and certificates without a usable key pair are skipped.
func Build(ctx context.Context, input BuildInput) (*Snapshot, error) {
	keys, usable := resolveKeyPairs(ctx, input.Keys, input.Certificates)
	input.Certificates = usable
	canonical, err := canonicalize(input)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical snapshot input: %w", err)
	}
	return buildCanonical(canonical, raw, keys)
}

// BuildFromInputs rebuilds from canonical inputs; malformed stored inputs and
// unresolvable key pairs fail closed.
func BuildFromInputs(ctx context.Context, raw []byte, source KeyPairSource) (*Snapshot, error) {
	var canonical canonicalInput
	if err := json.Unmarshal(raw, &canonical); err != nil {
		return nil, fmt.Errorf("unmarshal canonical snapshot input: %w", err)
	}
	keys := make(map[string]KeyPair, len(canonical.Certificates))
	for _, cert := range canonical.Certificates {
		if source == nil {
			return nil, fmt.Errorf("snapshot input certificate %q needs a key pair source", cert.Name)
		}
		pair, err := source.KeyPair(ctx, cert.Fingerprint)
		if err != nil {
			return nil, fmt.Errorf("load key pair for certificate %q: %w", cert.Name, err)
		}
		keys[cert.Fingerprint] = pair
	}
	return buildCanonical(canonical, raw, keys)
}

func resolveKeyPairs(ctx context.Context, source KeyPairSource, certs []Certificate) (map[string]KeyPair, []Certificate) {
	keys := make(map[string]KeyPair, len(certs))
	usable := make([]Certificate, 0, len(certs))
	for _, cert := range certs {
		fingerprint := strings.ToLower(strings.TrimSpace(cert.Fingerprint))
		if fingerprint == "" || source == nil {
			continue
		}
		if _, ok := keys[fingerprint]; !ok {
			pair, err := source.KeyPair(ctx, fingerprint)
			if err != nil {
				slog.Warn("xds certificate skipped: key pair unavailable", "certificate", cert.Name, "error", err)
				continue
			}
			keys[fingerprint] = pair
		}
		usable = append(usable, cert)
	}
	return keys, usable
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
		HTTPListeners:  normalizedListenAddrs(input.HTTPListenAddrs),
		HTTPSListeners: normalizedListenAddrs(input.HTTPSListenAddrs),
		Certificates:   canonicalCertificates(input.Certificates),
		Challenges:     canonicalChallenges(input.Challenges),
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

func buildCanonical(canonical canonicalInput, raw []byte, keys map[string]KeyPair) (*Snapshot, error) {
	httpPorts, err := listenPorts(canonical.HTTPListeners)
	if err != nil {
		return nil, err
	}
	if len(httpPorts) == 0 {
		return nil, fmt.Errorf("xds requires at least one HTTP ingress listen address")
	}
	httpsPorts, err := listenPorts(canonical.HTTPSListeners)
	if err != nil {
		return nil, err
	}
	// HTTPS listeners need at least one filter chain, so they exist only with a certificate.
	tlsEnabled := len(httpsPorts) > 0 && len(canonical.Certificates) > 0
	if !tlsEnabled {
		httpsPorts = nil
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
	var secrets []*tlsv3.Secret
	for _, cert := range canonical.Certificates {
		pair, ok := keys[cert.Fingerprint]
		if !ok {
			return nil, fmt.Errorf("snapshot input certificate %q has no key pair", cert.Name)
		}
		if tlsEnabled {
			secrets = append(secrets, tlsSecret(cert.Name, pair))
		}
	}
	for _, challenge := range canonical.Challenges {
		if !validChallengeValue(challenge.Token) || !validChallengeValue(challenge.KeyAuthorization) {
			return nil, fmt.Errorf("snapshot input challenge for %q is invalid", challenge.Hostname)
		}
	}

	sum := sha256.Sum256(raw)
	version := hex.EncodeToString(sum[:])

	routes := newRouteSet(canonical, tlsEnabled, httpPorts, httpsPorts)
	var clusters []clusterv3.Cluster
	var endpoints []*endpointv3.ClusterLoadAssignment
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
		routes.add("vhost/"+domain.domain, []string{domain.domain}, clusterName)
	}
	for i, static := range statics {
		clusterName := fmt.Sprintf("static/%d-%s", i, sanitizeResourceName(static.hosts[0]))
		clusters = append(clusters, strictDNSCluster(clusterName, static.host, static.port))
		routes.add(fmt.Sprintf("static-vhost/%d-%s", i, sanitizeResourceName(static.hosts[0])), static.hosts, clusterName)
	}
	routeConfigs := routes.configs()

	listeners, err := buildListeners(canonical.HTTPListeners, canonical.HTTPSListeners, canonical.Certificates, tlsEnabled)
	if err != nil {
		return nil, err
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
	routeItems := make([]cachetypes.Resource, 0, len(routeConfigs))
	for _, config := range routeConfigs {
		routeItems = append(routeItems, config)
	}
	secretItems := make([]cachetypes.Resource, 0, len(secrets))
	for _, secret := range secrets {
		secretItems = append(secretItems, secret)
	}
	resources := map[resourcev3.Type][]cachetypes.Resource{
		resourcev3.ListenerType: listenerItems,
		resourcev3.ClusterType:  clusterItems,
		resourcev3.RouteType:    routeItems,
		resourcev3.EndpointType: endpointItems,
		resourcev3.SecretType:   secretItems,
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
			Listeners:    len(listeners),
			Clusters:     len(clusters),
			Endpoints:    endpointCount,
			Domains:      len(domains) + len(statics),
			Certificates: len(secrets),
			Challenges:   len(canonical.Challenges),
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

type canonicalCertificate struct {
	Name        string   `json:"name"`
	ServerNames []string `json:"server_names"`
	Fingerprint string   `json:"fingerprint"`
}

type canonicalChallenge struct {
	Hostname         string `json:"hostname"`
	Token            string `json:"token"`
	KeyAuthorization string `json:"key_authorization"`
}

type canonicalInput struct {
	Domains        []canonicalDomain      `json:"domains"`
	Statics        []canonicalStatic      `json:"statics"`
	HTTPListeners  []string               `json:"http_listeners"`
	HTTPSListeners []string               `json:"https_listeners"`
	Certificates   []canonicalCertificate `json:"certificates"`
	Challenges     []canonicalChallenge   `json:"challenges"`
}

// canonicalCertificates sorts certificates by name and gives each server name to
// one certificate only: Envoy refuses two filter chains with the same match.
func canonicalCertificates(certs []Certificate) []canonicalCertificate {
	byName := map[string]canonicalCertificate{}
	for _, cert := range certs {
		name := strings.ToLower(strings.TrimSpace(cert.Name))
		fingerprint := strings.ToLower(strings.TrimSpace(cert.Fingerprint))
		serverNames := normalizeHosts(cert.ServerNames)
		if name == "" || fingerprint == "" || len(serverNames) == 0 || sanitizeResourceName(name) == "" {
			continue
		}
		if _, ok := byName[name]; ok {
			continue
		}
		byName[name] = canonicalCertificate{Name: name, ServerNames: serverNames, Fingerprint: fingerprint}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	claimed := map[string]struct{}{}
	out := make([]canonicalCertificate, 0, len(names))
	for _, name := range names {
		cert := byName[name]
		var serverNames []string
		for _, serverName := range cert.ServerNames {
			if _, ok := claimed[serverName]; ok {
				continue
			}
			claimed[serverName] = struct{}{}
			serverNames = append(serverNames, serverName)
		}
		if len(serverNames) == 0 {
			continue
		}
		cert.ServerNames = serverNames
		out = append(out, cert)
	}
	return out
}

func canonicalChallenges(challenges []Challenge) []canonicalChallenge {
	seen := map[string]struct{}{}
	var out []canonicalChallenge
	for _, challenge := range challenges {
		hostname := strings.ToLower(strings.TrimSpace(challenge.Hostname))
		token := strings.TrimSpace(challenge.Token)
		keyAuthorization := strings.TrimSpace(challenge.KeyAuthorization)
		if hostname == "" || !validChallengeValue(token) || !validChallengeValue(keyAuthorization) {
			continue
		}
		key := hostname + "\x00" + token
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, canonicalChallenge{Hostname: hostname, Token: token, KeyAuthorization: keyAuthorization})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hostname != out[j].Hostname {
			return out[i].Hostname < out[j].Hostname
		}
		return out[i].Token < out[j].Token
	})
	return out
}

// validChallengeValue accepts the base64url alphabet plus the "." that joins a
// key authorization. Anything else could escape the challenge path.
func validChallengeValue(raw string) bool {
	if raw == "" || len(raw) > 512 {
		return false
	}
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// covers reports whether a certificate server name matches hostname. A wildcard
// matches exactly one label, as in RFC 6125.
func covers(serverName, hostname string) bool {
	if serverName == hostname {
		return true
	}
	suffix, ok := strings.CutPrefix(serverName, "*.")
	if !ok {
		return false
	}
	label, rest, found := strings.Cut(hostname, ".")
	return found && label != "" && rest == suffix
}

// routeSet builds the HTTP and HTTPS route configurations. A hostname with a
// certificate redirects from HTTP to HTTPS; every hostname answers its pending
// ACME challenges over HTTP, also before it has backends.
type routeSet struct {
	tlsEnabled  bool
	httpPorts   []uint32
	httpsPorts  []uint32
	serverNames []string
	challenges  map[string][]canonicalChallenge
	routed      map[string]struct{}
	http        []*routev3.VirtualHost
	https       []*routev3.VirtualHost
}

func newRouteSet(canonical canonicalInput, tlsEnabled bool, httpPorts, httpsPorts []uint32) *routeSet {
	set := &routeSet{
		tlsEnabled: tlsEnabled,
		httpPorts:  httpPorts,
		httpsPorts: httpsPorts,
		challenges: map[string][]canonicalChallenge{},
		routed:     map[string]struct{}{},
	}
	if tlsEnabled {
		for _, cert := range canonical.Certificates {
			set.serverNames = append(set.serverNames, cert.ServerNames...)
		}
	}
	for _, challenge := range canonical.Challenges {
		set.challenges[challenge.Hostname] = append(set.challenges[challenge.Hostname], challenge)
	}
	return set
}

func (r *routeSet) secured(host string) bool {
	for _, serverName := range r.serverNames {
		if covers(serverName, host) {
			return true
		}
	}
	return false
}

func (r *routeSet) add(name string, hosts []string, cluster string) {
	var secured, plain []string
	for _, host := range hosts {
		if _, ok := r.routed[host]; ok {
			continue
		}
		r.routed[host] = struct{}{}
		if r.secured(host) {
			secured = append(secured, host)
		} else {
			plain = append(plain, host)
		}
	}
	if len(secured) > 0 {
		r.http = append(r.http, r.httpVirtualHost(name+"/redirect", secured, redirectRoute(r.httpsPorts[0])))
		r.https = append(r.https, &routev3.VirtualHost{
			Name:    name,
			Domains: hostMatchDomains(secured, r.httpsPorts),
			Routes:  []*routev3.Route{clusterRoute(cluster)},
		})
	}
	if len(plain) > 0 {
		r.http = append(r.http, r.httpVirtualHost(name, plain, clusterRoute(cluster)))
	}
}

func (r *routeSet) httpVirtualHost(name string, hosts []string, fallback *routev3.Route) *routev3.VirtualHost {
	var routes []*routev3.Route
	for _, host := range hosts {
		for _, challenge := range r.challenges[host] {
			routes = append(routes, challengeRoute(challenge))
		}
	}
	if fallback != nil {
		routes = append(routes, fallback)
	}
	return &routev3.VirtualHost{Name: name, Domains: hostMatchDomains(hosts, r.httpPorts), Routes: routes}
}

func (r *routeSet) configs() []*routev3.RouteConfiguration {
	var pending []string
	for host := range r.challenges {
		if _, ok := r.routed[host]; !ok {
			pending = append(pending, host)
		}
	}
	sort.Strings(pending)
	for _, host := range pending {
		r.http = append(r.http, r.httpVirtualHost("acme/"+host, []string{host}, nil))
	}
	sort.Slice(r.http, func(i, j int) bool { return r.http[i].Name < r.http[j].Name })
	configs := []*routev3.RouteConfiguration{{Name: HTTPRouteConfigName, VirtualHosts: r.http}}
	if r.tlsEnabled {
		sort.Slice(r.https, func(i, j int) bool { return r.https[i].Name < r.https[j].Name })
		configs = append(configs, &routev3.RouteConfiguration{Name: HTTPSRouteConfigName, VirtualHosts: r.https})
	}
	return configs
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

func listenPorts(addrs []string) ([]uint32, error) {
	var ports []uint32
	for _, addr := range addrs {
		_, port, err := listenSocket(addr)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(ports, port) {
			ports = append(ports, port)
		}
	}
	return ports, nil
}

func listenSocket(addr string) (string, uint32, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid ingress listen address %q: %w", addr, err)
	}
	if strings.TrimSpace(host) == "" {
		host = "0.0.0.0"
	}
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip == nil {
		return "", 0, fmt.Errorf("invalid ingress listen address %q: host must be an IP", addr)
	}
	port, err := parsePort(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid ingress listen address %q: %w", addr, err)
	}
	return host, port, nil
}

func buildListeners(httpAddrs, httpsAddrs []string, certs []canonicalCertificate, tlsEnabled bool) ([]listenerv3.Listener, error) {
	var listeners []listenerv3.Listener
	seen := make(map[string]struct{})
	for _, addr := range httpAddrs {
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		host, port, err := listenSocket(addr)
		if err != nil {
			return nil, err
		}
		chain, err := httpFilterChain(HTTPRouteConfigName, "ingress_http")
		if err != nil {
			return nil, err
		}
		listeners = append(listeners, listenerv3.Listener{
			Name:         "ingress-http-" + sanitizeResourceName(addr),
			Address:      socketAddress(host, port),
			FilterChains: []*listenerv3.FilterChain{chain},
		})
	}
	if !tlsEnabled {
		return listeners, nil
	}
	inspector, err := anypb.New(&tlsinspector.TlsInspector{})
	if err != nil {
		return nil, fmt.Errorf("marshal tls inspector: %w", err)
	}
	for _, addr := range httpsAddrs {
		if _, ok := seen[addr]; ok {
			return nil, fmt.Errorf("ingress listen address %q is both HTTP and HTTPS", addr)
		}
		seen[addr] = struct{}{}
		host, port, err := listenSocket(addr)
		if err != nil {
			return nil, err
		}
		chains := make([]*listenerv3.FilterChain, 0, len(certs))
		for _, cert := range certs {
			chain, err := httpFilterChain(HTTPSRouteConfigName, "ingress_https")
			if err != nil {
				return nil, err
			}
			transport, err := downstreamTLS(cert.Name)
			if err != nil {
				return nil, err
			}
			chain.Name = "tls/" + sanitizeResourceName(cert.Name)
			chain.FilterChainMatch = &listenerv3.FilterChainMatch{ServerNames: cert.ServerNames}
			chain.TransportSocket = transport
			chains = append(chains, chain)
		}
		listeners = append(listeners, listenerv3.Listener{
			Name:    "ingress-https-" + sanitizeResourceName(addr),
			Address: socketAddress(host, port),
			ListenerFilters: []*listenerv3.ListenerFilter{{
				Name:       wellknown.TlsInspector,
				ConfigType: &listenerv3.ListenerFilter_TypedConfig{TypedConfig: inspector},
			}},
			FilterChains: chains,
		})
	}
	return listeners, nil
}

func socketAddress(host string, port uint32) *corev3.Address {
	return &corev3.Address{
		Address: &corev3.Address_SocketAddress{
			SocketAddress: &corev3.SocketAddress{
				Protocol:      corev3.SocketAddress_TCP,
				Address:       host,
				PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: port},
			},
		},
	}
}

func httpFilterChain(routeConfig, statPrefix string) (*listenerv3.FilterChain, error) {
	manager, err := anypb.New(httpConnectionManager(routeConfig, statPrefix))
	if err != nil {
		return nil, fmt.Errorf("marshal http connection manager: %w", err)
	}
	return &listenerv3.FilterChain{
		Filters: []*listenerv3.Filter{{
			Name:       wellknown.HTTPConnectionManager,
			ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: manager},
		}},
	}, nil
}

func downstreamTLS(certName string) (*corev3.TransportSocket, error) {
	context, err := anypb.New(&tlsv3.DownstreamTlsContext{
		CommonTlsContext: &tlsv3.CommonTlsContext{
			TlsParams: &tlsv3.TlsParameters{
				TlsMinimumProtocolVersion: tlsv3.TlsParameters_TLSv1_2,
			},
			AlpnProtocols: []string{"h2", "http/1.1"},
			TlsCertificateSdsSecretConfigs: []*tlsv3.SdsSecretConfig{{
				Name:      SecretPrefix + certName,
				SdsConfig: adsConfigSource(),
			}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal downstream tls context: %w", err)
	}
	return &corev3.TransportSocket{
		Name:       wellknown.TransportSocketTls,
		ConfigType: &corev3.TransportSocket_TypedConfig{TypedConfig: context},
	}, nil
}

func tlsSecret(certName string, pair KeyPair) *tlsv3.Secret {
	return &tlsv3.Secret{
		Name: SecretPrefix + certName,
		Type: &tlsv3.Secret_TlsCertificate{TlsCertificate: &tlsv3.TlsCertificate{
			CertificateChain: &corev3.DataSource{Specifier: &corev3.DataSource_InlineBytes{InlineBytes: pair.CertificatePEM}},
			PrivateKey:       &corev3.DataSource{Specifier: &corev3.DataSource_InlineBytes{InlineBytes: pair.PrivateKeyPEM}},
		}},
	}
}

func parsePort(raw string) (uint32, error) {
	port, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("port %q must be between 1 and 65535", raw)
	}
	return uint32(port), nil
}

func httpConnectionManager(routeConfig, statPrefix string) *hcm.HttpConnectionManager {
	routerConfig, err := anypb.New(&routerfilter.Router{})
	if err != nil {
		panic(fmt.Sprintf("marshal router filter: %v", err))
	}
	return &hcm.HttpConnectionManager{
		CodecType:  hcm.HttpConnectionManager_AUTO,
		StatPrefix: statPrefix,
		RouteSpecifier: &hcm.HttpConnectionManager_Rds{
			Rds: &hcm.Rds{
				ConfigSource:    adsConfigSource(),
				RouteConfigName: routeConfig,
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

func clusterRoute(cluster string) *routev3.Route {
	return &routev3.Route{
		Match: &routev3.RouteMatch{
			PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"},
		},
		Action: &routev3.Route_Route{
			Route: &routev3.RouteAction{
				ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: cluster},
				// No L7 timeout: a 15s default would break long-lived responses.
				Timeout: durationpb.New(0),
			},
		},
	}
}

// redirectRoute sends plaintext requests to HTTPS. 308 keeps the method and body.
func redirectRoute(httpsPort uint32) *routev3.Route {
	redirect := &routev3.RedirectAction{
		SchemeRewriteSpecifier: &routev3.RedirectAction_HttpsRedirect{HttpsRedirect: true},
		ResponseCode:           routev3.RedirectAction_PERMANENT_REDIRECT,
	}
	if httpsPort != 443 {
		redirect.PortRedirect = httpsPort
	}
	return &routev3.Route{
		Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
		Action: &routev3.Route_Redirect{Redirect: redirect},
	}
}

func challengeRoute(challenge canonicalChallenge) *routev3.Route {
	return &routev3.Route{
		Match: &routev3.RouteMatch{
			PathSpecifier: &routev3.RouteMatch_Path{Path: ACMEChallengePathPrefix + challenge.Token},
		},
		Action: &routev3.Route_DirectResponse{DirectResponse: &routev3.DirectResponseAction{
			Status: 200,
			Body:   &corev3.DataSource{Specifier: &corev3.DataSource_InlineString{InlineString: challenge.KeyAuthorization}},
		}},
		ResponseHeadersToAdd: []*corev3.HeaderValueOption{{
			Header:       &corev3.HeaderValue{Key: "content-type", Value: "text/plain"},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		}},
	}
}

// matchDomains matches bare hostnames plus host:port forms; Envoy doesn't strip ports itself.
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

func hostMatchDomains(hosts []string, ports []uint32) []string {
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
