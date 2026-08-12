package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
)

var (
	errInvalidDomainHostname      = errors.New("hostname must be a valid DNS name")
	errDomainOwnershipNotProven   = errors.New("domain CNAME does not point to the service platform hostname")
	errPlatformDomainNotGenerated = errors.New("generate a platform domain for the service first")
)

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

func (s *PlatformService) verifyDomainOwnership(ctx context.Context, userID, projectID, serviceID, hostname string) error {
	platformBinding, err := s.store.platformDomainBindingForService(ctx, userID, projectID, serviceID)
	if err != nil {
		return err
	}
	target, err := s.dnsResolver.LookupCNAME(ctx, hostname)
	if err != nil {
		return fmt.Errorf("%w: lookup %s: %v", errDomainOwnershipNotProven, hostname, err)
	}
	target = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(target), "."))
	if target != platformBinding.Hostname {
		return fmt.Errorf("%w: expected %s, got %s", errDomainOwnershipNotProven, platformBinding.Hostname, target)
	}
	return nil
}
