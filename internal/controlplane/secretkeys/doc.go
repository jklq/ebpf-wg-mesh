// Package secretkeys owns the control plane's envelope encryption for sealed
// service secrets.
//
// A Provider wraps and unwraps data-encryption keys (DEKs) without ever
// exporting root key material: the root key never appears in logs, database
// rows, process arguments, or error strings. Only wrapped DEKs and key
// identifiers are persisted. Every control-plane replica shares the same key
// state through CockroachDB: the Registry records key IDs and their
// active/retired state, and the DEKStore keeps one wrapped DEK per
// environment. New wraps always use the active key while retired keys still
// unwrap, so rotation is introduce-then-rewrap with no flag day.
package secretkeys
