package signkeys

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"ebof-wg-mesh/internal/controlplane/secretkeys"

	"github.com/cockroachdb/cockroach-go/v2/crdb"
	"github.com/jackc/pgx/v5/pgconn"
)

// SignKeyWrapPurposeContext binds a wrapped signing key to its own row so
// wrapped bytes copied to another row do not unwrap.
const SignKeyWrapPurposeContext = "signkey-wrap/v1"

// SignKeyWrapPurpose returns the wrap purpose binding a wrapped signing key
// to its row.
func SignKeyWrapPurpose(id string) string { return SignKeyWrapPurposeContext + "/" + id }

// Record describes one signing key. It carries identifiers and public
// material only: private bytes stay wrapped in the row until Unwrap.
type Record struct {
	ID            string
	Scope         string
	KID           string
	State         string
	KeyType       string
	WrappingKeyID string
	PublicPEM     []byte
	CreatedAt     time.Time
	UpdatedAt     time.Time
	RetiredAt     *time.Time
}

// Material is a Record with its private bytes unwrapped into process memory.
// For ECDSA keys Private is PKCS#8 DER with Key and Cert parsed; for HMAC
// keys Private is the raw secret.
type Material struct {
	Record  Record
	Private []byte
	Key     *ecdsa.PrivateKey
	Cert    *x509.Certificate
}

// Provider is the read path every signer and verifier uses: the active key
// for signing, the active-plus-retiring set for verification, and the
// public bundle for trust anchors. Service is the database-backed
// implementation; tests substitute in-memory fakes behind this seam.
type Provider interface {
	Active(ctx context.Context, scope string) (Material, error)
	Verifying(ctx context.Context, scope string) ([]Material, error)
	PublicBundle(ctx context.Context, scope string) ([]byte, error)
}

// Service is the shared signing-key inventory. All replicas read the same
// rows; each replica unwraps into its own memory with its own keyring copy.
// Reads are read-through on every call so a replica that starts mid-rotation
// verifies with both keys without cache invalidation.
type Service struct {
	db   *sql.DB
	keks *secretkeys.Registry
}

var _ Provider = (*Service)(nil)

// New assembles a Service over an existing envelope key registry.
func New(db *sql.DB, keks *secretkeys.Registry) *Service {
	return &Service{db: db, keks: keks}
}

// Options configures Open.
type Options struct {
	// AllowGenerate permits creating missing scope keys automatically.
	// Development only: production requires explicit `signing-keys init`
	// and fails closed without an active key.
	AllowGenerate bool
	// RegistryEnabled requires the registry scope at startup. Scopes the
	// control plane never uses in this deployment stay uninitialized.
	RegistryEnabled bool
}

// Open builds the signing-key inventory. With AllowGenerate (development)
// it ensures every scope has an active key so replicas converge at startup.
// Without it (production) it requires explicitly initialized keys for the
// scopes this replica serves and fails closed otherwise.
func Open(ctx context.Context, db *sql.DB, keks *secretkeys.Service, opts Options) (*Service, error) {
	if db == nil || keks == nil || keks.Registry() == nil {
		return nil, errors.New("signing keys require a database and envelope key service")
	}
	svc := New(db, keks.Registry())
	required := []string{ScopeInternalCA, ScopeUserAssertion}
	if opts.RegistryEnabled {
		required = append(required, ScopeRegistry)
	}
	if opts.AllowGenerate {
		for _, scope := range AllScopes() {
			if scope == ScopeRegistry && !opts.RegistryEnabled {
				continue
			}
			if _, err := svc.EnsureActiveKey(ctx, scope, EnsureOptions{}); err != nil {
				return nil, err
			}
		}
		return svc, nil
	}
	for _, scope := range required {
		// Verifying, not just Active: a replica that cannot unwrap the
		// retiring key either (missing envelope material) must fail here,
		// not on its first verification.
		if _, err := svc.Verifying(ctx, scope); err != nil {
			if errors.Is(err, ErrNoActiveKey) {
				return nil, fmt.Errorf("%w for scope %q: initialize it with `controlplane signing-keys init --scope %s`: %w",
					ErrNoActiveKey, scope, scope, err)
			}
			return nil, err
		}
	}
	return svc, nil
}

