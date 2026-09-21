// Package secretkeys owns the control plane's envelope encryption for sealed
// service secrets.
//
// A small in-process key manager wraps and unwraps data-encryption keys
// (DEKs) under versioned AES-256 master keys. The master keys live in an
// explicitly provisioned keyring file that the operator replicates to every
// control-plane replica, separately from the database; CockroachDB holds
// only ciphertext, wrapped DEKs, and shared active/retired key-version
// metadata. Root key material never appears in logs, database rows, process
// arguments, or error strings. Only wrapped bytes and key identifiers are
// persisted.
//
// New wraps always use the active key while retired keys still unwrap, so
// rotation is provision-then-activate-then-rewrap with no flag day. Replicas
// hold no node-local authority beyond their copy of the provisioned file:
// every replica must hold every recorded version until the version's row is
// deleted, and replicas fail closed when any is missing.
package secretkeys
