package secretkeys

import (
	"context"
	"errors"
	"fmt"
)

// ProviderKeyring is the in-process key manager. Master keys live in a
// provisioned keyring file replicated to every control-plane replica;
// wrap/unwrap run in process with AES-256-GCM from the standard library.
const ProviderKeyring = "keyring"

// Provider wraps and unwraps data-encryption keys under root key material
// that stays inside the provider. Implementations must never surface root
// key material in returned values, logs, or error strings: only opaque
// wrapped bytes, key references, and failure reasons cross this boundary.
//
// A key reference identifies provider-side key material. Its shape is
// provider-defined (a keyring key version) and it is safe to persist in
// database rows.
//
// Wrap and Unwrap bind the ciphertext to purpose, an authenticated context
// (record identity and operation) that prevents transplanting wrapped bytes
// across records. Callers must pass a non-empty purpose that the unwrap
// side can reconstruct from the stored row.
type Provider interface {
	// Name reports the provider name ("keyring").
	Name() string
	// ProvisionKey makes provider-side key material available for future
	// Wrap/Unwrap calls and returns its reference. hint names the key
	// material to use and is always required: keys are created out of
	// band (provisioned to the keyring file on every replica) and
	// ProvisionKey verifies this replica holds the named material with a
	// round-trip proof before returning. It never generates material.
	ProvisionKey(ctx context.Context, hint string) (ref string, err error)
	// Wrap encrypts plaintext key material under ref, bound to purpose.
	Wrap(ctx context.Context, ref, purpose string, plaintext []byte) (wrapped []byte, err error)
	// Unwrap decrypts wrapped key material previously produced by Wrap
	// under ref and the same purpose.
	Unwrap(ctx context.Context, ref, purpose string, wrapped []byte) (plaintext []byte, err error)
	// Close releases provider resources.
	Close() error
}

// MaterialChecker is an optional Provider capability reporting whether
// this replica currently holds material for ref. The registry uses it to
// fail closed when a replica's keyring is missing a version the database
// references.
type MaterialChecker interface {
	HasKeyMaterial(ctx context.Context, ref string) (bool, error)
}

// BootstrapProvisioner is an optional Provider capability that generates
// first-install key material. Only development providers implement it;
// production providers fail closed and require explicit provisioning.
type BootstrapProvisioner interface {
	// EnsureBootstrapKey returns the single bootstrap key reference,
	// creating first-install material when the provider holds none. It
	// fails when the provider holds several keys (activate one
	// explicitly) and when generation is disabled.
	EnsureBootstrapKey(ctx context.Context) (ref string, err error)
}

// UnwrapFailureReason classifies why an Unwrap failed without exposing key
// material.
type UnwrapFailureReason string

const (
	// UnwrapReasonUnknownKey means the registry holds no key with the ID
	// recorded alongside the ciphertext.
	UnwrapReasonUnknownKey UnwrapFailureReason = "unknown-key"
	// UnwrapReasonMissingMaterial means the registry knows the key but this
	// replica's provider no longer has (or never had) matching root
	// material, e.g. a keyring version never provisioned here.
	UnwrapReasonMissingMaterial UnwrapFailureReason = "missing-provider-material"
	// UnwrapReasonCorruptCiphertext means the wrapped bytes fail
	// authentication: they were truncated, tampered with, wrapped under a
	// different key than recorded, or bound to another purpose or key
	// reference than the one requested.
	UnwrapReasonCorruptCiphertext UnwrapFailureReason = "corrupt-ciphertext"
	// UnwrapReasonProviderUnavailable means the provider call itself
	// failed (I/O error, unreadable keyring file).
	UnwrapReasonProviderUnavailable UnwrapFailureReason = "provider-unavailable"
)

// UnwrapError reports a key-unwrap failure with enough context to act on
// (which key ID, which reason) and never the key material itself.
type UnwrapError struct {
	KeyID  string
	Reason UnwrapFailureReason
	Err    error
}

func (e *UnwrapError) Error() string {
	if e == nil {
		return "unwrap failed"
	}
	if e.Err != nil {
		return fmt.Sprintf("unwrap with key %s failed (%s): %v", e.KeyID, e.Reason, e.Err)
	}
	return fmt.Sprintf("unwrap with key %s failed (%s)", e.KeyID, e.Reason)
}

func (e *UnwrapError) Unwrap() error { return e.Err }

// Registry-level sentinel errors.
var (
	// ErrUnknownKey is returned when no envelope key with the requested ID exists.
	ErrUnknownKey = errors.New("unknown envelope key")
	// ErrActiveKeyRequired is returned when an operation needs an active key
	// and none was ever activated.
	ErrActiveKeyRequired = errors.New("no active envelope key")
	// ErrKeyIsActive is returned when deleting the active key. Rotate first.
	ErrKeyIsActive = errors.New("envelope key is active; rotate before deleting")
	// ErrKeyHasLiveCiphertext is returned when deleting a key that still
	// wraps data-encryption keys. Rewrap first.
	ErrKeyHasLiveCiphertext = errors.New("envelope key still wraps live data-encryption keys")
	// ErrKeyVersionExists is returned when activating a keyring version the
	// registry already records. Each version activates once; provision a
	// new version instead.
	ErrKeyVersionExists = errors.New("key version is already recorded")
	// ErrConcurrentActivation is returned when two activations race. One
	// wins; the loser re-reads the winner.
	ErrConcurrentActivation = errors.New("concurrent key activation")
	// ErrNoSuchSecret is returned for sealed-secret reads/deletes that name
	// a secret with no sealed versions.
	ErrNoSuchSecret = errors.New("no such sealed secret")

	// ErrProviderKeyNotFound is returned by Provider implementations when
	// the referenced root material is absent on this replica. The registry
	// maps it to UnwrapReasonMissingMaterial.
	ErrProviderKeyNotFound = errors.New("provider key material not found")
	// ErrCiphertextInvalid is returned by Provider implementations when
	// wrapped bytes fail authentication. The registry maps it to
	// UnwrapReasonCorruptCiphertext.
	ErrCiphertextInvalid = errors.New("wrapped key authentication failed")
)
