// Package signkeys owns the platform's short-lived signing keys: the
// internal mTLS certificate authority, the embedded registry token signer,
// the dashboard user-assertion HMAC secret, and the dashboard session HMAC
// secret.
//
// Every key lives as one row in platform_signing_keys, with its private
// material wrapped by the envelope key provider from 2.3a
// (internal/controlplane/secretkeys). CockroachDB holds the wrapped bytes
// and the shared active/retiring state; the master keyring file stays
// provisioned to every replica separately from the database. Replicas hold
// no node-local signing authority: they read key state through on every
// use, sign only with the active key, and verify against both the active
// and the retiring key.
//
// Rotation is overlap, not ceremony: rotate-start demotes the active key
// to retiring and activates a fresh key, both keys verify while the
// longest-lived credential drains, and rotate-finish deletes the retiring
// key once the overlap elapsed. Overlap minimums per scope live in
// scopes.go and in docs/signing-keys.md.
package signkeys
