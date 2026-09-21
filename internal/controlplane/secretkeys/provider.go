package secretkeys

import (
	"context"
	"errors"
	"fmt"
)

// Provider names understood by the control-plane configuration.
const (
	ProviderFile   = "file"
	ProviderAWSKMS = "aws-kms"
)

// Provider wraps and unwraps data-encryption keys under a root key that
// stays inside the provider. Implementations must never surface root key
// material in returned values, logs, or error strings: only opaque wrapped
// bytes, key references, and failure reasons cross this boundary.
//
// A key reference identifies provider-side key material. Its shape is
// provider-defined (a file key label, a KMS key ARN) and it is safe to
// persist in database rows.
type Provider interface {
	// Name reports the provider name ("file", "aws-kms").
	Name() string
	// ProvisionKey makes provider-side key material available for future
	// Wrap/Unwrap calls and returns its reference. For providers that
	// generate their own material (file) hint is ignored; for providers
	// whose keys are created out of band (AWS KMS) hint carries the
	// operator-supplied key identifier and ProvisionKey verifies it
	// round-trips before returning.
	ProvisionKey(ctx context.Context, hint string) (ref string, err error)
	// Wrap encrypts plaintext key material under ref.
	Wrap(ctx context.Context, ref string, plaintext []byte) (wrapped []byte, err error)
	// Unwrap decrypts wrapped key material previously produced by Wrap
	// under ref.
	Unwrap(ctx context.Context, ref string, wrapped []byte) (plaintext []byte, err error)
	// Close releases provider resources.
	Close() error
}

// UnwrapFailureReason classifies why an Unwrap failed without exposing key
// material.
type UnwrapFailureReason string

const (
	// UnwrapReasonUnknownKey means the registry holds no key with the ID
	// recorded alongside the ciphertext.
	UnwrapReasonUnknownKey UnwrapFailureReason = "unknown-key"
	// UnwrapReasonMissingMaterial means the registry knows the key but the
	// provider no longer has (or never had) matching root material, e.g. a
	// file key deleted out of band or a KMS key scheduled for deletion.
	UnwrapReasonMissingMaterial UnwrapFailureReason = "missing-provider-material"
	// UnwrapReasonCorruptCiphertext means the wrapped bytes fail
	// authentication: they were truncated, tampered with, or wrapped under
	// a different key than recorded.
	UnwrapReasonCorruptCiphertext UnwrapFailureReason = "corrupt-ciphertext"
	// UnwrapReasonProviderUnavailable means the provider call itself
	// failed (I/O error, KMS outage, credentials, throttling).
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
	// and rotation deleted or never created one.
	ErrActiveKeyRequired = errors.New("no active envelope key")
	// ErrKeyIsActive is returned when deleting the active key. Rotate first.
	ErrKeyIsActive = errors.New("envelope key is active; rotate before deleting")
	// ErrKeyHasLiveCiphertext is returned when deleting a key that still
	// wraps data-encryption keys. Rewrap first.
	ErrKeyHasLiveCiphertext = errors.New("envelope key still wraps live data-encryption keys")
	// ErrNoSuchSecret is returned for sealed-secret reads/deletes that name
	// a secret with no sealed versions.
	ErrNoSuchSecret = errors.New("no such sealed secret")

	// ErrProviderKeyNotFound is returned by Provider implementations when
	// the referenced root material is gone: a file key deleted out of
	// band, a KMS key deleted or disabled. The registry maps it to
	// UnwrapReasonMissingMaterial.
	ErrProviderKeyNotFound = errors.New("provider key material not found")
	// ErrCiphertextInvalid is returned by Provider implementations when
	// wrapped bytes fail authentication. The registry maps it to
	// UnwrapReasonCorruptCiphertext.
	ErrCiphertextInvalid = errors.New("wrapped key authentication failed")
)
