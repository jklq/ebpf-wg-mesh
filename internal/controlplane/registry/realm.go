package registry

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// tokenRealm is a Bearer challenge's token endpoint that passed tokenRealmURL.
// trustedAuthority reports whether the realm's authority was operator-approved in its
// own right (dialable as DNS declares it, privates included). Otherwise the dial
// guard's re-validation and pinning apply: the registry's entry covers only its endpoints.
type tokenRealm struct {
	url              *url.URL
	trustedAuthority bool
}

// tokenRealmURL validates a Bearer realm against the challenging registry before
// following it. The realm is attacker-controlled, so it is confined to the registry's
// own site and barred from loopback/private destinations unless the registry lives
// there or the realm authority is allowlisted — else a malicious registry could probe
// internal services and echo responses as tokens.
func tokenRealmURL(ctx context.Context, realm, registryHost string, allowedPrivateHosts []string) (*tokenRealm, error) {
	tokenURL, err := url.Parse(realm)
	if err != nil || !tokenURL.IsAbs() || (tokenURL.Scheme != "https" && tokenURL.Scheme != "http") {
		return nil, fmt.Errorf("registry auth realm %q is not a valid token URL", realm)
	}
	if tokenURL.User != nil {
		return nil, fmt.Errorf("registry auth realm %q must not carry credentials", realm)
	}
	endpoint := registryEndpoint(registryHost)
	realmHost := tokenURL.Hostname()
	endpointHost := hostnameOf(endpoint)
	repositoryHost := hostnameOf(registryHost)
	if tokenURL.Scheme != "https" {
		// Plain http is only for dev registries, and only to their exact authority.
		if registryScheme(endpoint) != "http" || !strings.EqualFold(tokenURL.Host, endpoint) {
			return nil, fmt.Errorf("registry auth realm %q must use https", realm)
		}
	}
	if !sameRegistrySite(realmHost, endpointHost) && !sameRegistrySite(realmHost, repositoryHost) {
		return nil, fmt.Errorf("registry auth realm %q is outside the registry's site", realm)
	}
	// A private destination needs the realm's own allowlist entry: the registry's
	// entry covers its endpoints, not every same-site service.
	trusted := registryHostAllowed(tokenURL.Host, allowedPrivateHosts)
	if !strings.EqualFold(realmHost, endpointHost) && !strings.EqualFold(realmHost, repositoryHost) && !trusted {
		prohibited, err := prohibitedDestination(ctx, realmHost)
		if err != nil {
			return nil, fmt.Errorf("registry auth realm %q cannot be verified: %w", realm, err)
		}
		if prohibited {
			return nil, fmt.Errorf("registry auth realm %q resolves to a prohibited private destination", realm)
		}
	}
	return &tokenRealm{url: tokenURL, trustedAuthority: trusted}, nil
}

// permittedRegistryDestination requires that the control plane may send registry
// traffic to registryHost: it is on the operator's direct-image allowlist, or it is
// public. The host is user-controlled, so unlisted loopback/private/prohibited
// destinations are refused, and resolution failures fail closed.
func permittedRegistryDestination(ctx context.Context, registryHost string, allowedPrivateHosts []string) error {
	endpoint := registryEndpoint(registryHost)
	if registryHostAllowed(registryHost, allowedPrivateHosts) || registryHostAllowed(endpoint, allowedPrivateHosts) {
		return nil
	}
	prohibited, err := prohibitedDestination(ctx, hostnameOf(endpoint))
	if err != nil {
		return fmt.Errorf("registry host %q cannot be verified: %w", registryHost, err)
	}
	if prohibited {
		return fmt.Errorf("registry host %q resolves to a prohibited private destination; list it in CONTROLPLANE_DIRECT_IMAGE_ALLOWED_PRIVATE_REGISTRIES if the control plane should reach it", registryHost)
	}
	return nil
}

// registryHostAllowed reports whether hostport is on the direct-image allowlist.
// Ports are part of the identity: an entry matches exactly and a bare host means the
// default port, so one port never waives checks for another.
func registryHostAllowed(hostport string, allowedPrivateHosts []string) bool {
	want := normalizedRegistryEndpoint(hostport)
	for _, allowed := range allowedPrivateHosts {
		if strings.EqualFold(normalizedRegistryEndpoint(allowed), want) {
			return true
		}
	}
	return false
}

// normalizedRegistryEndpoint canonicalizes a registry endpoint for matching: Docker
// Hub aliases collapse to registry-1.docker.io and a bare host means the default port.
func normalizedRegistryEndpoint(hostport string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(hostport))
	if err != nil {
		host, port = strings.TrimSpace(hostport), ""
	}
	host = strings.Trim(hostnameOf(host), "[]")
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(registryEndpoint(host), port)
}

func hostnameOf(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(hostport, "[]")
}

// sameRegistrySite reports whether two hosts are the same site: exactly equal, or
// siblings under one registrable domain.
func sameRegistrySite(a, b string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	if net.ParseIP(a) != nil || net.ParseIP(b) != nil {
		return false
	}
	aDomain, err := publicsuffix.EffectiveTLDPlusOne(strings.ToLower(a))
	if err != nil {
		return false
	}
	bDomain, err := publicsuffix.EffectiveTLDPlusOne(strings.ToLower(b))
	if err != nil {
		return false
	}
	return aDomain == bDomain
}

// lookupIPAddr resolves a realm host for the prohibited-destination
// check. Tests replace it to stay offline.
var lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// prohibitedDestination reports whether a hostname is or resolves to a non-public
// address (loopback, private, link-local, multicast, unspecified). Failures fail closed.
func prohibitedDestination(ctx context.Context, host string) (bool, error) {
	if strings.EqualFold(host, "localhost") {
		return true, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return prohibitedIP(ip), nil
	}
	addrs, err := lookupIPAddr(ctx, host)
	if err != nil {
		return false, err
	}
	for _, addr := range addrs {
		if prohibitedIP(addr.IP) {
			return true, nil
		}
	}
	return false, nil
}

// nonPublicRanges are the special-purpose networks that are not public unicast and not
// covered by Go's IsPrivate: RFC 6598 shared space (routes like private space) plus
// documentation, benchmarking, protocol-assignment, and reserved blocks.
var nonPublicRanges = parseNonPublicRanges(
	"0.0.0.0/8",       // "this network" and protocol assignments (RFC 1122)
	"100.64.0.0/10",   // shared address space, CGNAT (RFC 6598)
	"192.0.0.0/24",    // IETF protocol assignments (RFC 6890)
	"192.0.2.0/24",    // TEST-NET-1 (RFC 5737)
	"198.51.100.0/24", // TEST-NET-2 (RFC 5737)
	"203.0.113.0/24",  // TEST-NET-3 (RFC 5737)
	"198.18.0.0/15",   // benchmarking (RFC 2544)
	"240.0.0.0/4",     // reserved, includes broadcast (RFC 1112)
	"100::/64",        // discard-only (RFC 6666)
	"2001:2::/48",     // benchmarking (RFC 5180)
	"2001:db8::/32",   // documentation (RFC 3849)
)

func parseNonPublicRanges(cidrs ...string) []*net.IPNet {
	networks := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err)
		}
		networks = append(networks, network)
	}
	return networks
}

func prohibitedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, network := range nonPublicRanges {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}