// EnsureOptions configures EnsureActiveKey.
type EnsureOptions struct {
	// RegistryIssuer names the token issuer embedded in a generated
	// registry signer certificate. Empty selects the default.
	RegistryIssuer string
}

// EnsureActiveKey returns the scope's active key, generating the scope's
// first key when none exists. Concurrent replicas racing to initialize
// converge on one row; losers discard their candidate and load the winner.
// Production servers never call this: Open requires explicit init there.
func (s *Service) EnsureActiveKey(ctx context.Context, scope string, opts EnsureOptions) (Material, error) {
	if err := ValidateScope(scope); err != nil {
		return Material{}, err
	}
	if mat, err := s.Active(ctx, scope); err == nil {
		return mat, nil
	} else if !errors.Is(err, ErrNoActiveKey) {
		return Material{}, err
	}
	mat, err := s.generate(ctx, scope, RotateOptions{RegistryIssuer: opts.RegistryIssuer})
	if err != nil {
		return Material{}, err
	}
	rec, err := s.insertActiveRecord(ctx, scope, mat)
	if err != nil {
		if isUniqueViolation(err) {
			// Another replica won the race; its key is authoritative.
			return s.Active(ctx, scope)
		}
		return Material{}, err
	}
	_ = rec
	// Re-read through the shared row so the winner's material is
	// authoritative even when this replica generated a loser candidate.
	return s.Active(ctx, scope)
}

// RotateOptions configures Init and RotateStart.
type RotateOptions struct {
	// HMACSecret provisions operator-supplied HMAC material for
	// user-assertion and dashboard-session scopes instead of generating
	// it. It lets the operator install the same secret the dashboard
	// already holds from its host secret manager. Empty generates.
	HMACSecret []byte
	// RegistryIssuer names the token issuer embedded in a generated
	// registry signer certificate. Empty selects the default.
	RegistryIssuer string
}

// Init creates a scope's first active key. It fails when the scope already
// has one: Init runs once per scope, then rotation takes over.
func (s *Service) Init(ctx context.Context, scope string, opts RotateOptions) (Record, error) {
	if err := ValidateScope(scope); err != nil {
		return Record{}, err
	}
	if _, err := s.activeRecord(ctx, scope); err == nil {
		return Record{}, fmt.Errorf("%w: scope %q already has an active key; rotate it with `signing-keys rotate-start`",
			ErrAlreadyInitialized, scope)
	} else if !errors.Is(err, ErrNoActiveKey) {
		return Record{}, err
	}
	mat, err := s.generate(ctx, scope, opts)
	if err != nil {
		return Record{}, err
	}
	rec, err := s.insertActiveRecord(ctx, scope, mat)
	if err != nil {
		if isUniqueViolation(err) {
			return Record{}, fmt.Errorf("%w: another init won the race; run `signing-keys list` to see the winner",
				ErrConcurrentRotation)
		}
		return Record{}, err
	}
	return rec, nil
}

