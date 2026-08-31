//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
)

func TestSchemaVersionFourUpgradesDeploymentActionsToVersionFive(t *testing.T) {
	ctx := context.Background()
	dbURL := createTestDatabase(t)
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range currentSchema {
		if strings.Contains(current, "CREATE TABLE deployment_actions") || strings.Contains(current, "idx_deployment_actions_target") {
			continue
		}
		v4 := strings.ReplaceAll(current, "\n\t\t\ttarget_allocation_id STRING NOT NULL DEFAULT '',", "")
		v4 = strings.ReplaceAll(v4, "\n\t\t\tresolved_spec_json JSONB NOT NULL,", "")
		v4 = strings.ReplaceAll(v4, "\n\t\t\tvariable_versions_json JSONB NOT NULL,", "")
		v4 = strings.Replace(v4, "CREATE INDEX idx_deployments_service_build", "CREATE UNIQUE INDEX idx_deployments_service_build", 1)
		if _, err := db.ExecContext(ctx, v4); err != nil {
			_ = db.Close()
			t.Fatalf("create v4 schema: %v\n%s", err, v4)
		}
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (version INT8 PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (4, $1)`, time.Now().UTC()); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(config.DatabaseConfig{URL: dbURL, MaxOpenConns: 2, MaxIdleConns: 2}, testMeshConfig())
	if err != nil {
		t.Fatalf("upgrade v4 schema: %v", err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != 5 {
		t.Fatalf("schema version = %d, err=%v", version, err)
	}
	for _, expected := range []struct{ table, column string }{
		{table: "service_rollouts", column: "target_allocation_id"},
		{table: "deployments", column: "resolved_spec_json"},
		{table: "deployments", column: "variable_versions_json"},
	} {
		var count int
		if err := store.db.QueryRowContext(ctx,
			`SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`,
			expected.table, expected.column,
		).Scan(&count); err != nil || count != 1 {
			t.Fatalf("missing %s.%s after v5 upgrade: count=%d err=%v", expected.table, expected.column, count, err)
		}
	}
	var actionsTable int
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = 'deployment_actions'`,
	).Scan(&actionsTable); err != nil || actionsTable != 1 {
		t.Fatalf("deployment_actions table count=%d err=%v", actionsTable, err)
	}
}
