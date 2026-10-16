package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// apiTokenSecretPrefix marks public API token secrets. The prefix makes
// leaked tokens identifiable by secret scanners.
const apiTokenSecretPrefix = "wgm_"

const (
	apiTokenScopeAccount   = "account"
	apiTokenScopeWorkspace = "workspace"
	apiTokenScopeProject   = "project"

	apiTokenAccessRead = "read"
	apiTokenAccessFull = "full"

	maxAPITokenNameLength  = 64
	maxAPITokenExpiryDays  = 3650
	apiTokenLastUsedWindow = time.Minute
)

var (
	errAPITokenNameRequired = errors.New("api token name is required")
	errAPITokenNameTooLong  = fmt.Errorf("api token name must be at most %d characters", maxAPITokenNameLength)
	errAPITokenScope        = errors.New("api token scope is invalid for the supplied workspace, project, and environment")
	errAPITokenExpiry       = fmt.Errorf("expires_in_days must be between 0 and %d", maxAPITokenExpiryDays)
	errAPITokenNotFound     = errors.New("api token not found")
)

// apiTokenRecord is one api_tokens row. Empty scope IDs follow the scope:
// account tokens carry none, workspace tokens a workspace, project tokens a
// workspace, project, and environment.
type apiTokenRecord struct {
	ID              string
	Name            string
	Prefix          string
	Scope           string
	Access          string
	WorkspaceID     string
	ProjectID       string
	EnvironmentID   string
	CreatedByUserID string
	CreatedByEmail  string
	CreatedAt       time.Time
	ExpiresAt       *time.Time
	LastUsedAt      *time.Time
	RevokedAt       *time.Time
}

// newAPITokenSecret mints a token secret and its stored forms. Only the
// SHA-256 hash and a short display prefix are persisted.
func newAPITokenSecret() (secret, prefix, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", "", fmt.Errorf("mint api token secret: %w", err)
	}
	secret = apiTokenSecretPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return secret, apiTokenPrefix(secret), hashAPITokenSecret(secret), nil
}

func apiTokenPrefix(secret string) string {
	if len(secret) <= len(apiTokenSecretPrefix)+8 {
		return secret
	}
	return secret[:len(apiTokenSecretPrefix)+8]
}

func hashAPITokenSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// looksLikeAPITokenSecret is a cheap shape check before the hash lookup.
func looksLikeAPITokenSecret(secret string) bool {
	if !strings.HasPrefix(secret, apiTokenSecretPrefix) || len(secret) > 128 {
		return false
	}
	body := secret[len(apiTokenSecretPrefix):]
	decoded, err := base64.RawURLEncoding.DecodeString(body)
	return err == nil && len(decoded) == 32
}

func normalizeAPITokenName(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", errAPITokenNameRequired
	case len([]rune(name)) > maxAPITokenNameLength:
		return "", errAPITokenNameTooLong
	}
	return name, nil
}

// Exported aliases for the offline host-admin token command
// (controlplane api-token), which shares the token format and store.
type (
	APITokenRecord = apiTokenRecord
	APITokenMint   = createAPITokenInput
)

const (
	APITokenScopeAccount   = apiTokenScopeAccount
	APITokenScopeWorkspace = apiTokenScopeWorkspace
	APITokenScopeProject   = apiTokenScopeProject
	APITokenAccessRead     = apiTokenAccessRead
	APITokenAccessFull     = apiTokenAccessFull
)

// CreateAPITokenOffline mints a token directly against the platform
// database. Host-admin only: authorization was the operator's decision to
// run the command. The secret is returned once, exactly like the RPC.
func CreateAPITokenOffline(ctx context.Context, db *sql.DB, userID, email string, in APITokenMint) (APITokenRecord, string, error) {
	return (&database{db: db}).createAPIToken(ctx, userID, email, in)
}

// createAPITokenInput carries scope IDs already authorized by the caller.
type createAPITokenInput struct {
	Name          string
	Scope         string
	Access        string
	WorkspaceID   string
	ProjectID     string
	EnvironmentID string
	ExpiresInDays int32
}

