package certificates

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"ebof-wg-mesh/internal/controlplane/xds"
)

type Config struct {
	Store   Store
	Hosts   HostSource
	Ingress Ingress
	// Issuer is nil when automatic certificates are off. Status is then unmanaged
	// for every hostname that the platform certificate does not cover.
	Issuer Issuer
	Verify OwnershipVerifier
	// PlatformSuffix, PlatformCertFile, and PlatformKeyFile configure the wildcard
	// certificate of generated hostnames. Generated names never fall back to ACME.
	PlatformSuffix             string
	PlatformCertFile           string
	PlatformKeyFile            string
	RequirePlatformCertificate bool
	// Workers bounds the concurrent issuances. Zero selects a default.
	Workers int
	Now     func() time.Time
}

// Service owns the certificate of every public hostname. Every replica serves
// key pairs and status; only the live owner runs the renewal loop.
type Service struct {
	store          Store
	hosts          HostSource
	ingress        Ingress
	issuer         Issuer
	verify         OwnershipVerifier
	platform       *platformCertificate
	platformSuffix string
	workers        int
	now            func() time.Time

	keysMu sync.Mutex
	keys   map[string]xds.KeyPair

	nudge    chan struct{}
	inflight sync.Map
}

func New(cfg Config) (*Service, error) {
	if cfg.RequirePlatformCertificate && (strings.TrimSpace(cfg.PlatformCertFile) == "" || strings.TrimSpace(cfg.PlatformKeyFile) == "") {
		return nil, fmt.Errorf("production requires externally provisioned platform wildcard certificate files")
	}
	platform, err := newPlatformCertificate(cfg.PlatformCertFile, cfg.PlatformKeyFile, cfg.PlatformSuffix)
	if err != nil {
		return nil, err
	}
	workers := cfg.Workers
	if workers <= 0 {
		workers = 4
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		store:          cfg.Store,
		hosts:          cfg.Hosts,
		ingress:        cfg.Ingress,
		issuer:         cfg.Issuer,
		verify:         cfg.Verify,
		platform:       platform,
		platformSuffix: strings.ToLower(strings.Trim(strings.TrimSpace(cfg.PlatformSuffix), ".")),
		workers:        workers,
		now:            now,
		keys:           map[string]xds.KeyPair{},
		nudge:          make(chan struct{}, 1),
	}, nil
}

// SetIngress attaches the publisher after construction; the publisher reads
// key pairs from the service, so the two refer to each other.
func (s *Service) SetIngress(ingress Ingress) {
	s.ingress = ingress
}

// Managed reports whether the service issues or holds any certificate.
func (s *Service) Managed() bool {
	return s != nil && (s.issuer != nil || s.platform != nil)
}

// IngressCertificates lists the certificates and challenges that the next
// snapshot serves. It covers only hostnames that the ingress routes now, and
// leaves out expired certificates.
func (s *Service) IngressCertificates(ctx context.Context) ([]xds.Certificate, []xds.Challenge, error) {
	if !s.Managed() {
		return nil, nil, nil
	}
	now := s.now()
	var certs []xds.Certificate
	if s.platform != nil {
		version, err := s.platform.load()
		if err != nil {
			return nil, nil, err
		}
		if now.Before(version.NotBefore) || !now.Before(version.NotAfter) {
			return nil, nil, fmt.Errorf("platform wildcard certificate is expired or not yet valid")
		}
		s.rememberPlatform(version)
		certs = append(certs, xds.Certificate{
			Name: version.Hostname, ServerNames: []string{version.Hostname}, Fingerprint: version.Fingerprint,
		})
	}
	if s.issuer == nil {
		return certs, nil, nil
	}
	hosts, err := s.hosts.RoutedHostnames(ctx)
	if err != nil {
		return nil, nil, err
	}
	records, err := s.store.ListCertificates(ctx)
	if err != nil {
		return nil, nil, err
	}
	routed := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		routed[host.Name] = s.individualCertificate(host)
	}
	for _, record := range records {
		if !routed[record.Hostname] || record.Fingerprint == "" || !now.Before(record.NotAfter) {
			continue
		}
		certs = append(certs, xds.Certificate{
			Name: record.Hostname, ServerNames: []string{record.Hostname}, Fingerprint: record.Fingerprint,
		})
	}
	challenges, err := s.store.ListChallenges(ctx)
	if err != nil {
		return nil, nil, err
	}
	filtered := challenges[:0]
	for _, challenge := range challenges {
		individual, known := routed[challenge.Hostname]
		if !s.platformHostname(challenge.Hostname) && (!known || individual) {
			filtered = append(filtered, challenge)
		}
	}
	return certs, filtered, nil
}

