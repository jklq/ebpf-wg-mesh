package secretkeys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"ebof-wg-mesh/internal/sqlretry"
	"github.com/jackc/pgx/v5/pgconn"
)

// KeyState is the lifecycle state of an envelope key.
type KeyState string

const (
	// KeyStateActive marks the single key new wraps use.
	KeyStateActive KeyState = "active"
	// KeyStateRetired marks a key that still unwraps but never wraps new material.
	// Retired keys become deletable once no wrapped DEK references them.
	KeyStateRetired KeyState = "retired"
)

// KeyRecord describes one envelope key; identifiers only, never key material.
type KeyRecord struct {
	ID          string
	Provider    string
	ProviderRef string
	State       KeyState
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Registry is the DB-backed envelope key inventory shared by every control-plane
// replica. Exactly one key is active; activation demotes the previous one, and
// rewrap migrates wrapped DEKs onto the new key.
type Registry struct {
	db       *sql.DB
	provider *Keyring
}

func NewRegistry(db *sql.DB, provider *Keyring) *Registry {
	return &Registry{db: db, provider: provider}
}

// EnsureActiveKey returns the active key, bootstrapping the first key when none
// exists and the provider generates its own material (development keyring).
// Production fails closed: provision and activate explicitly. Concurrent replicas
// race safely: the partial unique index admits one active row, and losers re-read the winner.
func (r *Registry) EnsureActiveKey(ctx context.Context) (KeyRecord, error) {
	if rec, err := r.ActiveKey(ctx); err == nil {
		return rec, nil
	} else if !errors.Is(err, ErrActiveKeyRequired) {
		return KeyRecord{}, err
	}
	ref, err := r.provider.EnsureBootstrapKey(ctx)
	if err != nil {
		return KeyRecord{}, err
	}
	rec, err := r.insertActiveRecord(ctx, ref)
	if err == nil {
		return rec, nil
	}
	if !isUniqueViolation(err) {
		return KeyRecord{}, err
	}
	// Another replica won the race; its key is authoritative.
	return r.ActiveKey(ctx)
}

// Activate records an already-provisioned version as the new active key and
// retires the previous one. The version must already exist in this replica's file
// (provision everywhere first); each version activates once. Callers run
// DEKStore.RewrapAll after, and keep the retired material on every replica until
// no wrapped DEK references it.
func (r *Registry) Activate(ctx context.Context, version string) (KeyRecord, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return KeyRecord{}, errors.New("activate requires a key version: provision it with `controlplane keys provision` first")
	}
	var existing string
	if err := r.db.QueryRowContext(ctx,
		`SELECT id FROM envelope_keys WHERE provider_ref = $1 LIMIT 1`, version).Scan(&existing); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return KeyRecord{}, fmt.Errorf("look up key version %s: %w", version, err)
	} else if err == nil {
		return KeyRecord{}, fmt.Errorf("%w: version %s is already recorded as envelope key %s",
			ErrKeyVersionExists, version, existing)
	}
	// Provision outside the transaction: provider calls must not hold database locks.
	ref, err := r.provider.ProvisionKey(ctx, version)
	if err != nil {
		return KeyRecord{}, err
	}
	rec, err := r.insertActiveRecord(ctx, ref)
	if err != nil {
		if isUniqueViolation(err) {
			return KeyRecord{}, fmt.Errorf("%w: another activation won the race; run `controlplane keys list` to see the winner",
				ErrConcurrentActivation)
		}
		return KeyRecord{}, err
	}
	return rec, nil
}

// ActiveKey returns the key new wraps use.
func (r *Registry) ActiveKey(ctx context.Context) (KeyRecord, error) {
	rec, err := scanKeyRecord(r.db.QueryRowContext(ctx,
		`SELECT id, provider, provider_ref, state, created_at, updated_at
		   FROM envelope_keys WHERE state = 'active' LIMIT 1`))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return KeyRecord{}, ErrActiveKeyRequired
		}
		return KeyRecord{}, fmt.Errorf("load active envelope key: %w", err)
	}
	return rec, nil
}

// VerifyLocalCoverage fails closed when this replica lacks material for any
// recorded envelope key. Every replica must hold every recorded version until its
// row is deleted: retired keys still unwrap live ciphertext. Replicas start and
// report ready only when this passes.
func (r *Registry) VerifyLocalCoverage(ctx context.Context) error {
	keys, err := r.ListKeys(ctx)
	if err != nil {
		return err
	}
	var missing []string
	for _, key := range keys {
		present, err := r.provider.HasKeyMaterial(ctx, key.ProviderRef)
		if err != nil {
			return fmt.Errorf("verify key material for envelope key %s (version %s): %w",
				key.ID, key.ProviderRef, err)
		}
		if !present {
			missing = append(missing, fmt.Sprintf("%s (version %s)", key.ID, key.ProviderRef))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w on this replica for envelope key(s): %s; copy the provisioned keyring file here before serving",
			ErrProviderKeyNotFound, strings.Join(missing, ", "))
	}
	return nil
}

// ListKeys returns every key, active first, newest first within a state.
func (r *Registry) ListKeys(ctx context.Context) ([]KeyRecord, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, provider, provider_ref, state, created_at, updated_at
		   FROM envelope_keys
		  ORDER BY CASE state WHEN 'active' THEN 0 ELSE 1 END, created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("list envelope keys: %w", err)
	}
	defer rows.Close()
	var out []KeyRecord
	for rows.Next() {
		rec, err := scanKeyRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list envelope keys: %w", err)
	}
	return out, nil
}

