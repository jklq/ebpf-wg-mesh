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
		if strings.Contains(current, "CREATE TABLE deployment_actions") ||
			strings.Contains(current, "idx_deployment_actions_target") ||
			strings.Contains(current, "sandbox_profile_audit") {
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
	if err := store.db.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != 8 {
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

func TestSchemaVersionFiveUpgradesFleetToVersionSix(t *testing.T) {
	ctx := context.Background()
	dbURL := createTestDatabase(t)
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE agents (
			id STRING PRIMARY KEY,
			name STRING NOT NULL,
			advertise_addr STRING NOT NULL,
			workload_ipv6_subnet STRING NOT NULL DEFAULT '',
			wireguard_public_key STRING NOT NULL DEFAULT '',
			wireguard_listen_port INT8 NOT NULL DEFAULT 0,
			wireguard_ipv6 STRING NOT NULL DEFAULT '',
			cpu_millis_capacity INT8 NOT NULL,
			memory_mebibytes_capacity INT8 NOT NULL,
			last_seen_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			desired_revision INT8 NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX idx_agents_last_seen_id ON agents(last_seen_at DESC, id)
			STORING (cpu_millis_capacity, memory_mebibytes_capacity)`,
		`CREATE TABLE agent_bootstrap_tokens (
			token_hash BYTES PRIMARY KEY,
			agent_id STRING NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			consumed_at TIMESTAMPTZ NULL
		)`,
		`CREATE TABLE service_revisions (
			service_id STRING NOT NULL,
			spec_revision INT8 NOT NULL,
			spec_json JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (service_id, spec_revision)
		)`,
		`CREATE TABLE deployments (
			id STRING PRIMARY KEY,
			resolved_spec_json JSONB NOT NULL
		)`,
		`CREATE TABLE schema_migrations (version INT8 PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			t.Fatalf("create v5 fleet tables: %v\n%s", err, stmt)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (5, $1)`, time.Now().UTC()); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(config.DatabaseConfig{URL: dbURL, MaxOpenConns: 2, MaxIdleConns: 2}, testMeshConfig())
	if err != nil {
		t.Fatalf("upgrade v5 schema: %v", err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != 8 {
		t.Fatalf("schema version = %d, err=%v", version, err)
	}
	for _, expected := range []struct{ table, column string }{
		{table: "agents", column: "lifecycle_state"},
		{table: "agents", column: "failure_domain"},
		{table: "agents", column: "reserved_cpu_millis"},
		{table: "agent_bootstrap_tokens", column: "origin"},
	} {
		var count int
		if err := store.db.QueryRowContext(ctx,
			`SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`,
			expected.table, expected.column,
		).Scan(&count); err != nil || count != 1 {
			t.Fatalf("missing %s.%s after v6 upgrade: count=%d err=%v", expected.table, expected.column, count, err)
		}
	}
	for _, table := range []string{"platform_operators", "agent_certificates"} {
		var count int
		if err := store.db.QueryRowContext(ctx,
			`SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = $1`,
			table,
		).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s table count=%d err=%v", table, count, err)
		}
	}
}

func TestSchemaVersionSixUpgradesIsolationThroughRailwayDefaults(t *testing.T) {
	ctx := context.Background()
	dbURL := createTestDatabase(t)
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range currentSchema {
		if strings.Contains(current, "sandbox_profile_audit") {
			continue
		}
		if _, err := db.ExecContext(ctx, current); err != nil {
			_ = db.Close()
			t.Fatalf("create v6 schema: %v\n%s", err, current)
		}
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (version INT8 PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (6, $1)`, now); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO projects(id, name, kind, owner_user_id, created_at)
		VALUES ('proj-1', 'demo', 'user', 'user-1', $1)
	`, now); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO environments(id, project_id, name, kind, is_production, network_identity, created_at, updated_at)
		VALUES ('env-1', 'proj-1', 'production', 'persistent', TRUE, 1, $1, $1)
	`, now); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO services(id, environment_id, name, current_spec_revision, created_at, updated_at)
		VALUES ('svc-1', 'env-1', 'web', 1, $1, $1)
	`, now); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at)
		VALUES ('svc-1', 1, '{"runtime":{"cpuMillis":250},"source":{"image":{"image":"busybox:1.36"}}}'::JSONB, $1)
	`, now); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(config.DatabaseConfig{URL: dbURL, MaxOpenConns: 2, MaxIdleConns: 2}, testMeshConfig())
	if err != nil {
		t.Fatalf("upgrade v6 schema: %v", err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != 8 {
		t.Fatalf("schema version = %d, err=%v", version, err)
	}
	var auditTable int
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = 'sandbox_profile_audit_events'`,
	).Scan(&auditTable); err != nil || auditTable != 0 {
		t.Fatalf("sandbox_profile_audit_events table count=%d err=%v", auditTable, err)
	}
	var profilePresent bool
	if err := store.db.QueryRowContext(ctx,
		`SELECT spec_json->'runtime'->'sandboxProfile' IS NOT NULL FROM service_revisions WHERE service_id = 'svc-1' AND spec_revision = 1`,
	).Scan(&profilePresent); err != nil || profilePresent {
		t.Fatalf("sandbox profile present=%t err=%v", profilePresent, err)
	}
}

func TestSchemaVersionSevenCutsPersistedProfilesOverToRailwayDefaults(t *testing.T) {
	ctx := context.Background()
	dbURL := createTestDatabase(t)
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE service_revisions (
			service_id STRING NOT NULL,
			spec_revision INT8 NOT NULL,
			spec_json JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (service_id, spec_revision)
		)`,
		`CREATE TABLE deployments (
			id STRING PRIMARY KEY,
			resolved_spec_json JSONB NOT NULL
		)`,
		`CREATE TABLE sandbox_profile_audit_events (
			id STRING PRIMARY KEY,
			service_id STRING NOT NULL,
			actor_user_id STRING NOT NULL,
			action STRING NOT NULL,
			previous_profile_name STRING NOT NULL DEFAULT '',
			profile_name STRING NOT NULL,
			risk STRING NOT NULL,
			relaxations JSONB NOT NULL,
			spec_revision INT8 NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE TABLE schema_migrations (version INT8 PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			t.Fatalf("create v7 schema: %v\n%s", err, stmt)
		}
	}
	now := time.Now().UTC()
	oldSpec := `{"runtime":{"sandboxProfile":{"name":"legacy-root","risk":"Runs as root","relaxations":["SANDBOX_RELAXATION_RUN_AS_ROOT"]}}}`
	if _, err := db.ExecContext(ctx, `INSERT INTO service_revisions VALUES ('svc-1', 1, $1, $2)`, oldSpec, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO deployments VALUES ('dep-1', $1)`, oldSpec); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO sandbox_profile_audit_events VALUES ('audit-1', 'svc-1', 'user-1', 'selected', '', 'legacy-root', 'Runs as root', '["SANDBOX_RELAXATION_RUN_AS_ROOT"]', 1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations VALUES (7, $1)`, now); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(config.DatabaseConfig{URL: dbURL, MaxOpenConns: 2, MaxIdleConns: 2}, testMeshConfig())
	if err != nil {
		t.Fatalf("upgrade v7 schema: %v", err)
	}
	defer store.Close()

	for _, query := range []string{
		`SELECT spec_json->'runtime'->'sandboxProfile' IS NOT NULL FROM service_revisions WHERE service_id = 'svc-1'`,
		`SELECT resolved_spec_json->'runtime'->'sandboxProfile' IS NOT NULL FROM deployments WHERE id = 'dep-1'`,
	} {
		var profilePresent bool
		if err := store.db.QueryRowContext(ctx, query).Scan(&profilePresent); err != nil || profilePresent {
			t.Fatalf("cut-over profile present=%t err=%v", profilePresent, err)
		}
	}
	var auditTable int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = 'sandbox_profile_audit_events'`).Scan(&auditTable); err != nil || auditTable != 0 {
		t.Fatalf("sandbox profile audit table count=%d err=%v", auditTable, err)
	}
}
