// Package platformdb is the seam between platform logic and the database
// provider that stores platform state. Callers name a provider and a schema;
// everything provider-specific — driver registration, connection-string
// dialect, search-path wiring, retryable-error classification, and server
// version handling — lives behind Provider in one adapter per provider.
// Adding a provider means adding one adapter here, not editing callers.
//
// The platform database holds exactly three schemas in one database:
// platform (control-plane state), console (browser auth/session state), and
// recovery (installer/recovery metadata). Application workloads never get
// credentials for this database; a service that needs a database runs its
// own, outside this seam.
package platformdb

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

const (
	// ProviderPostgres is the PostgreSQL provider, currently the only adapter.
	ProviderPostgres = "postgres"

	// SchemaPlatform holds control-plane tables.
	SchemaPlatform = "platform"
	// SchemaConsole holds console (dashboard) tables.
	SchemaConsole = "console"
	// SchemaRecovery holds recovery/installer metadata tables.
	SchemaRecovery = "recovery"

	// DefaultDatabase is the one database that carries all three schemas.
	DefaultDatabase = "platform"
)

// Schemas lists every schema in the platform database, in creation order.
var Schemas = []string{SchemaPlatform, SchemaConsole, SchemaRecovery}

// Config selects a provider and the schema a connection resolves unqualified
// names against. URL is the provider connection string; its database is the
// platform database regardless of schema.
type Config struct {
	Provider     string
	URL          string
	Schema       string
	MaxOpenConns int
	MaxIdleConns int
}

// Provider is the database-provider interface: the whole surface callers may
// depend on. Implementations must make Open return a pool whose connections
// resolve unqualified table names in Config.Schema.
type Provider interface {
	Name() string
	// DefaultPort is the provider's TCP port when a URL or address omits one.
	DefaultPort() int
	Open(ctx context.Context, cfg Config) (*sql.DB, error)
	// Retryable reports whether replaying the whole transaction can succeed
	// (serialization failure or deadlock in the provider's dialect).
	Retryable(err error) bool
	EnsureSchema(ctx context.Context, db *sql.DB, schema string) error
	ServerVersion(ctx context.Context, db *sql.DB) (string, error)
}

// For returns the provider adapter by name. Empty selects the default.
func For(name string) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", ProviderPostgres, "postgresql", "pgx":
		return postgresProvider{}, nil
	default:
		return nil, fmt.Errorf("unknown database provider %q", name)
	}
}

// Default returns the default provider adapter.
func Default() Provider {
	p, _ := For("")
	return p
}

// OpenURL opens a pool for url with the default provider and the given
// schema. It is the one call site shape operator tooling uses, so provider
// selection stays in exactly one place.
func OpenURL(ctx context.Context, rawURL, schema string) (*sql.DB, error) {
	return Open(ctx, Config{URL: rawURL, Schema: schema})
}

// Open opens a pool through the provider named by cfg.
func Open(ctx context.Context, cfg Config) (*sql.DB, error) {
	p, err := For(cfg.Provider)
	if err != nil {
		return nil, err
	}
	return p.Open(ctx, cfg)
}

var schemaName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ValidateSchema reports whether schema is a safe, lower-case identifier.
func ValidateSchema(schema string) error {
	if !schemaName.MatchString(schema) {
		return fmt.Errorf("invalid database schema %q", schema)
	}
	return nil
}

// QuoteIdentifier renders a provider-safe quoted identifier. PostgreSQL and
// the SQL standard share double-quote quoting; adapters with a different
// rule would own this behind their own seam, so callers never inline quotes.
func QuoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
