package signkeys

import (
	"errors"
	"fmt"
	"time"
)

// Scopes of platform signing keys. Each scope holds at most one active and
// one retiring key; signers use the active key and verifiers accept both.
const (
	// ScopeInternalCA signs internal mTLS client and server certificates
	// for agents, builders, and the dashboard.
	ScopeInternalCA = "internal-ca"
	// ScopeRegistry signs embedded registry capability credentials and
	// registry access tokens.
	ScopeRegistry = "registry"
	// ScopeUserAssertion is the HMAC secret the dashboard uses to mint
	// user assertions and the control plane uses to verify them.
	ScopeUserAssertion = "user-assertion"
	// ScopeDashboardSession is the HMAC secret the dashboard uses to sign
	// browser session tokens. The control plane stores and rotates it as
	// the system of record; only the dashboard verifies with it.
	ScopeDashboardSession = "dashboard-session"
)

// Key types stored per scope.
const (
	// KeyTypeECDSAP256 is an ECDSA P-256 private key (PKCS#8 DER when
	// unwrapped) with its self-signed certificate in PublicPEM.
	KeyTypeECDSAP256 = "ecdsa-p256"
	// KeyTypeHMAC256 is a raw HMAC-SHA-256 secret of at least 32 bytes.
	KeyTypeHMAC256 = "hmac-256"
)

// Key states within a scope.
const (
	// KeyStateActive marks the single key new signatures use.
	KeyStateActive = "active"
	// KeyStateRetiring marks the previous key, which still verifies while
	// credentials it signed drain. Rotate-finish deletes it once the
	// scope's overlap elapsed.
	KeyStateRetiring = "retiring"
)

// Minimum overlap each scope's retiring key must verify before
// rotate-finish deletes it. Each exceeds the longest credential the scope
// signs; see docs/signing-keys.md for the lifetime table behind these.
//
// Operators who raise a credential lifetime above its default must pass a
// larger --min-overlap to rotate-finish; the default refuses to delete
// early but cannot know about custom lifetimes.
const (
	// MinOverlapInternalCA covers the default 24h client certificate plus
	// renewal skew and reconnect margin. Server certificates (30 days by
	// default) are not the binding constraint: replicas keep serving a
	// retiring-chained leaf through the overlap and flip after finish.
	MinOverlapInternalCA = 48 * time.Hour
	// MinOverlapRegistry covers the default 48h pull capability plus a
	// day of operational margin.
	MinOverlapRegistry = 72 * time.Hour
	// MinOverlapUserAssertion covers the 30s assertion lifetime with wide
	// clock-skew margin.
	MinOverlapUserAssertion = 5 * time.Minute
	// MinOverlapDashboardSession covers the 30-day refresh token lifetime
	// plus margin.
	MinOverlapDashboardSession = 31 * 24 * time.Hour
)

// MinOverlapForScope reports the default minimum overlap for scope.
func MinOverlapForScope(scope string) (time.Duration, error) {
	switch scope {
	case ScopeInternalCA:
		return MinOverlapInternalCA, nil
	case ScopeRegistry:
		return MinOverlapRegistry, nil
	case ScopeUserAssertion:
		return MinOverlapUserAssertion, nil
	case ScopeDashboardSession:
		return MinOverlapDashboardSession, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrUnknownScope, scope)
	}
}

// KeyTypeForScope reports the key type a scope stores.
func KeyTypeForScope(scope string) (string, error) {
	switch scope {
	case ScopeInternalCA, ScopeRegistry:
		return KeyTypeECDSAP256, nil
	case ScopeUserAssertion, ScopeDashboardSession:
		return KeyTypeHMAC256, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownScope, scope)
	}
}

// AllScopes lists every signing scope in stable order.
func AllScopes() []string {
	return []string{ScopeInternalCA, ScopeRegistry, ScopeUserAssertion, ScopeDashboardSession}
}

// ValidateScope rejects unknown scopes before they reach SQL.
func ValidateScope(scope string) error {
	switch scope {
	case ScopeInternalCA, ScopeRegistry, ScopeUserAssertion, ScopeDashboardSession:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrUnknownScope, scope)
	}
}

// Rotation errors. Identifiers only; key material never appears here.
var (
	// ErrUnknownScope names a scope this package does not manage.
	ErrUnknownScope = errors.New("unknown signing scope")
	// ErrNoActiveKey means the scope was never initialized. Init it
	// explicitly (development bootstraps it automatically).
	ErrNoActiveKey = errors.New("no active signing key")
	// ErrAlreadyInitialized means Init found an existing active key.
	ErrAlreadyInitialized = errors.New("signing scope is already initialized")
	// ErrRotationInProgress means rotate-start found a retiring key.
	// Finish that rotation before starting another.
	ErrRotationInProgress = errors.New("signing rotation already in progress")
	// ErrNoRotationInProgress means rotate-finish found no retiring key.
	ErrNoRotationInProgress = errors.New("no signing rotation in progress")
	// ErrOverlapNotElapsed means the retiring key has not verified long
	// enough to delete yet.
	ErrOverlapNotElapsed = errors.New("signing overlap has not elapsed")
	// ErrConcurrentRotation means two rotations raced. One wins; the
	// loser re-lists.
	ErrConcurrentRotation = errors.New("concurrent signing rotation")
	// ErrHMACSecretInvalid means an operator-supplied HMAC secret is too
	// short or not usable as dashboard transport.
	ErrHMACSecretInvalid = errors.New("invalid HMAC secret")
)
