package secretkeys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach-go/v2/crdb"
)

// SecretMetadata is the masked existence record listable through the API:
// names and versions, never values.
type SecretMetadata struct {
	Name      string
	Version   int64
	UpdatedAt time.Time
}

// SealedStore persists sealed service secrets. Each seal appends a new
// immutable version; deployments capture name-to-version pins so rollback
// restores the captured values. Deletion tombstones the name while retaining
// history for pinned reads, so a rollback to a deployment that captured a
// since-deleted secret still resolves.
type SealedStore struct {
	db   *sql.DB
	deks *DEKStore
}

// NewSealedStore builds the sealed-secret inventory over db.
func NewSealedStore(db *sql.DB, deks *DEKStore) *SealedStore {
	return &SealedStore{db: db, deks: deks}
}

// sealVersionAttempts bounds version-allocation retries when concurrent
// seals race for the same (service, name).
const sealVersionAttempts = 10

// Seal appends a new sealed version for (serviceID, name) and returns its
// version. Re-sealing a tombstoned name clears the tombstone: the name lives
// again at a new version. Callers validate the name and its disjointness
// from public environment keys before calling.
//
// Concurrent seals for one name race on max(version)+1; a loser that hits
// the primary-key conflict recomputes and retries instead of surfacing a
// raw duplicate-key error.
func (s *SealedStore) Seal(ctx context.Context, q Querier, serviceID, environmentID, name string, plaintext []byte) (int64, error) {
	if strings.TrimSpace(serviceID) == "" || strings.TrimSpace(environmentID) == "" || strings.TrimSpace(name) == "" {
		return 0, errors.New("seal requires service id, environment id, and name")
	}
	var version int64
	if err := withQuerierTx(ctx, s.db, q, func(ctx context.Context, tx Querier) error {
		// Resolve the DEK inside the transaction so the mint and the
		// sealed row share a snapshot.
		dek, dekID, err := s.deks.DEKForScope(ctx, tx, DEKScopeKindEnvironment, environmentID)
		if err != nil {
			return err
		}
		defer clear(dek[:])
		for attempt := 0; ; attempt++ {
			var current sql.NullInt64
			if err := tx.QueryRowContext(ctx,
				`SELECT max(version) FROM service_secret_versions WHERE service_id = $1 AND name = $2`,
				serviceID, name).Scan(&current); err != nil {
				return fmt.Errorf("load sealed secret versions: %w", err)
			}
			candidate := int64(1)
			if current.Valid {
				candidate = current.Int64 + 1
			}
			nonce, ciphertext, err := SealValue(dek, sealAAD(serviceID, name, candidate), plaintext)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx,
				`INSERT INTO service_secret_versions(service_id, name, version, environment_id, dek_id, nonce, ciphertext, created_at)
				  VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				serviceID, name, candidate, environmentID, dekID, nonce, ciphertext, time.Now().UTC())
			if err == nil {
				version = candidate
				break
			}
			if !isUniqueViolation(err) || attempt+1 >= sealVersionAttempts {
				return fmt.Errorf("insert sealed secret version: %w", err)
			}
		}
		// Re-sealing resurrects a deleted name.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM service_secret_tombstones WHERE service_id = $1 AND name = $2`, serviceID, name); err != nil {
			return fmt.Errorf("clear sealed secret tombstone: %w", err)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return version, nil
}

// Delete tombstones (serviceID, name) so current resolution skips it while
// pinned deployment reads keep resolving captured versions. It reports
// ErrNoSuchSecret when the name has no sealed versions.
func (s *SealedStore) Delete(ctx context.Context, q Querier, serviceID, name string) error {
	var versions int64
	row := q.QueryRowContext(ctx,
		`SELECT count(*) FROM service_secret_versions WHERE service_id = $1 AND name = $2`, serviceID, name)
	if err := row.Scan(&versions); err != nil {
		return fmt.Errorf("load sealed secret versions: %w", err)
	}
	if versions == 0 {
		return fmt.Errorf("%w: service %s has no sealed secret %q", ErrNoSuchSecret, serviceID, name)
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO service_secret_tombstones(service_id, name, deleted_at) VALUES ($1, $2, $3)
		  ON CONFLICT(service_id, name) DO UPDATE SET deleted_at = excluded.deleted_at`,
		serviceID, name, time.Now().UTC()); err != nil {
		return fmt.Errorf("tombstone sealed secret: %w", err)
	}
	return nil
}

// ListMasked returns masked existence records for the service's live (not
// tombstoned) secrets, ordered by name.
func (s *SealedStore) ListMasked(ctx context.Context, q Querier, serviceID string) ([]SecretMetadata, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT v.name, v.version, v.created_at
		   FROM service_secret_versions v
		   JOIN (SELECT service_id, name, max(version) AS version
		           FROM service_secret_versions WHERE service_id = $1 GROUP BY service_id, name) cur
		     ON cur.service_id = v.service_id AND cur.name = v.name AND cur.version = v.version
		  WHERE v.service_id = $1
		    AND NOT EXISTS (SELECT 1 FROM service_secret_tombstones t
		                     WHERE t.service_id = v.service_id AND t.name = v.name)
		  ORDER BY v.name`, serviceID)
	if err != nil {
		return nil, fmt.Errorf("list sealed secrets: %w", err)
	}
	defer rows.Close()
	var out []SecretMetadata
	for rows.Next() {
		var meta SecretMetadata
		if err := rows.Scan(&meta.Name, &meta.Version, &meta.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan sealed secret: %w", err)
		}
		out = append(out, meta)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sealed secrets: %w", err)
	}
	return out, nil
}

// CurrentVersions returns the live name-to-version map for one service,
// excluding tombstoned names.
func (s *SealedStore) CurrentVersions(ctx context.Context, q Querier, serviceID string) (map[string]int64, error) {
	metas, err := s.ListMasked(ctx, q, serviceID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(metas))
	for _, meta := range metas {
		out[meta.Name] = meta.Version
	}
	return out, nil
}

// OpenVersion decrypts one pinned sealed version. Tombstoned names still
// open: pinned reads exist so rollback resolves captured values.
func (s *SealedStore) OpenVersion(ctx context.Context, q Querier, serviceID, name string, version int64) ([]byte, error) {
	var dekID string
	var nonce, ciphertext []byte
	if err := q.QueryRowContext(ctx,
		`SELECT dek_id, nonce, ciphertext FROM service_secret_versions
		  WHERE service_id = $1 AND name = $2 AND version = $3`,
		serviceID, name, version).Scan(&dekID, &nonce, &ciphertext); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("sealed secret %q for service %s has no version %d", name, serviceID, version)
		}
		return nil, fmt.Errorf("load sealed secret %q: %w", name, err)
	}
	dek, err := s.deks.DEKByID(ctx, q, dekID)
	if err != nil {
		return nil, fmt.Errorf("open sealed secret %q for service %s: %w", name, serviceID, err)
	}
	defer clear(dek[:])
	plaintext, err := OpenValue(dek, sealAAD(serviceID, name, version), nonce, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("open sealed secret %q for service %s: %w", name, serviceID, err)
	}
	return plaintext, nil
}

// OpenCurrent decrypts the newest live version of a sealed secret.
func (s *SealedStore) OpenCurrent(ctx context.Context, q Querier, serviceID, name string) ([]byte, int64, error) {
	var version sql.NullInt64
	if err := q.QueryRowContext(ctx,
		`SELECT max(v.version) FROM service_secret_versions v
		  WHERE v.service_id = $1 AND v.name = $2
		    AND NOT EXISTS (SELECT 1 FROM service_secret_tombstones t
		                     WHERE t.service_id = v.service_id AND t.name = v.name)`,
		serviceID, name).Scan(&version); err != nil {
		return nil, 0, fmt.Errorf("load sealed secret %q: %w", name, err)
	}
	if !version.Valid {
		return nil, 0, fmt.Errorf("%w: service %s has no live sealed secret %q", ErrNoSuchSecret, serviceID, name)
	}
	plaintext, err := s.OpenVersion(ctx, q, serviceID, name, version.Int64)
	if err != nil {
		return nil, 0, err
	}
	return plaintext, version.Int64, nil
}

// ResolveMany decrypts the live secrets for each service, honoring pinned
// versions where provided. pins maps service ID to a name-to-version map
// captured by a deployment; names absent from pins resolve to current. It
// returns service ID to decrypted name-to-value maps. Values are runtime
// plaintext for assigned allocations only; callers must not log or persist
// them.
func (s *SealedStore) ResolveMany(ctx context.Context, q Querier, serviceIDs []string, pins map[string]map[string]int64) (map[string]map[string]string, error) {
	out := make(map[string]map[string]string, len(serviceIDs))
	for _, serviceID := range serviceIDs {
		if _, ok := out[serviceID]; ok {
			continue
		}
		live, err := s.CurrentVersions(ctx, q, serviceID)
		if err != nil {
			return nil, err
		}
		wanted := make(map[string]int64, len(live))
		for name, version := range live {
			wanted[name] = version
		}
		// Pinned names resolve to their captured version even when the
		// name was later deleted or re-sealed; that is what makes
		// rollback restore captured values.
		for name, version := range pins[serviceID] {
			if version > 0 {
				wanted[name] = version
			}
		}
		if len(wanted) == 0 {
			continue
		}
		resolved := make(map[string]string, len(wanted))
		for name, version := range wanted {
			plaintext, err := s.OpenVersion(ctx, q, serviceID, name, version)
			if err != nil {
				return nil, err
			}
			resolved[name] = string(plaintext)
			clear(plaintext)
		}
		out[serviceID] = resolved
	}
	return out, nil
}

// sealAAD binds a sealed value to its exact location so a row copied to
// another service, name, or version does not decrypt.
func sealAAD(serviceID, name string, version int64) []byte {
	return []byte("sealed/v1\x00" + serviceID + "\x00" + name + "\x00" + strconv.FormatInt(version, 10))
}

// withQuerierTx runs fn in a transaction when q is the store DB, or reuses
// the caller's transaction when q already is one.
func withQuerierTx(ctx context.Context, db *sql.DB, q Querier, fn func(context.Context, Querier) error) error {
	if tx, ok := q.(*sql.Tx); ok {
		return fn(ctx, tx)
	}
	return crdb.ExecuteTx(ctx, db, nil, func(tx *sql.Tx) error { return fn(ctx, tx) })
}
