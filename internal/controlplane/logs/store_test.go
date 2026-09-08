package logs

import (
	"strings"
	"testing"
)

func TestLogStoreMigrationTTLUsesDateTimeCast(t *testing.T) {
	t.Parallel()

	migrations := logStoreMigrations(14)
	var stmt string
	for _, migration := range migrations {
		if migration.name != "service_logs_table" {
			continue
		}
		if len(migration.stmts) != 1 {
			t.Fatalf("unexpected service_logs_table migration: %#v", migration)
		}
		stmt = migration.stmts[0]
		break
	}
	if stmt == "" {
		t.Fatalf("service_logs_table migration not found: %#v", migrations)
	}
	if !strings.Contains(stmt, "TTL toDateTime(observed_at) + INTERVAL 14 DAY") {
		t.Fatalf("expected TTL to cast DateTime64 observed_at to DateTime, got:\n%s", stmt)
	}
	if strings.Contains(stmt, "TTL observed_at + INTERVAL") {
		t.Fatalf("TTL must not use DateTime64 observed_at directly:\n%s", stmt)
	}
}
