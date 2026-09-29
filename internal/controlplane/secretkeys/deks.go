package secretkeys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cockroachdb/cockroach-go/v2/crdb"
	"github.com/jackc/pgx/v5/pgconn"
)

// DEKScopeKindEnvironment scopes a DEK to one environment — the platform's
// isolation scope — bounding a key compromise to one environment.
const DEKScopeKindEnvironment = "environment"

// DEKWrapPurposeContext binds a wrapped DEK to its own row so copied bytes do not unwrap.
const DEKWrapPurposeContext = "dek-wrap/v1"

// DEKWrapPurpose returns the wrap purpose binding a wrapped DEK to its row.
func DEKWrapPurpose(dekID string) string { return DEKWrapPurposeContext + "/" + dekID }

// DEKStore mints, caches, and rewraps per-scope data-encryption keys. All replicas
// share the wrapped rows; each replica unwraps into its own memory cache on first use.
type DEKStore struct {
	db       *sql.DB
	registry *Registry

	mu      sync.Mutex
	byScope map[string]string         // scopeKind + "\x00" + scopeID -> dek ID
	byID    map[string]*[DEKSize]byte // dek ID -> plaintext DEK
}

func NewDEKStore(db *sql.DB, registry *Registry) *DEKStore {
	return &DEKStore{
		db:       db,
		registry: registry,
		byScope:  map[string]string{},
		byID:     map[string]*[DEKSize]byte{},
	}
}

// DEKForScope returns the plaintext DEK for a scope, minting and wrapping it under
// the active key on first use. Racing replicas converge on one row; losers discard
// their candidate and load the winner. q scopes the row to the caller's transaction
// so a seal in the same transaction sees the mint.
func (s *DEKStore) DEKForScope(ctx context.Context, q Querier, scopeKind, scopeID string) ([DEKSize]byte, string, error) {
	var zero [DEKSize]byte
	if scopeKind == "" || scopeID == "" {
		return zero, "", errors.New("dek scope kind and id are required")
	}
	s.mu.Lock()
	if id, ok := s.byScope[scopeCacheKey(scopeKind, scopeID)]; ok {
		if dek, ok := s.byID[id]; ok {
			s.mu.Unlock()
			return *dek, id, nil
		}
	}
	s.mu.Unlock()

	dek, id, err := s.loadOrMintDEK(ctx, q, scopeKind, scopeID)
	if err != nil {
		return zero, "", err
	}
	s.mu.Lock()
	s.byScope[scopeCacheKey(scopeKind, scopeID)] = id
	s.byID[id] = &dek
	s.mu.Unlock()
	return dek, id, nil
}

// DEKByID returns the plaintext DEK for a stored DEK record, unwrapping on cache
// miss. Failures surface as typed *UnwrapError diagnostics.
func (s *DEKStore) DEKByID(ctx context.Context, q Querier, dekID string) ([DEKSize]byte, error) {
	var zero [DEKSize]byte
	s.mu.Lock()
	if dek, ok := s.byID[dekID]; ok {
		s.mu.Unlock()
		return *dek, nil
	}
	s.mu.Unlock()

	var wrappingKeyID string
	var wrapped []byte
	var scopeKind, scopeID string
	if err := q.QueryRowContext(ctx,
		`SELECT scope_kind, scope_id, wrapping_key_id, wrapped_dek FROM envelope_data_keys WHERE id = $1`,
		dekID).Scan(&scopeKind, &scopeID, &wrappingKeyID, &wrapped); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return zero, fmt.Errorf("unknown data-encryption key %s", dekID)
		}
		return zero, fmt.Errorf("load data-encryption key %s: %w", dekID, err)
	}
	raw, err := s.registry.Unwrap(ctx, wrappingKeyID, DEKWrapPurpose(dekID), wrapped)
	if err != nil {
		return zero, err
	}
	defer clear(raw)
	if len(raw) != DEKSize {
		return zero, &UnwrapError{KeyID: wrappingKeyID, Reason: UnwrapReasonCorruptCiphertext,
			Err: fmt.Errorf("data-encryption key %s has invalid length", dekID)}
	}
	var dek [DEKSize]byte
	copy(dek[:], raw)
	s.mu.Lock()
	s.byID[dekID] = &dek
	// Opportunistically heal the scope mapping so later scope lookups hit.
	ck := scopeCacheKey(scopeKind, scopeID)
	if _, ok := s.byScope[ck]; !ok {
		s.byScope[ck] = dekID
	}
	s.mu.Unlock()
	return dek, nil
}

