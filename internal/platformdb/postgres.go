package platformdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/XSAM/otelsql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/otel/attribute"
)

// postgresProvider is the PostgreSQL adapter. It is the only place in the
// codebase that registers the pgx driver or knows PostgreSQL URL dialect,
// SQLSTATE retry codes, and version reporting.
type postgresProvider struct{}

func (postgresProvider) Name() string     { return ProviderPostgres }
func (postgresProvider) DefaultPort() int { return 5432 }

var instrumentedDriver = sync.OnceValue(func() string {
	// One wrapped driver for every pool: query spans carry the operation
	// class only (DisableQuery keeps statement text and any literals out
	// of telemetry), and pool gauges come from RegisterDBStatsMetrics.
	name, err := otelsql.Register("pgx",
		otelsql.WithAttributes(attribute.String("db.system", "postgresql")),
		otelsql.WithSpanOptions(otelsql.SpanOptions{
			DisableQuery:         true,
			OmitConnResetSession: true,
			OmitConnectorConnect: true,
			OmitConnPrepare:      true,
		}),
		otelsql.WithSpanNameFormatter(func(_ context.Context, method otelsql.Method, query string) string {
			fields := strings.Fields(query)
			if len(fields) == 0 {
				return string(method)
			}
			return string(method) + " " + strings.ToUpper(fields[0])
		}),
	)
	if err != nil {
		slog.Warn("platformdb: otelsql driver registration failed; database spans disabled", "error", err)
		return "pgx"
	}
	return name
})

func (postgresProvider) driverName() string { return instrumentedDriver() }

// WithSchema returns rawURL with the connection's search_path pinned to
// schema (falling back to public for provider-owned objects). pgx passes
// unknown URL parameters to the server as runtime parameters, so every
// pooled connection resolves unqualified names in schema.
func WithSchema(rawURL, schema string) (string, error) {
	if err := ValidateSchema(schema); err != nil {
		return "", err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse database URL: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("database URL must use postgres:// or postgresql://, got %q", u.Scheme)
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// DatabaseName extracts the database name from a provider URL.
func DatabaseName(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}

// ReplaceDatabase returns rawURL pointing at database name instead.
func ReplaceDatabase(rawURL, name string) (string, error) {
	if err := ValidateSchema(name); err != nil {
		return "", fmt.Errorf("database name: %w", err)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse database URL: %w", err)
	}
	u.Path = "/" + name
	return u.String(), nil
}

func (p postgresProvider) Open(ctx context.Context, cfg Config) (*sql.DB, error) {
	dsn := cfg.URL
	if cfg.Schema != "" {
		var err error
		if dsn, err = WithSchema(cfg.URL, cfg.Schema); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open(p.driverName(), dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if cfg.MaxOpenConns > 0 {
		db.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		db.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	if _, err := otelsql.RegisterDBStatsMetrics(db, otelsql.WithAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.namespace", cfg.Schema),
	)); err != nil {
		slog.Warn("platformdb: pool stats metrics unavailable", "error", err)
	}
	return db, nil
}

// Retryable classifies PostgreSQL serialization failures (40001) and
// deadlocks (40P01). Anything else must not be blindly replayed.
func (postgresProvider) Retryable(err error) bool {
	var state interface{ SQLState() string }
	if !errors.As(err, &state) {
		return false
	}
	return state.SQLState() == "40001" || state.SQLState() == "40P01"
}

func (postgresProvider) EnsureSchema(ctx context.Context, db *sql.DB, schema string) error {
	if err := ValidateSchema(schema); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+QuoteIdentifier(schema))
	return err
}

func (postgresProvider) ServerVersion(ctx context.Context, db *sql.DB) (string, error) {
	var version string
	err := db.QueryRowContext(ctx, "SELECT current_setting('server_version')").Scan(&version)
	return version, err
}

// MajorVersion parses the PostgreSQL major version ("17.4" -> 17). It
// returns 0 for anything unparseable, which never matches a pinned release.
func MajorVersion(version string) int {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	// server_version may carry a distribution suffix: "17.4 (Debian ...)".
	if i := strings.IndexAny(v, " -"); i >= 0 {
		v = v[:i]
	}
	major, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}
	return n
}

// ValidateVersionTransition gates in-place (restart-only) upgrades: same
// major with a non-decreasing minor/patch. A PostgreSQL major change needs
// pg_upgrade or a restore into a fresh data directory, never a rolling
// binary swap, so it is rejected here and routed to a restore plan.
func ValidateVersionTransition(from, to string) error {
	parse := func(v string) ([3]int, error) {
		var parts [3]int
		fields := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
		if len(fields) < 1 || len(fields) > 3 {
			return parts, fmt.Errorf("PostgreSQL must pin a stable major[.minor[.patch]] version")
		}
		for i, f := range fields {
			x, err := strconv.Atoi(f)
			if err != nil || x < 0 {
				return parts, fmt.Errorf("invalid PostgreSQL version %q", v)
			}
			parts[i] = x
		}
		return parts, nil
	}
	a, err := parse(from)
	if err != nil {
		return err
	}
	b, err := parse(to)
	if err != nil {
		return err
	}
	if a[0] == b[0] && (b[1] > a[1] || b[1] == a[1] && b[2] >= a[2]) {
		return nil
	}
	return fmt.Errorf("unsupported in-place PostgreSQL version transition %s -> %s; a major upgrade requires pg_upgrade or a restore for downgrade", from, to)
}
