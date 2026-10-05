package certificates

import (
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// platformCertificate is the operator-managed wildcard certificate for the
// platform domain suffix. It reloads the files when they change, so an external
// renewal needs no restart.
type platformCertificate struct {
	certFile, keyFile string
	serverName        string

	mu      sync.Mutex
	modTime [2]time.Time
	current Version
}

func newPlatformCertificate(certFile, keyFile, suffix string) (*platformCertificate, error) {
	certFile, keyFile = strings.TrimSpace(certFile), strings.TrimSpace(keyFile)
	if certFile == "" && keyFile == "" {
		return nil, nil
	}
	suffix = strings.ToLower(strings.Trim(strings.TrimSpace(suffix), "."))
	if suffix == "" {
		return nil, errors.New("platform certificate needs the platform domain suffix")
	}
	p := &platformCertificate{certFile: certFile, keyFile: keyFile, serverName: "*." + suffix}
	if _, err := p.load(); err != nil {
		return nil, err
	}
	return p, nil
}

// load returns the current version and reloads it after a file change. A broken
// replacement keeps the last good version.
func (p *platformCertificate) load() (Version, error) {
	certInfo, err := os.Stat(p.certFile)
	if err != nil {
		return p.fallback(fmt.Errorf("stat platform certificate: %w", err))
	}
	keyInfo, err := os.Stat(p.keyFile)
	if err != nil {
		return p.fallback(fmt.Errorf("stat platform key: %w", err))
	}
	modTime := [2]time.Time{certInfo.ModTime(), keyInfo.ModTime()}
	p.mu.Lock()
	if p.current.Fingerprint != "" && p.modTime == modTime {
		current := p.current
		p.mu.Unlock()
		return current, nil
	}
	p.mu.Unlock()

	chainPEM, err := os.ReadFile(p.certFile)
	if err != nil {
		return p.fallback(fmt.Errorf("read platform certificate: %w", err))
	}
	keyPEM, err := os.ReadFile(p.keyFile)
	if err != nil {
		return p.fallback(fmt.Errorf("read platform key: %w", err))
	}
	version, err := ParseVersion("", chainPEM, keyPEM)
	if err != nil {
		return p.fallback(fmt.Errorf("platform certificate: %w", err))
	}
	if err := p.verifyWildcard(version); err != nil {
		return p.fallback(err)
	}
	version.Hostname = p.serverName
	p.mu.Lock()
	p.current, p.modTime = version, modTime
	p.mu.Unlock()
	return version, nil
}

func (p *platformCertificate) verifyWildcard(version Version) error {
	leaf, err := parseLeaf(version.ChainPEM)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(leaf.DNSNames, func(name string) bool { return strings.EqualFold(name, p.serverName) }) {
		return fmt.Errorf("platform certificate must contain the wildcard SAN %s", p.serverName)
	}
	now := time.Now()
	if now.Before(version.NotBefore) || !now.Before(version.NotAfter) {
		return fmt.Errorf("platform wildcard certificate is expired or not yet valid")
	}
	if len(leaf.ExtKeyUsage) > 0 && !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) && !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageAny) {
		return fmt.Errorf("platform wildcard certificate must permit TLS server authentication")
	}
	return nil
}

func (p *platformCertificate) fallback(err error) (Version, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current.Fingerprint != "" && time.Now().Before(p.current.NotAfter) {
		return p.current, nil
	}
	return Version{}, err
}

func parseLeaf(chainPEM []byte) (*x509.Certificate, error) {
	version, err := leafDER(chainPEM)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(version)
}