// RewrapAll unwraps every DEK with its current wrapping key and re-wraps it under
// the active key. DEK bytes are unchanged, so sealed values keep decrypting and the
// cache stays valid; only the wrapping migrates. It is idempotent: each row commits
// independently, so an interrupted run resumes where it stopped.
//
// Run Activate first, and re-run after any concurrent activation: an activation
// landing mid-rewrap leaves rows on the retired key, and the next run converges them.
func (s *DEKStore) RewrapAll(ctx context.Context) (rewrapped int, err error) {
	active, err := s.registry.ActiveKey(ctx)
	if err != nil {
		return 0, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, wrapping_key_id, wrapped_dek FROM envelope_data_keys WHERE wrapping_key_id <> $1`, active.ID)
	if err != nil {
		return 0, fmt.Errorf("list data-encryption keys to rewrap: %w", err)
	}
	type pending struct {
		id            string
		wrappingKeyID string
		wrapped       []byte
	}
	var work []pending
	for rows.Next() {
		var item pending
		if err := rows.Scan(&item.id, &item.wrappingKeyID, &item.wrapped); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan data-encryption key to rewrap: %w", err)
		}
		work = append(work, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("list data-encryption keys to rewrap: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("list data-encryption keys to rewrap: %w", err)
	}
	for _, item := range work {
		// Unwrap outside the write transaction: provider calls must not hold database
		// locks. WrapWithActive reads the newest active key, so a mid-rewrap activation
		// converges this row instead of stranding it.
		raw, err := s.registry.Unwrap(ctx, item.wrappingKeyID, DEKWrapPurpose(item.id), item.wrapped)
		if err != nil {
			return rewrapped, fmt.Errorf("rewrap data-encryption key %s: %w", item.id, err)
		}
		newKeyID, newWrapped, err := s.registry.WrapWithActive(ctx, DEKWrapPurpose(item.id), raw)
		clear(raw)
		if err != nil {
			return rewrapped, fmt.Errorf("rewrap data-encryption key %s: %w", item.id, err)
		}
		changed, err := s.casWrapping(ctx, item.id, item.wrappingKeyID, newKeyID, newWrapped)
		if err != nil {
			return rewrapped, fmt.Errorf("rewrap data-encryption key %s: %w", item.id, err)
		}
		if changed {
			rewrapped++
		}
	}
	return rewrapped, nil
}

// VerifyAll probe-unwraps every wrapped DEK with its recorded key and reports how
// many verified. It detects inconsistent replicas (same version ID, different
// material) and corrupt rows before they wedge reads or rewrap. Plaintext is discarded.
func (s *DEKStore) VerifyAll(ctx context.Context) (verified int, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, wrapping_key_id, wrapped_dek FROM envelope_data_keys`)
	if err != nil {
		return 0, fmt.Errorf("list data-encryption keys to verify: %w", err)
	}
	defer rows.Close()
	type pending struct {
		id            string
		wrappingKeyID string
		wrapped       []byte
	}
	var work []pending
	for rows.Next() {
		var item pending
		if err := rows.Scan(&item.id, &item.wrappingKeyID, &item.wrapped); err != nil {
			return 0, fmt.Errorf("scan data-encryption key to verify: %w", err)
		}
		work = append(work, item)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("list data-encryption keys to verify: %w", err)
	}
	for _, item := range work {
		raw, err := s.registry.Unwrap(ctx, item.wrappingKeyID, DEKWrapPurpose(item.id), item.wrapped)
		if err != nil {
			return verified, fmt.Errorf("verify data-encryption key %s: %w", item.id, err)
		}
		if len(raw) != DEKSize {
			clear(raw)
			return verified, fmt.Errorf("verify data-encryption key %s: invalid length", item.id)
		}
		clear(raw)
		verified++
	}
	return verified, nil
}

func (s *DEKStore) casWrapping(ctx context.Context, dekID, fromKeyID, toKeyID string, wrapped []byte) (bool, error) {
	var changed bool
	if err := crdb.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE envelope_data_keys SET wrapping_key_id = $1, wrapped_dek = $2, updated_at = $3
			  WHERE id = $4 AND wrapping_key_id = $5`,
			toKeyID, wrapped, time.Now().UTC(), dekID, fromKeyID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		changed = n > 0
		return nil
	}); err != nil {
		return false, err
	}
	return changed, nil
}

func (s *DEKStore) loadOrMintDEK(ctx context.Context, q Querier, scopeKind, scopeID string) ([DEKSize]byte, string, error) {
	var zero [DEKSize]byte
	var id, wrappingKeyID string
	var wrapped []byte
	if err := q.QueryRowContext(ctx,
		`SELECT id, wrapping_key_id, wrapped_dek FROM envelope_data_keys WHERE scope_kind = $1 AND scope_id = $2`,
		scopeKind, scopeID).Scan(&id, &wrappingKeyID, &wrapped); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return zero, "", fmt.Errorf("load data-encryption key: %w", err)
	} else if err == nil {
		raw, err := s.registry.Unwrap(ctx, wrappingKeyID, DEKWrapPurpose(id), wrapped)
		if err != nil {
			return zero, "", err
		}
		defer clear(raw)
		if len(raw) != DEKSize {
			return zero, "", &UnwrapError{KeyID: wrappingKeyID, Reason: UnwrapReasonCorruptCiphertext,
				Err: fmt.Errorf("data-encryption key %s has invalid length", id)}
		}
		var dek [DEKSize]byte
		copy(dek[:], raw)
		return dek, id, nil
	}
	// No row: mint a candidate. The unique scope constraint elects one winner; the ID
	// is generated before the wrap so the wrap binds to this row's purpose.
	candidate, err := GenerateDEK()
	if err != nil {
		return zero, "", err
	}
	defer clear(candidate[:])
	candidateID, err := GenerateDEKID()
	if err != nil {
		return zero, "", err
	}
	keyID, wrappedCandidate, err := s.registry.WrapWithActive(ctx, DEKWrapPurpose(candidateID), candidate[:])
	if err != nil {
		return zero, "", err
	}
	now := time.Now().UTC()
	res, err := q.ExecContext(ctx,
		`INSERT INTO envelope_data_keys(id, scope_kind, scope_id, wrapping_key_id, wrapped_dek, created_at, updated_at)
		  VALUES ($1, $2, $3, $4, $5, $6, $6) ON CONFLICT(scope_kind, scope_id) DO NOTHING`,
		candidateID, scopeKind, scopeID, keyID, wrappedCandidate, now)
	if err != nil {
		return zero, "", fmt.Errorf("mint data-encryption key: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return zero, "", fmt.Errorf("mint data-encryption key: %w", err)
	} else if n == 1 {
		return candidate, candidateID, nil
	}
	// The winner may be invisible to this snapshot; let the transaction retry.
	return zero, "", &pgconn.PgError{Code: "40001", Message: "retry data-encryption key mint after concurrent insert"}
}

func scopeCacheKey(kind, id string) string { return kind + "\x00" + id }
