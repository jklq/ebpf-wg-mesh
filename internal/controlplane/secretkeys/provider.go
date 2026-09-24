package secretkeys

import (
	"errors"
	"fmt"
)

// ProviderKeyring names the file-backed keyring provider.
const ProviderKeyring = "keyring"

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

	// ErrProviderKeyNotFound is returned when the referenced root material
	// is absent on this replica. The registry
	// maps it to UnwrapReasonMissingMaterial.
	ErrProviderKeyNotFound = errors.New("provider key material not found")
	// ErrCiphertextInvalid is returned when wrapped bytes fail authentication.
	// The registry maps it to
	// UnwrapReasonCorruptCiphertext.
	ErrCiphertextInvalid = errors.New("wrapped key authentication failed")
)
