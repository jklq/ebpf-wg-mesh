// Package schemacompat reads the database-owned compatibility boundary. A
// future additive schema can admit older binaries; contraction raises the
// boundary only after those binaries have left the deployment.
package schemacompat

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
)

type Queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Status struct {
	Version        int
	CompatibleFrom int
	Count          int
}

func (s Status) Supports(release, minimum int) bool {
	return s.Count == 1 && s.CompatibleFrom > 0 && s.CompatibleFrom <= s.Version &&
		s.Version >= minimum && s.CompatibleFrom <= release
}

func Read(ctx context.Context, db Queryer, schema string) (Status, error) {
	var status Status
	if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(schema) {
		return status, fmt.Errorf("invalid schema identifier")
	}
	var metadata bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=$1 AND table_name='schema_migrations' AND column_name='min_compatible_version')`, schema).Scan(&metadata); err != nil {
		return status, err
	}
	compatibility := "version"
	if metadata {
		compatibility = "COALESCE(min_compatible_version,version)"
	}
	query := "SELECT COALESCE(MAX(version),0),COALESCE(MAX(" + compatibility + "),0),COUNT(*) FROM " + schema + ".schema_migrations"
	err := db.QueryRowContext(ctx, query).Scan(&status.Version, &status.CompatibleFrom, &status.Count)
	return status, err
}