// RotateStart demotes the scope's active key to retiring and activates a
// fresh key. Both keys verify until rotate-finish deletes the retiring key
// after the overlap elapsed. It fails while a rotation is already in
// progress: finish that one first.
func (s *Service) RotateStart(ctx context.Context, scope string, opts RotateOptions) (active, retiring Record, err error) {
	if err := ValidateScope(scope); err != nil {
		return Record{}, Record{}, err
	}
	current, err := s.activeRecord(ctx, scope)
	if err != nil {
		if errors.Is(err, ErrNoActiveKey) {
			return Record{}, Record{}, fmt.Errorf("%w for scope %q: initialize it with `signing-keys init` first",
				ErrNoActiveKey, scope)
		}
		return Record{}, Record{}, err
	}
	if _, err := s.retiringRecord(ctx, scope); err == nil {
		return Record{}, Record{}, fmt.Errorf("%w: scope %q is mid-rotation; run `signing-keys rotate-finish` first",
			ErrRotationInProgress, scope)
	} else if !errors.Is(err, ErrNoRotationInProgress) {
		return Record{}, Record{}, err
	}
	// Generate outside the transaction: provider calls must not hold
	// database locks.
	mat, err := s.generate(ctx, scope, opts)
	if err != nil {
		return Record{}, Record{}, err
	}
	var newActive Record
	if err := crdb.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		res, err := tx.ExecContext(ctx,
			`UPDATE platform_signing_keys SET state = 'retiring', retired_at = $1, updated_at = $1
			  WHERE id = $2 AND state = 'active'`, now, current.ID)
		if err != nil {
			return fmt.Errorf("retire active signing key: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return fmt.Errorf("retire active signing key: %w", err)
			}
			return ErrConcurrentRotation
		}
		retiring = current
		retiring.State = KeyStateRetiring
		retiring.RetiredAt = &now
		retiring.UpdatedAt = now
		var nullRetired sql.NullTime
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO platform_signing_keys(id, scope, kid, state, key_type, wrapping_key_id, wrapped_key, public_pem, created_at, updated_at)
			  VALUES ($1, $2, $3, 'active', $4, $5, $6, $7, $8, $8)
			  RETURNING id, scope, kid, state, key_type, wrapping_key_id, public_pem, created_at, updated_at, retired_at`,
			mat.id, scope, mat.kid, mat.keyType, mat.wrappingKeyID, mat.wrapped, string(mat.publicPEM), now).Scan(
			&newActive.ID, &newActive.Scope, &newActive.KID, &newActive.State, &newActive.KeyType,
			&newActive.WrappingKeyID, &newActive.PublicPEM, &newActive.CreatedAt, &newActive.UpdatedAt, &nullRetired); err != nil {
			return fmt.Errorf("insert signing key: %w", err)
		}
		if nullRetired.Valid {
			newActive.RetiredAt = &nullRetired.Time
		}
		return nil
	}); err != nil {
		if errors.Is(err, ErrConcurrentRotation) || isUniqueViolation(err) {
			return Record{}, Record{}, fmt.Errorf("%w: another rotation won the race; run `signing-keys list` to see the winner",
				ErrConcurrentRotation)
		}
		return Record{}, Record{}, err
	}
	return newActive, retiring, nil
}

// FinishOptions configures RotateFinish.
type FinishOptions struct {
	// Now anchors overlap measurement. Zero selects the current time.
	Now time.Time
	// MinOverlap overrides the scope's default minimum overlap. Values
	// below the default are ignored: the default is a floor, not a hint.
	MinOverlap time.Duration
	// Force deletes the retiring key before the overlap elapsed. Tests
	// and documented emergencies only; the runbook never uses it.
	Force bool
}

// RotateFinish deletes a scope's retiring key after its overlap elapsed.
// It refuses early deletion so verifiers keep accepting both keys for
// longer than the longest credential lifetime.
func (s *Service) RotateFinish(ctx context.Context, scope string, opts FinishOptions) (Record, error) {
	if err := ValidateScope(scope); err != nil {
		return Record{}, err
	}
	retiring, err := s.retiringRecord(ctx, scope)
	if err != nil {
		return Record{}, err
	}
	now := opts.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	minimum, err := MinOverlapForScope(scope)
	if err != nil {
		return Record{}, err
	}
	if opts.MinOverlap > minimum {
		minimum = opts.MinOverlap
	}
	var retiredAt time.Time
	if retiring.RetiredAt != nil {
		retiredAt = retiring.RetiredAt.UTC()
	}
	if !opts.Force && now.Sub(retiredAt) < minimum {
		remaining := minimum - now.Sub(retiredAt)
		return Record{}, fmt.Errorf("%w: scope %q retiring key %s needs %s more overlap (retired %s, minimum %s)",
			ErrOverlapNotElapsed, scope, retiring.ID, remaining.Round(time.Second), retiredAt.Format(time.RFC3339), minimum)
	}
	if err := crdb.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM platform_signing_keys WHERE id = $1 AND state = 'retiring'`, retiring.ID)
		if err != nil {
			return fmt.Errorf("delete retiring signing key: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return fmt.Errorf("delete retiring signing key: %w", err)
			}
			return ErrConcurrentRotation
		}
		return nil
	}); err != nil {
		if errors.Is(err, ErrConcurrentRotation) {
			return Record{}, fmt.Errorf("%w: another finish won the race; run `signing-keys list` to see the winner",
				ErrConcurrentRotation)
		}
		return Record{}, err
	}
	return retiring, nil
}

