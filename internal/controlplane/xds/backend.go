// Package xds is the control-plane xDS authority for the Envoy ingress data
// plane. Snapshots are a deterministic function of control-plane state:
// replicas racing to compute from the same state produce identical bytes and
// converge on one version. A NACK never withdraws the last published snapshot.
package xds

import "context"

type Backend struct {
	Domain       string
	Upstream     string
	AllocationID string
}

type StaticRoute struct {
	Hosts    []string
	Upstream string
}

// Certificate is one TLS certificate the HTTPS listeners serve. The snapshot
// inputs hold only its fingerprint; the key pair stays outside the published row.
type Certificate struct {
	// Name identifies the SDS secret. It stays stable across renewals, so a
	// renewal replaces the secret without touching the listener.
	Name        string
	ServerNames []string
	// Fingerprint is the hex SHA-256 of the leaf certificate DER.
	Fingerprint string
}

// Challenge is one pending ACME HTTP-01 challenge that Envoy answers directly.
type Challenge struct {
	Hostname         string
	Token            string
	KeyAuthorization string
}

// KeyPair is the PEM certificate chain and private key of one certificate.
type KeyPair struct {
	CertificatePEM []byte
	PrivateKeyPEM  []byte
}

// KeyPairSource loads the key pair of a certificate fingerprint. Every replica
// must resolve each fingerprint that a publication references.
type KeyPairSource interface {
	KeyPair(ctx context.Context, fingerprint string) (KeyPair, error)
}
