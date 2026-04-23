package controlplane

import (
	"strings"
	"testing"
)

func TestLogStoreMigrationTTLUsesDateTimeCast(t *testing.T) {
	t.Parallel()

	migrations := logStoreMigrations(14)
	if len(migrations) != 1 || len(migrations[0].stmts) != 1 {
		t.Fatalf("unexpected log migrations: %#v", migrations)
	}
	stmt := migrations[0].stmts[0]
	if !strings.Contains(stmt, "TTL toDateTime(observed_at) + INTERVAL 14 DAY") {
		t.Fatalf("expected TTL to cast DateTime64 observed_at to DateTime, got:\n%s", stmt)
	}
	if strings.Contains(stmt, "TTL observed_at + INTERVAL") {
		t.Fatalf("TTL must not use DateTime64 observed_at directly:\n%s", stmt)
	}
}
