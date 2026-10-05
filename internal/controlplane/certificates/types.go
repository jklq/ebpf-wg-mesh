// Package certificates gives every public hostname an HTTPS certificate. The
// live owner issues and renews custom-domain certificates over ACME HTTP-01.
// Envoy answers the challenges from the xDS snapshot, and serves the
// certificates over SDS. Generated hostnames exclusively use an externally
// provisioned wildcard certificate, required in production.
package certificates

import (
	"context"
	"time"

	"ebof-wg-mesh/internal/controlplane/xds"
)

// Hostname is one public hostname that the ingress routes.
type Hostname struct {
	Name              string
	PlatformGenerated bool
	// PlatformHostname is the CNAME target that proves ownership of a custom
	// hostname. It is empty for generated and operator-configured hostnames.
	PlatformHostname string
}

// Record is the issuance state of one hostname.
type Record struct {
	Hostname string
	// Fingerprint names the current certificate version. Empty means no certificate yet.
	Fingerprint   string
	NotAfter      time.Time
	RenewAt       time.Time
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
}

// Version is one immutable issued certificate. KeyPEM is plaintext in memory
// only; the store encrypts it at rest.
type Version struct {
	Fingerprint string
	Hostname    string
	ChainPEM    []byte
	KeyPEM      []byte
	NotBefore   time.Time
	NotAfter    time.Time
}

// Account is the ACME account of one directory. KeyPEM is plaintext in memory only.
type Account struct {
	DirectoryURL string
	URI          string
	KeyPEM       []byte
}

type Store interface {
	ListCertificates(context.Context) ([]Record, error)
	CertificateRecord(ctx context.Context, hostname string) (Record, bool, error)
	// SaveIssued stores a version and makes it current for its hostname.
	SaveIssued(ctx context.Context, version Version, renewAt time.Time) error
	// RecordFailure keeps the current version and schedules the next attempt.
	RecordFailure(ctx context.Context, hostname string, attempts int, nextAttempt time.Time, message string) error
	CertificateVersion(ctx context.Context, fingerprint string) (Version, error)
	// PruneCertificates deletes the state of hostnames without a domain binding,
	// except keep, and versions that are neither current nor newer than retainAfter.
	PruneCertificates(ctx context.Context, keep []string, retainAfter time.Time) error

	PutChallenge(ctx context.Context, challenge xds.Challenge, expiresAt time.Time) error
	DeleteChallenge(ctx context.Context, token string) error
	ListChallenges(ctx context.Context) ([]xds.Challenge, error)

	LoadACMEAccount(ctx context.Context, directoryURL string) (Account, bool, error)
	SaveACMEAccount(ctx context.Context, account Account) error
}

// HostSource lists the hostnames that the ingress routes now.
type HostSource interface {
	RoutedHostnames(context.Context) ([]Hostname, error)
}

// Ingress publishes snapshots and reports when every Envoy node applied them.
type Ingress interface {
	Sync(context.Context) error
	Applied(context.Context) (nodes int, converged bool, err error)
	RequestSync()
}

// Issuer obtains a certificate for one hostname.
type Issuer interface {
	Issue(ctx context.Context, hostname string, solver Solver) (Issued, error)
}

// Solver makes an HTTP-01 challenge answerable before the CA validates it.
type Solver interface {
	Present(ctx context.Context, hostname, token, keyAuthorization string) error
	CleanUp(ctx context.Context, hostname, token string) error
}

// Issued is the PEM chain and private key that an issuer returns.
type Issued struct {
	ChainPEM []byte
	KeyPEM   []byte
}

// OwnershipVerifier checks that a custom hostname points at its platform hostname.
type OwnershipVerifier func(ctx context.Context, hostname, platformHostname string) error

type State int

const (
	// StateUnmanaged means the platform does not manage HTTPS for the hostname.
	StateUnmanaged State = iota
	StatePending
	StateActive
	StateFailed
)

// Status is the user-visible certificate state of one hostname.
type Status struct {
	State     State
	Message   string
	ExpiresAt time.Time
	RetryAt   time.Time
}
