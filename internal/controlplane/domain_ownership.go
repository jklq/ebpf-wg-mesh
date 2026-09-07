package controlplane

import (
	"context"
	"crypto/sha256"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"encoding/base32"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

var (
	errInvalidDomainHostname      = errors.New("hostname must be a valid DNS name")
	errDomainOwnershipNotProven   = errors.New("domain CNAME does not point to the service platform hostname")
	errPlatformDomainNotGenerated = errors.New("generate a platform domain for the service first")
	errPlatformDomainReassignment = errors.New("generated platform domain cannot be reassigned to another service")
	errPlatformDomainInUse        = errors.New("generated platform domain cannot be deleted while a custom domain is attached")
)

const domainOwnershipLookupTimeout = 3 * time.Second

var platformDomainAdjectives = [...]string{
	"amber", "azure", "coral", "crimson", "golden", "indigo", "jade", "lilac",
	"mint", "navy", "olive", "peach", "rose", "silver", "teal", "violet",
}

func canonicalDomainHostname(raw string) (string, error) {
	hostname := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	if hostname == "" || len(hostname) > 253 || !strings.Contains(hostname, ".") {
		return "", errInvalidDomainHostname
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errInvalidDomainHostname
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return "", errInvalidDomainHostname
			}
		}
	}
	return hostname, nil
}

func generatedPlatformHostname(projectID, serviceID, suffix string) (string, error) {
	suffix, err := canonicalDomainHostname(suffix)
	if err != nil {
		return "", fmt.Errorf("platform domain suffix: %w", err)
	}
	digest := sha256.Sum256([]byte(projectID + "\x00" + serviceID))
	adjective := platformDomainAdjectives[int(digest[0])%len(platformDomainAdjectives)]
	token := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[1:11]))
	return adjective + "-" + token + "." + suffix, nil
}

func isPlatformHostname(hostname, suffix string) bool {
	return hostname == suffix || strings.HasSuffix(hostname, "."+suffix)
}

func (s *Domains) annotateDomainBinding(ctx context.Context, userID string, rec deliverycore.DomainBindingRecord) *platformv1.DomainBinding {
	binding := toProtoDomainBinding(rec)
	if rec.PlatformGenerated {
		binding.OwnershipState = platformv1.DomainOwnershipState_DOMAIN_OWNERSHIP_STATE_VERIFIED
		return binding
	}
	state, err := s.inspectDomainOwnership(ctx, userID, rec.ServiceID, rec.Hostname, rec.PlatformGenerated)
	if err != nil {
		binding.OwnershipState = platformv1.DomainOwnershipState_DOMAIN_OWNERSHIP_STATE_UNVERIFIED
		binding.OwnershipMessage = err.Error()
		return binding
	}
	binding.OwnershipState = state
	return binding
}

func (s *Domains) inspectDomainOwnership(ctx context.Context, userID, serviceID, hostname string, platformGenerated bool) (platformv1.DomainOwnershipState, error) {
	if platformGenerated {
		return platformv1.DomainOwnershipState_DOMAIN_OWNERSHIP_STATE_VERIFIED, nil
	}
	platformBinding, err := s.store.platformDomainBindingForService(ctx, userID, serviceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return platformv1.DomainOwnershipState_DOMAIN_OWNERSHIP_STATE_UNVERIFIED, errPlatformDomainNotGenerated
		}
		return platformv1.DomainOwnershipState_DOMAIN_OWNERSHIP_STATE_UNSPECIFIED, err
	}
	lookupCtx, cancel := context.WithTimeout(ctx, domainOwnershipLookupTimeout)
	defer cancel()
	if err := s.proveDomainOwnership(lookupCtx, hostname, platformBinding.Hostname); err != nil {
		return platformv1.DomainOwnershipState_DOMAIN_OWNERSHIP_STATE_UNVERIFIED, err
	}
	return platformv1.DomainOwnershipState_DOMAIN_OWNERSHIP_STATE_VERIFIED, nil
}

func (s *Domains) proveDomainOwnership(ctx context.Context, hostname, platformHostname string) error {
	hostname = normalizeDNSName(hostname)
	platformHostname = normalizeDNSName(platformHostname)
	target, cnameErr := s.dnsResolver.LookupCNAME(ctx, hostname)
	target = normalizeDNSName(target)
	if cnameErr == nil && target != "" && target != hostname {
		if target == platformHostname {
			return nil
		}
		platformCanon, err := s.dnsResolver.LookupCNAME(ctx, platformHostname)
		if err == nil && normalizeDNSName(platformCanon) == target {
			return nil
		}
	}

	customAddrs, customAddrErr := s.dnsResolver.LookupHost(ctx, hostname)
	platformAddrs, platformAddrErr := s.dnsResolver.LookupHost(ctx, platformHostname)
	if customAddrErr == nil && platformAddrErr == nil && addressSetsOverlap(customAddrs, platformAddrs) {
		return nil
	}

	if cnameErr != nil {
		return fmt.Errorf("%w: lookup %s: %v", errDomainOwnershipNotProven, hostname, cnameErr)
	}
	if target == "" || target == hostname {
		return fmt.Errorf("%w: expected CNAME %s", errDomainOwnershipNotProven, platformHostname)
	}
	return fmt.Errorf("%w: expected %s, got %s", errDomainOwnershipNotProven, platformHostname, target)
}

func normalizeDNSName(raw string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
}

func addressSetsOverlap(left, right []string) bool {
	seen := make(map[string]struct{}, len(left))
	for _, addr := range left {
		if ip := net.ParseIP(strings.TrimSpace(addr)); ip != nil {
			seen[ip.String()] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return false
	}
	for _, addr := range right {
		if ip := net.ParseIP(strings.TrimSpace(addr)); ip != nil {
			if _, ok := seen[ip.String()]; ok {
				return true
			}
		}
	}
	return false
}

func newPublicDNSResolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, net.JoinHostPort("1.1.1.1", "53"))
		},
	}
}
