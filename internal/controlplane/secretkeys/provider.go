package secretkeys

import (
	"errors"
	"fmt"
)

// ProviderKeyring names the file-backed keyring provider.
const ProviderKeyring = "keyring"

// UnwrapFailureReason classifies why an Unwrap failed, without exposing key material.
type UnwrapFailureReason string

const (
	// UnwrapReasonUnknownKey means the registry holds no key with the recorded ID.
	UnwrapReasonUnknownKey UnwrapFailureReason = "unknown-key"
	// UnwrapReasonMissingMaterial means this replica's provider lacks matching root
	// material, e.g. a keyring version never provisioned here.
	UnwrapReasonMissingMaterial UnwrapFailureReason = "missing-provider-material"
	// UnwrapReasonCorruptCiphertext means the wrapped bytes fail authentication:
	// truncated, tampered with, wrapped under a different key than recorded, or bound to another purpose.
	UnwrapReasonCorruptCiphertext UnwrapFailureReason = "corrupt-ciphertext"
	// UnwrapReasonProviderUnavailable means the provider call itself failed (I/O error, unreadable keyring file).
	UnwrapReasonProviderUnavailable UnwrapFailureReason = "provider-unavailable"
)

// UnwrapError reports a key-unwrap failure: which key ID, which reason, never the material.
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
	ErrUnknownKey        = errors.New("unknown envelope key")
	ErrActiveKeyRequired = errors.New("no active envelope key")
	// ErrKeyIsActive refuses deleting the active key; rotate first.
	ErrKeyIsActive = errors.New("envelope key is active; rotate before deleting")
	// ErrKeyHasLiveCiphertext refuses deleting a key that still wraps DEKs; rewrap first.
	ErrKeyHasLiveCiphertext = errors.New("envelope key still wraps live data-encryption keys")
	// ErrKeyVersionExists: each version activates once; provision a new version instead.
	ErrKeyVersionExists = errors.New("key version is already recorded")
	// ErrConcurrentActivation: one activation wins the race; the loser re-reads the winner.
	ErrConcurrentActivation = errors.New("concurrent key activation")

	// ErrProviderKeyNotFound: the referenced root material is absent on this replica.
	ErrProviderKeyNotFound = errors.New("provider key material not found")
	// ErrCiphertextInvalid: wrapped bytes fail authentication.
	ErrCiphertextInvalid = errors.New("wrapped key authentication failed")
)