// WrappedCounts reports how many DEKs each envelope key wraps; operators consult it before deleting retired keys.
func (r *Registry) WrappedCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT wrapping_key_id, count(*) FROM envelope_data_keys GROUP BY wrapping_key_id`)
	if err != nil {
		return nil, fmt.Errorf("count wrapped data keys: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var keyID string
		var count int64
		if err := rows.Scan(&keyID, &count); err != nil {
			return nil, fmt.Errorf("scan wrapped data keys: %w", err)
		}
		out[keyID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count wrapped data keys: %w", err)
	}
	return out, nil
}

// DeleteKey removes a retired key. It refuses the active key and any key that
// still wraps data-encryption or signing keys: activate and rewrap first, then
// delete the row before removing the version from keyring files.
func (r *Registry) DeleteKey(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrUnknownKey
	}
	return sqlretry.ExecuteTx(ctx, r.db, nil, func(tx *sql.Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM envelope_keys WHERE id = $1`, id).Scan(&state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrUnknownKey
			}
			return fmt.Errorf("load envelope key %s: %w", id, err)
		}
		if KeyState(state) == KeyStateActive {
			return ErrKeyIsActive
		}
		var live int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM envelope_data_keys WHERE wrapping_key_id = $1`, id).Scan(&live); err != nil {
			return fmt.Errorf("count wrapped data keys for %s: %w", id, err)
		}
		if live > 0 {
			return fmt.Errorf("%w: key %s still wraps %d data-encryption key(s)", ErrKeyHasLiveCiphertext, id, live)
		}
		var signing int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM platform_signing_keys WHERE wrapping_key_id = $1`, id).Scan(&signing); err != nil {
			return fmt.Errorf("count wrapped signing keys for %s: %w", id, err)
		}
		if signing > 0 {
			return fmt.Errorf("%w: key %s still wraps %d signing key(s)", ErrKeyHasLiveCiphertext, id, signing)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM envelope_keys WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete envelope key %s: %w", id, err)
		}
		return nil
	})
}

// WrapWithActive wraps plaintext key material under the active key and reports
// the key ID. purpose binds the wrap to the protected record and must be reconstructible.
func (r *Registry) WrapWithActive(ctx context.Context, purpose string, plaintext []byte) (string, []byte, error) {
	active, err := r.ActiveKey(ctx)
	if err != nil {
		return "", nil, err
	}
	wrapped, err := r.provider.Wrap(ctx, active.ProviderRef, purpose, plaintext)
	if err != nil {
		return "", nil, &UnwrapError{KeyID: active.ID, Reason: UnwrapReasonProviderUnavailable, Err: err}
	}
	return active.ID, wrapped, nil
}

// Unwrap unwraps material recorded under keyID, classifying failures for operator diagnostics.
func (r *Registry) Unwrap(ctx context.Context, keyID, purpose string, wrapped []byte) ([]byte, error) {
	var ref string
	if err := r.db.QueryRowContext(ctx, `SELECT provider_ref FROM envelope_keys WHERE id = $1`, keyID).Scan(&ref); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &UnwrapError{KeyID: keyID, Reason: UnwrapReasonUnknownKey, Err: ErrUnknownKey}
		}
		return nil, &UnwrapError{KeyID: keyID, Reason: UnwrapReasonProviderUnavailable, Err: err}
	}
	plaintext, err := r.provider.Unwrap(ctx, ref, purpose, wrapped)
	if err != nil {
		return nil, &UnwrapError{KeyID: keyID, Reason: classifyUnwrapReason(err), Err: err}
	}
	return plaintext, nil
}

func classifyUnwrapReason(err error) UnwrapFailureReason {
	switch {
	case errors.Is(err, ErrProviderKeyNotFound):
		return UnwrapReasonMissingMaterial
	case errors.Is(err, ErrCiphertextInvalid):
		return UnwrapReasonCorruptCiphertext
	default:
		return UnwrapReasonProviderUnavailable
	}
}

// insertActiveRecord retires the previous active key and records ref as the new
// active key in one transaction. ref must already be provider-verified; this only touches the database.
func (r *Registry) insertActiveRecord(ctx context.Context, ref string) (KeyRecord, error) {
	id, err := GenerateKeyID()
	if err != nil {
		return KeyRecord{}, err
	}
	var rec KeyRecord
	if err := sqlretry.ExecuteTx(ctx, r.db, nil, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx,
			`UPDATE envelope_keys SET state = 'retired', updated_at = $1 WHERE state = 'active'`, now); err != nil {
			return fmt.Errorf("retire active envelope key: %w", err)
		}
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO envelope_keys(id, provider, provider_ref, state, created_at, updated_at)
			  VALUES ($1, $2, $3, 'active', $4, $4)
			  RETURNING id, provider, provider_ref, state, created_at, updated_at`,
			id, r.provider.Name(), ref, now).Scan(
			&rec.ID, &rec.Provider, &rec.ProviderRef, &rec.State, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return fmt.Errorf("insert envelope key: %w", err)
		}
		return nil
	}); err != nil {
		return KeyRecord{}, err
	}
	return rec, nil
}

func scanKeyRecord(row *sql.Row) (KeyRecord, error) {
	var rec KeyRecord
	if err := row.Scan(&rec.ID, &rec.Provider, &rec.ProviderRef, &rec.State, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return KeyRecord{}, err
	}
	return rec, nil
}

func scanKeyRows(rows *sql.Rows) (KeyRecord, error) {
	var rec KeyRecord
	if err := rows.Scan(&rec.ID, &rec.Provider, &rec.ProviderRef, &rec.State, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return KeyRecord{}, fmt.Errorf("scan envelope key: %w", err)
	}
	return rec, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
