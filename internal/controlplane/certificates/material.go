package certificates

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

// ParseVersion validates a PEM chain and key and derives the version identity.
// The leaf must match the key and, when hostname is set, cover hostname.
func ParseVersion(hostname string, chainPEM, keyPEM []byte) (Version, error) {
	pair, err := tls.X509KeyPair(chainPEM, keyPEM)
	if err != nil {
		return Version{}, fmt.Errorf("certificate and key do not form a pair: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return Version{}, errors.New("certificate chain is empty")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return Version{}, fmt.Errorf("parse leaf certificate: %w", err)
	}
	if hostname != "" {
		if err := leaf.VerifyHostname(hostname); err != nil {
			return Version{}, err
		}
	}
	return Version{
		Fingerprint: Fingerprint(leaf.Raw),
		Hostname:    hostname,
		ChainPEM:    append([]byte(nil), chainPEM...),
		KeyPEM:      append([]byte(nil), keyPEM...),
		NotBefore:   leaf.NotBefore.UTC(),
		NotAfter:    leaf.NotAfter.UTC(),
	}, nil
}

// Fingerprint is the hex SHA-256 of a DER certificate.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// verifyFingerprint guards against a stored row whose leaf moved under its key.
func verifyFingerprint(version Version) error {
	der, err := leafDER(version.ChainPEM)
	if err != nil {
		return err
	}
	if got := Fingerprint(der); got != version.Fingerprint {
		return fmt.Errorf("certificate fingerprint %s does not match %s", got, version.Fingerprint)
	}
	return nil
}

func leafDER(chainPEM []byte) ([]byte, error) {
	block, _ := pem.Decode(chainPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("certificate chain is not PEM")
	}
	return block.Bytes, nil
}

// renewAt schedules renewal when one third of the lifetime remains, as Let's Encrypt advises.
func renewAt(notBefore, notAfter time.Time) time.Time {
	lifetime := notAfter.Sub(notBefore)
	if lifetime <= 0 {
		return notBefore
	}
	return notBefore.Add(lifetime * 2 / 3)
}
