package registry

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// tokenRealmURL validates a Bearer challenge's token realm against the
// registry that issued the challenge before the control plane follows it.
// The realm is attacker-controlled input — anyone can pick the registry
// host in an image reference — so it is confined to the registry's own
// site and may not reach loopback or private destinations unless the
// registry itself lives there (or the operator has allowlisted it as an
// internal registry the control plane may reach). Without this, a
// malicious registry could make the control plane probe internal services
// and echo any response back as a "token".
func tokenRealmURL(ctx context.Context, realm, registryHost string, allowedPrivateHosts []string) (*url.URL, error) {
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
		// Plain http is only for dev registries served over plain http,
		// and only to their exact authority.
		if registryScheme(endpoint) != "http" || !strings.EqualFold(tokenURL.Host, endpoint) {
			return nil, fmt.Errorf("registry auth realm %q must use https", realm)
		}
	}
	if !sameRegistrySite(realmHost, endpointHost) && !sameRegistrySite(realmHost, repositoryHost) {
		return nil, fmt.Errorf("registry auth realm %q is outside the registry's site", realm)
	}
	if !strings.EqualFold(realmHost, endpointHost) && !strings.EqualFold(realmHost, repositoryHost) &&
		!registryHostAllowed(registryHost, allowedPrivateHosts) && !registryHostAllowed(endpoint, allowedPrivateHosts) {
		prohibited, err := prohibitedDestination(ctx, realmHost)
		if err != nil {
			return nil, fmt.Errorf("registry auth realm %q cannot be verified: %w", realm, err)
		}
		if prohibited {
			return nil, fmt.Errorf("registry auth realm %q resolves to a prohibited private destination", realm)
		}
	}
	return tokenURL, nil
}

// permittedRegistryDestination requires that the control plane may send
// registry traffic to registryHost: it is on the operator's direct-image
// registry allowlist (an internal registry declared reachable and
// trusted), or it is a public destination. The registry host is
// user-controlled input — anyone can pick the registry host in an image
// reference — so an unlisted host at or resolving to loopback, private,
// link-local, or other prohibited ranges is refused: a project writer must
// not be able to point the control plane at internal services. Resolution
// failures fail closed.
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

// registryHostAllowed reports whether hostport is on the operator's
// direct-image registry allowlist (exact host[:port] match).
func registryHostAllowed(hostport string, allowedPrivateHosts []string) bool {
	hostport = strings.TrimSpace(hostport)
	for _, allowed := range allowedPrivateHosts {
		if strings.EqualFold(strings.TrimSpace(allowed), hostport) {
			return true
		}
	}
	return false
}

func hostnameOf(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(hostport, "[]")
}

// sameRegistrySite reports whether two hosts are the same site: exactly
// equal, or sibling names of one registrable domain. auth.docker.io and
// registry-1.docker.io are both docker.io; an unrelated host never is.
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

// prohibitedDestination reports whether a hostname is, or resolves to, a
// loopback, private, link-local, multicast, or unspecified address — the
// ranges an external registry must never reach through a token realm.
// Resolution failures fail closed.
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

func prohibitedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
}
