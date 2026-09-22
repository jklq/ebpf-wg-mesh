// Package xds is the control-plane xDS authority for the Envoy ingress data
// plane. Snapshot computation is a deterministic function of control-plane
// state: replicas racing to compute it produce identical bytes and therefore
// an identical content version, so split ownership converges instead of
// flapping. Snapshots publish atomically across LDS/CDS/EDS/RDS; Envoy
// ACK/NACK is the apply protocol and a NACK never withdraws the last
// published snapshot.
package xds

import (
	"slices"
	"strings"
)

// Backend is one routable workload endpoint behind a domain.
type Backend struct {
	Domain       string
	Upstream     string
	AllocationID string
}

// StaticRoute routes fixed hostnames to an operator-provided upstream
// (dashboards, consoles) ahead of dynamic workload backends.
type StaticRoute struct {
	Hosts    []string
	Upstream string
}

// GroupedBackend is one domain with every healthy upstream behind it,
// in deterministic order.
type GroupedBackend struct {
	Hosts     []string
	Upstreams []string
}

// GroupBackends groups backends by normalized domain in first-seen order.
func GroupBackends(backends []Backend) []GroupedBackend {
	order := make([]string, 0, len(backends))
	grouped := make(map[string]*GroupedBackend, len(backends))
	for _, backend := range backends {
		domain := strings.ToLower(strings.TrimSpace(backend.Domain))
		if domain == "" {
			continue
		}
		entry, ok := grouped[domain]
		if !ok {
			entry = &GroupedBackend{Hosts: []string{domain}}
			grouped[domain] = entry
			order = append(order, domain)
		}
		entry.Upstreams = append(entry.Upstreams, backend.Upstream)
	}
	out := make([]GroupedBackend, 0, len(order))
	for _, domain := range order {
		out = append(out, *grouped[domain])
	}
	return out
}

// NormalizeHosts lowercases, dedupes, and sorts hostnames so snapshots built
// from the same inputs always compare equal.
func NormalizeHosts(hosts []string) []string {
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

// NormalizeUpstreams trims, dedupes, and sorts upstreams for the same reason.
func NormalizeUpstreams(upstreams []string) []string {
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