// createAPIToken inserts a token for userID and returns the record with the
// plaintext secret, which is never stored or returned again.
func (s *database) createAPIToken(ctx context.Context, userID, email string, in createAPITokenInput) (apiTokenRecord, string, error) {
	if in.ExpiresInDays < 0 || in.ExpiresInDays > maxAPITokenExpiryDays {
		return apiTokenRecord{}, "", errAPITokenExpiry
	}
	secret, prefix, hash, err := newAPITokenSecret()
	if err != nil {
		return apiTokenRecord{}, "", err
	}
	rec := apiTokenRecord{
		ID:              uuid.NewString(),
		Name:            in.Name,
		Prefix:          prefix,
		Scope:           in.Scope,
		Access:          in.Access,
		WorkspaceID:     in.WorkspaceID,
		ProjectID:       in.ProjectID,
		EnvironmentID:   in.EnvironmentID,
		CreatedByUserID: userID,
		CreatedByEmail:  email,
		CreatedAt:       time.Now().UTC(),
	}
	if in.ExpiresInDays > 0 {
		expires := rec.CreatedAt.Add(time.Duration(in.ExpiresInDays) * 24 * time.Hour)
		rec.ExpiresAt = &expires
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO api_tokens(id, name, secret_hash, prefix, scope, access,
			workspace_id, project_id, environment_id, created_by_user_id,
			created_by_email, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6,
			NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''), $10, $11, $12, $13)`,
		rec.ID, rec.Name, hash, rec.Prefix, rec.Scope, rec.Access,
		rec.WorkspaceID, rec.ProjectID, rec.EnvironmentID,
		rec.CreatedByUserID, rec.CreatedByEmail, rec.CreatedAt, rec.ExpiresAt)
	if err != nil {
		return apiTokenRecord{}, "", fmt.Errorf("create api token: %w", err)
	}
	return rec, secret, nil
}

const apiTokenColumns = `id, name, prefix, scope, access,
	COALESCE(workspace_id, ''), COALESCE(project_id, ''), COALESCE(environment_id, ''),
	created_by_user_id, created_by_email, created_at, expires_at, last_used_at, revoked_at`

func scanAPIToken(row interface{ Scan(...any) error }) (apiTokenRecord, error) {
	var rec apiTokenRecord
	err := row.Scan(&rec.ID, &rec.Name, &rec.Prefix, &rec.Scope, &rec.Access,
		&rec.WorkspaceID, &rec.ProjectID, &rec.EnvironmentID,
		&rec.CreatedByUserID, &rec.CreatedByEmail, &rec.CreatedAt,
		&rec.ExpiresAt, &rec.LastUsedAt, &rec.RevokedAt)
	return rec, err
}

func (s *database) listAPITokens(ctx context.Context, where string, arg any, includeRevoked bool) ([]apiTokenRecord, error) {
	query := `SELECT ` + apiTokenColumns + ` FROM api_tokens WHERE ` + where
	if !includeRevoked {
		query += ` AND revoked_at IS NULL`
	}
	query += ` ORDER BY created_at DESC, id`
	rows, err := s.db.QueryContext(ctx, query, arg)
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	defer rows.Close()
	var out []apiTokenRecord
	for rows.Next() {
		rec, err := scanAPIToken(rows)
		if err != nil {
			return nil, fmt.Errorf("list api tokens: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *database) apiTokenByID(ctx context.Context, tokenID string) (apiTokenRecord, error) {
	rec, err := scanAPIToken(s.db.QueryRowContext(ctx,
		`SELECT `+apiTokenColumns+` FROM api_tokens WHERE id = $1`, tokenID))
	if errors.Is(err, sql.ErrNoRows) {
		return apiTokenRecord{}, errAPITokenNotFound
	}
	if err != nil {
		return apiTokenRecord{}, fmt.Errorf("load api token: %w", err)
	}
	return rec, nil
}

// revokeAPIToken tombstones the token. Revoking an already revoked token is
// a no-op so retries converge.
func (s *database) revokeAPIToken(ctx context.Context, tokenID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at = COALESCE(revoked_at, statement_timestamp()) WHERE id = $1`, tokenID)
	if err != nil {
		return fmt.Errorf("revoke api token: %w", err)
	}
	return nil
}

var (
	errAPITokenInvalid = errors.New("invalid api token")
	errAPITokenExpired = errors.New("api token expired")
	errAPITokenRevoked = errors.New("api token revoked")
)