// Active returns the scope's signing key with private bytes unwrapped.
func (s *Service) Active(ctx context.Context, scope string) (Material, error) {
	if err := ValidateScope(scope); err != nil {
		return Material{}, err
	}
	rec, err := s.activeRecord(ctx, scope)
	if err != nil {
		return Material{}, err
	}
	return s.materialize(ctx, rec)
}

// Verifying returns the keys verifiers accept: the active key first, then
// the retiring key while a rotation overlaps. Every call reads through to
// the database so replicas agree mid-rotation without cache invalidation.
func (s *Service) Verifying(ctx context.Context, scope string) ([]Material, error) {
	if err := ValidateScope(scope); err != nil {
		return nil, err
	}
	active, err := s.activeRecord(ctx, scope)
	if err != nil {
		return nil, err
	}
	out := []Record{active}
	if retiring, err := s.retiringRecord(ctx, scope); err == nil {
		out = append(out, retiring)
	} else if !errors.Is(err, ErrNoRotationInProgress) {
		return nil, err
	}
	mats := make([]Material, 0, len(out))
	for _, rec := range out {
		mat, err := s.materialize(ctx, rec)
		if err != nil {
			return nil, err
		}
		mats = append(mats, mat)
	}
	return mats, nil
}

// PublicBundle returns the concatenated certificates verifiers trust for an
// ECDSA scope: the active certificate first, then the retiring one while a
// rotation overlaps. Each certificate is trimmed and newline-terminated in
// a deterministic order shared by every replica.
func (s *Service) PublicBundle(ctx context.Context, scope string) ([]byte, error) {
	mats, err := s.Verifying(ctx, scope)
	if err != nil {
		return nil, err
	}
	var bundle []byte
	for _, mat := range mats {
		if mat.Record.KeyType != KeyTypeECDSAP256 {
			return nil, fmt.Errorf("scope %q holds no certificates", scope)
		}
		if len(bytes.TrimSpace(mat.Record.PublicPEM)) == 0 {
			return nil, fmt.Errorf("signing key %s has no certificate", mat.Record.ID)
		}
		bundle = append(bundle, bytes.TrimSpace(mat.Record.PublicPEM)...)
		bundle = append(bundle, '\n')
	}
	return bundle, nil
}

// ActiveSecret returns the active HMAC secret for dashboard-held scopes.
// Only the export path and the local harness call this: the secret crosses
// into operator or dashboard provisioning and never into logs.
func (s *Service) ActiveSecret(ctx context.Context, scope string) ([]byte, error) {
	mat, err := s.Active(ctx, scope)
	if err != nil {
		return nil, err
	}
	if mat.Record.KeyType != KeyTypeHMAC256 {
		return nil, fmt.Errorf("scope %q holds no HMAC secret", scope)
	}
	return append([]byte(nil), mat.Private...), nil
}

// List returns every signing key, active first within each scope. It never
// returns private bytes.
func (s *Service) List(ctx context.Context) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, scope, kid, state, key_type, wrapping_key_id, public_pem, created_at, updated_at, retired_at
		   FROM platform_signing_keys
		  ORDER BY scope, CASE state WHEN 'active' THEN 0 ELSE 1 END, created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list signing keys: %w", err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var rec Record
		var retiredAt sql.NullTime
		if err := rows.Scan(&rec.ID, &rec.Scope, &rec.KID, &rec.State, &rec.KeyType,
			&rec.WrappingKeyID, &rec.PublicPEM, &rec.CreatedAt, &rec.UpdatedAt, &retiredAt); err != nil {
			return nil, fmt.Errorf("scan signing key: %w", err)
		}
		if retiredAt.Valid {
			rec.RetiredAt = &retiredAt.Time
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list signing keys: %w", err)
	}
	return out, nil
}