func (s *Service) platformHostname(hostname string) bool {
	_, rest, ok := strings.Cut(strings.ToLower(strings.TrimSpace(hostname)), ".")
	return s.platformSuffix != "" && ok && rest == s.platformSuffix
}

func (s *Service) individualCertificate(host Hostname) bool {
	return !host.PlatformGenerated && !s.platformHostname(host.Name)
}

func (s *Service) rememberPlatform(version Version) {
	s.keysMu.Lock()
	s.keys[version.Fingerprint] = xds.KeyPair{CertificatePEM: version.ChainPEM, PrivateKeyPEM: version.KeyPEM}
	s.keysMu.Unlock()
}

// KeyPair implements xds.KeyPairSource. Decrypted key pairs stay in memory for
// the life of the process; versions are immutable, so the cache never goes stale.
func (s *Service) KeyPair(ctx context.Context, fingerprint string) (xds.KeyPair, error) {
	if s.platform != nil {
		if version, err := s.platform.load(); err == nil && version.Fingerprint == fingerprint {
			return xds.KeyPair{CertificatePEM: version.ChainPEM, PrivateKeyPEM: version.KeyPEM}, nil
		}
	}
	s.keysMu.Lock()
	pair, ok := s.keys[fingerprint]
	s.keysMu.Unlock()
	if ok {
		return pair, nil
	}
	version, err := s.store.CertificateVersion(ctx, fingerprint)
	if err != nil {
		return xds.KeyPair{}, fmt.Errorf("load certificate %s: %w", fingerprint, err)
	}
	if err := verifyFingerprint(version); err != nil {
		return xds.KeyPair{}, err
	}
	pair = xds.KeyPair{CertificatePEM: version.ChainPEM, PrivateKeyPEM: version.KeyPEM}
	s.keysMu.Lock()
	s.keys[fingerprint] = pair
	s.keysMu.Unlock()
	return pair, nil
}

// Status reports the certificate state of one hostname for the API.
func (s *Service) Status(ctx context.Context, hostname string) Status {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if s == nil {
		return Status{}
	}
	if s.platformHostname(hostname) {
		if s.platform == nil {
			if s.issuer == nil {
				return Status{}
			}
			return Status{State: StateFailed, Message: "generated hostnames require an externally provisioned wildcard certificate"}
		}
		version, err := s.platform.load()
		if err != nil {
			return Status{State: StateFailed, Message: err.Error()}
		}
		if s.now().Before(version.NotBefore) || !s.now().Before(version.NotAfter) {
			return Status{State: StateFailed, ExpiresAt: version.NotAfter, Message: "platform wildcard certificate is expired or not yet valid"}
		}
		return Status{State: StateActive, ExpiresAt: version.NotAfter}
	}
	if s.issuer == nil {
		return Status{}
	}
	record, ok, err := s.store.CertificateRecord(ctx, hostname)
	if err != nil {
		return Status{State: StatePending, Message: "certificate state is unavailable"}
	}
	if !ok {
		return Status{State: StatePending}
	}
	now := s.now()
	if record.Fingerprint != "" && now.Before(record.NotAfter) {
		status := Status{State: StateActive, ExpiresAt: record.NotAfter}
		if record.LastError != "" {
			status.Message = "renewal failed: " + record.LastError
			status.RetryAt = record.NextAttemptAt
		}
		return status
	}
	if record.LastError == "" {
		return Status{State: StatePending}
	}
	status := Status{State: StateFailed, Message: record.LastError, RetryAt: record.NextAttemptAt}
	if record.Fingerprint != "" {
		status.Message = "certificate expired; " + record.LastError
		status.ExpiresAt = record.NotAfter
	}
	return status
}

// RequestReconcile asks the renewal loop to look at the hostnames now, for
// example after a domain binding changed.
func (s *Service) RequestReconcile() {
	if s == nil {
		return
	}
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}