// apiTokenBySecret authenticates a presented secret. Unknown, malformed,
// revoked, and expired secrets are distinct errors for the public API's
// error mapping, but all fail closed.
func (s *database) apiTokenBySecret(ctx context.Context, secret string, now time.Time) (apiTokenRecord, error) {
	if !looksLikeAPITokenSecret(secret) {
		return apiTokenRecord{}, errAPITokenInvalid
	}
	rec, err := scanAPIToken(s.db.QueryRowContext(ctx,
		`SELECT `+apiTokenColumns+` FROM api_tokens WHERE secret_hash = $1`, hashAPITokenSecret(secret)))
	if errors.Is(err, sql.ErrNoRows) {
		return apiTokenRecord{}, errAPITokenInvalid
	}
	if err != nil {
		return apiTokenRecord{}, fmt.Errorf("authenticate api token: %w", err)
	}
	switch {
	case rec.RevokedAt != nil:
		return apiTokenRecord{}, errAPITokenRevoked
	case rec.ExpiresAt != nil && !now.Before(*rec.ExpiresAt):
		return apiTokenRecord{}, errAPITokenExpired
	}
	return rec, nil
}

// touchAPITokenLastUsed records use at most once per apiTokenLastUsedWindow.
func (s *database) touchAPITokenLastUsed(ctx context.Context, tokenID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at = statement_timestamp()
		 WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < statement_timestamp() - $2::interval)`,
		tokenID, apiTokenLastUsedWindow.String())
	if err != nil {
		return fmt.Errorf("touch api token: %w", err)
	}
	return nil
}

// resourceScope identifies where a resource lives, for confining
// workspace- and project-scoped tokens. Zero fields stay empty for
// ancestor kinds (a project has no environment).
type resourceScope struct {
	WorkspaceID   string
	ProjectID     string
	EnvironmentID string
	ServiceID     string
}

// resourceScope resolves a resource's workspace/project/environment by
// kind: "workspace", "project", "environment", "service", "volume", or
// "domain" (by hostname). found is false for unknown or managed-project
// resources, which tokens can never reach.
func (s *database) resolveResourceScope(ctx context.Context, kind, id string) (resourceScope, bool, error) {
	var query string
	switch kind {
	case "workspace":
		query = `SELECT id, '', '', '' FROM workspaces WHERE id = $1`
	case "project":
		query = `SELECT COALESCE(workspace_id, ''), id, '', '' FROM projects WHERE id = $1 AND kind = 'user'`
	case "environment":
		query = `SELECT COALESCE(p.workspace_id, ''), p.id, e.id, ''
			FROM environments e JOIN projects p ON p.id = e.project_id
			WHERE e.id = $1 AND p.kind = 'user'`
	case "service":
		query = `SELECT COALESCE(p.workspace_id, ''), p.id, e.id, svc.id
			FROM services svc
			JOIN environments e ON e.id = svc.environment_id
			JOIN projects p ON p.id = e.project_id
			WHERE svc.id = $1 AND p.kind = 'user'`
	case "volume":
		query = `SELECT COALESCE(p.workspace_id, ''), p.id, e.id, ''
			FROM volumes v
			JOIN environments e ON e.id = v.environment_id
			JOIN projects p ON p.id = e.project_id
			WHERE v.id = $1 AND p.kind = 'user'`
	case "domain":
		query = `SELECT COALESCE(p.workspace_id, ''), p.id, e.id, svc.id
			FROM domain_bindings d
			JOIN services svc ON svc.id = d.service_id
			JOIN environments e ON e.id = svc.environment_id
			JOIN projects p ON p.id = e.project_id
			WHERE d.hostname = $1 AND p.kind = 'user'`
	default:
		return resourceScope{}, false, fmt.Errorf("unknown resource kind %q", kind)
	}
	var scope resourceScope
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&scope.WorkspaceID, &scope.ProjectID, &scope.EnvironmentID, &scope.ServiceID)
	if errors.Is(err, sql.ErrNoRows) {
		return resourceScope{}, false, nil
	}
	if err != nil {
		return resourceScope{}, false, fmt.Errorf("resolve %s scope: %w", kind, err)
	}
	if kind == "workspace" {
		scope.WorkspaceID = id
	}
	return scope, scope.WorkspaceID != "", nil
}