// WrappingCounts reports how many signing keys each envelope key currently
// wraps. Envelope rotation consults it alongside DEK counts before deleting
// a retired envelope key.
func (s *Service) WrappingCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT wrapping_key_id, count(*) FROM platform_signing_keys GROUP BY wrapping_key_id`)
	if err != nil {
		return nil, fmt.Errorf("count signing keys by wrapping key: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var keyID string
		var count int64
		if err := rows.Scan(&keyID, &count); err != nil {
			return nil, fmt.Errorf("scan signing key counts: %w", err)
		}
		out[keyID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count signing keys by wrapping key: %w", err)
	}
	return out, nil
}

// RewrapAll migrates every signing key's wrap onto the active envelope key.
// It mirrors the DEK rewrap contract: idempotent, resumable per row, safe to
// re-run until it reports zero. Run it from `keys rewrap` after activating
// a new envelope key.
func (s *Service) RewrapAll(ctx context.Context) (rewrapped int, err error) {
	active, err := s.keks.ActiveKey(ctx)
	if err != nil {
		return 0, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, wrapping_key_id, wrapped_key FROM platform_signing_keys WHERE wrapping_key_id <> $1`, active.ID)
	if err != nil {
		return 0, fmt.Errorf("list signing keys to rewrap: %w", err)
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
			return 0, fmt.Errorf("scan signing key to rewrap: %w", err)
		}
		work = append(work, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("list signing keys to rewrap: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("list signing keys to rewrap: %w", err)
	}
	for _, item := range work {
		// Unwrap outside the write transaction: provider calls must not
		// hold database locks.
		raw, err := s.keks.Unwrap(ctx, item.wrappingKeyID, SignKeyWrapPurpose(item.id), item.wrapped)
		if err != nil {
			return rewrapped, fmt.Errorf("rewrap signing key %s: %w", item.id, err)
		}
		newKeyID, newWrapped, err := s.keks.WrapWithActive(ctx, SignKeyWrapPurpose(item.id), raw)
		if err != nil {
			return rewrapped, fmt.Errorf("rewrap signing key %s: %w", item.id, err)
		}
		changed, err := s.casWrapping(ctx, item.id, item.wrappingKeyID, newKeyID, newWrapped)
		if err != nil {
			return rewrapped, fmt.Errorf("rewrap signing key %s: %w", item.id, err)
		}
		if changed {
			rewrapped++
		}
	}
	return rewrapped, nil
}

