package productionops

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

func inspectSQLClient(ctx context.Context, path, schema string) error {
	if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(schema) {
		return fmt.Errorf("invalid console schema")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	u, err := url.Parse(strings.TrimSpace(string(b)))
	if err != nil || u.Scheme != "postgresql" || u.Query().Get("sslmode") != "verify-full" {
		return fmt.Errorf("component SQL client requires verified TLS")
	}
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	if err = db.PingContext(ctx); err != nil {
		return fmt.Errorf("component SQL TLS authentication failed")
	}
	for _, query := range []string{"SELECT count(*) FROM public.schema_migrations", "SELECT count(*) FROM " + schema + ".schema_migrations", "SELECT count(*) FROM public.recovery_runtime_authority"} {
		var count int
		if err = db.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return fmt.Errorf("component SQL permissions are incomplete")
		}
		if count != 1 {
			return fmt.Errorf("component SQL schema/authority differs")
		}
	}
	return nil
}