// VerifyAll probe-unwraps every signing key with the envelope key its row
// records and reports how many verified. Plaintext is discarded; only the
// count crosses this boundary.
func (s *Service) VerifyAll(ctx context.Context) (verified int, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, key_type, wrapping_key_id, wrapped_key, public_pem FROM platform_signing_keys`)
	if err != nil {
		return 0, fmt.Errorf("list signing keys to verify: %w", err)
	}
	defer rows.Close()
	type pending struct {
		id            string
		keyType       string
		wrappingKeyID string
		wrapped       []byte
		publicPEM     []byte
	}
	var work []pending
	for rows.Next() {
		var item pending
		if err := rows.Scan(&item.id, &item.keyType, &item.wrappingKeyID, &item.wrapped, &item.publicPEM); err != nil {
			return 0, fmt.Errorf("scan signing key to verify: %w", err)
		}
		work = append(work, item)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("list signing keys to verify: %w", err)
	}
	for _, item := range work {
		raw, err := s.keks.Unwrap(ctx, item.wrappingKeyID, SignKeyWrapPurpose(item.id), item.wrapped)
		if err != nil {
			return verified, fmt.Errorf("verify signing key %s: %w", item.id, err)
		}
		switch item.keyType {
		case KeyTypeECDSAP256:
			key, err := parseECDSAPrivateKey(raw)
			if err != nil {
				return verified, fmt.Errorf("verify signing key %s: %w", item.id, err)
			}
			cert, err := parseCertificatePEM(item.publicPEM)
			if err != nil {
				return verified, fmt.Errorf("verify signing key %s: %w", item.id, err)
			}
			public, ok := cert.PublicKey.(*ecdsa.PublicKey)
			if !ok || !public.Equal(&key.PublicKey) {
				return verified, fmt.Errorf("verify signing key %s: certificate does not match key", item.id)
			}
		case KeyTypeHMAC256:
			if err := validateHMACSecret(raw); err != nil {
				return verified, fmt.Errorf("verify signing key %s: %w", item.id, err)
			}
		default:
			return verified, fmt.Errorf("verify signing key %s: unknown key type %q", item.id, item.keyType)
		}
		verified++
	}
	return verified, nil
}

// Ready reports whether every required scope holds an active key. Replicas
// use it for readiness alongside the envelope key check.
func (s *Service) Ready(ctx context.Context, required []string) bool {
	if s == nil {
		return false
	}
	for _, scope := range required {
		if _, err := s.activeRecord(ctx, scope); err != nil {
			return false
		}
	}
	return true
}

// generatedMaterial is a fresh unwrapped key with its wrap already applied,
// waiting for its row insert.
type generatedMaterial struct {
	id            string
	kid           string
	keyType       string
	private       []byte
	publicPEM     []byte
	wrappingKeyID string
	wrapped       []byte
	Record        Record
}

// generate mints fresh key material for scope and wraps it under the active
// envelope key. It touches no signing-key rows: callers insert.
func (s *Service) generate(ctx context.Context, scope string, opts RotateOptions) (*generatedMaterial, error) {
	keyType, err := KeyTypeForScope(scope)
	if err != nil {
		return nil, err
	}
	var private, publicPEM []byte
	switch keyType {
	case KeyTypeECDSAP256:
		var identity *ecdsaIdentity
		switch scope {
		case ScopeInternalCA:
			identity, err = generateCAIdentity()
		case ScopeRegistry:
			identity, err = generateRegistryIdentity(opts.RegistryIssuer)
		default:
			return nil, fmt.Errorf("%w for ECDSA scope %q", ErrUnknownScope, scope)
		}
		if err != nil {
			return nil, err
		}
		private, publicPEM = identity.keyDER, identity.certPEM
	case KeyTypeHMAC256:
		private = append([]byte(nil), opts.HMACSecret...)
		if len(private) == 0 {
			private, err = generateHMACSecret()
			if err != nil {
				return nil, err
			}
		} else if err := validateHMACSecret(private); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("scope %q has unknown key type %q", scope, keyType)
	}
	id, err := generateKeyID()
	if err != nil {
		return nil, err
	}
	kid, err := generateKID()
	if err != nil {
		return nil, err
	}
	wrappingKeyID, wrapped, err := s.keks.WrapWithActive(ctx, SignKeyWrapPurpose(id), private)
	if err != nil {
		return nil, err
	}
	return &generatedMaterial{
		id: id, kid: kid, keyType: keyType,
		private: private, publicPEM: publicPEM,
		wrappingKeyID: wrappingKeyID, wrapped: wrapped,
	}, nil
}

// insertActiveRecord records generated material as a scope's active key.
// Callers serialize on the scope's unique state: concurrent inserts collide
// and the loser re-reads the winner.
func (s *Service) insertActiveRecord(ctx context.Context, scope string, mat *generatedMaterial) (Record, error) {
	var rec Record
	var retiredAt sql.NullTime
	now := time.Now().UTC()
	if err := s.db.QueryRowContext(ctx,
		`INSERT INTO platform_signing_keys(id, scope, kid, state, key_type, wrapping_key_id, wrapped_key, public_pem, created_at, updated_at)
		  VALUES ($1, $2, $3, 'active', $4, $5, $6, $7, $8, $8)
		  RETURNING id, scope, kid, state, key_type, wrapping_key_id, public_pem, created_at, updated_at, retired_at`,
		mat.id, scope, mat.kid, mat.keyType, mat.wrappingKeyID, mat.wrapped, string(mat.publicPEM), now).Scan(
		&rec.ID, &rec.Scope, &rec.KID, &rec.State, &rec.KeyType,
		&rec.WrappingKeyID, &rec.PublicPEM, &rec.CreatedAt, &rec.UpdatedAt, &retiredAt); err != nil {
		return Record{}, fmt.Errorf("insert signing key: %w", err)
	}
	if retiredAt.Valid {
		rec.RetiredAt = &retiredAt.Time
	}
	return rec, nil
}

// activeRecord loads a scope's active row without unwrapping.
func (s *Service) activeRecord(ctx context.Context, scope string) (Record, error) {
	return s.stateRecord(ctx, scope, KeyStateActive, ErrNoActiveKey)
}

// retiringRecord loads a scope's retiring row without unwrapping.
func (s *Service) retiringRecord(ctx context.Context, scope string) (Record, error) {
	return s.stateRecord(ctx, scope, KeyStateRetiring, ErrNoRotationInProgress)
}

func (s *Service) stateRecord(ctx context.Context, scope, state string, missing error) (Record, error) {
	var rec Record
	var retiredAt sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT id, scope, kid, state, key_type, wrapping_key_id, public_pem, created_at, updated_at, retired_at
		   FROM platform_signing_keys WHERE scope = $1 AND state = $2 LIMIT 1`,
		scope, state).Scan(&rec.ID, &rec.Scope, &rec.KID, &rec.State, &rec.KeyType,
		&rec.WrappingKeyID, &rec.PublicPEM, &rec.CreatedAt, &rec.UpdatedAt, &retiredAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Record{}, fmt.Errorf("%w for scope %q", missing, scope)
		}
		return Record{}, fmt.Errorf("load %s signing key for scope %q: %w", state, scope, err)
	}
	if retiredAt.Valid {
		rec.RetiredAt = &retiredAt.Time
	}
	return rec, nil
}

// materialize unwraps a row's private bytes and parses ECDSA material.
func (s *Service) materialize(ctx context.Context, rec Record) (Material, error) {
	var wrappingKeyID string
	var wrapped []byte
	if err := s.db.QueryRowContext(ctx,
		`SELECT wrapping_key_id, wrapped_key FROM platform_signing_keys WHERE id = $1`, rec.ID).
		Scan(&wrappingKeyID, &wrapped); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Material{}, fmt.Errorf("signing key %s disappeared mid-rotation; re-list and retry", rec.ID)
		}
		return Material{}, fmt.Errorf("load signing key %s: %w", rec.ID, err)
	}
	raw, err := s.keks.Unwrap(ctx, wrappingKeyID, SignKeyWrapPurpose(rec.ID), wrapped)
	if err != nil {
		return Material{}, fmt.Errorf("unwrap signing key %s (scope %q): %w", rec.ID, rec.Scope, err)
	}
	mat := Material{Record: rec, Private: raw}
	switch rec.KeyType {
	case KeyTypeECDSAP256:
		key, err := parseECDSAPrivateKey(raw)
		if err != nil {
			return Material{}, fmt.Errorf("signing key %s (scope %q): %w", rec.ID, rec.Scope, err)
		}
		cert, err := parseCertificatePEM(rec.PublicPEM)
		if err != nil {
			return Material{}, fmt.Errorf("signing key %s (scope %q): %w", rec.ID, rec.Scope, err)
		}
		mat.Key, mat.Cert = key, cert
	case KeyTypeHMAC256:
		if err := validateHMACSecret(raw); err != nil {
			return Material{}, fmt.Errorf("signing key %s (scope %q): %w", rec.ID, rec.Scope, err)
		}
	default:
		return Material{}, fmt.Errorf("signing key %s has unknown key type %q", rec.ID, rec.KeyType)
	}
	return mat, nil
}

func (s *Service) casWrapping(ctx context.Context, id, fromKeyID, toKeyID string, wrapped []byte) (bool, error) {
	var changed bool
	if err := crdb.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE platform_signing_keys SET wrapping_key_id = $1, wrapped_key = $2, updated_at = $3
			  WHERE id = $4 AND wrapping_key_id = $5`,
			toKeyID, wrapped, time.Now().UTC(), id, fromKeyID)
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

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
