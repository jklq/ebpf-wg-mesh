//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
)

func TestProjectNetworkIdentitiesAreUniqueAndDeliveredToAgents(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{Users: []config.BootstrapUser{
		{Subject: "user-1", Email: "user-1@example.com", Projects: []string{"one", "two"}},
	}}); err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil {
		t.Fatalf("listProjects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("expected two projects, got %d", len(projects))
	}
	if projects[0].NetworkIdentity == 0 || projects[1].NetworkIdentity == 0 || projects[0].NetworkIdentity == projects[1].NetworkIdentity {
		t.Fatalf("expected distinct non-zero network identities: %#v", projects)
	}

	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatalf("upsertAgent: %v", err)
	}
	service, err := store.createScheduledService(ctx, "user-1", projects[0].ID, "web", directImageServiceSpec("nginx:1.27", nil))
	if err != nil {
		t.Fatalf("createScheduledService: %v", err)
	}
	state, err := store.desiredStateForAgent(ctx, service.AllocatedAgentID)
	if err != nil {
		t.Fatalf("desiredStateForAgent: %v", err)
	}
	if len(state.GetServices()) != 1 || state.GetServices()[0].GetNetworkIdentity() != projects[0].NetworkIdentity {
		t.Fatalf("network identity was not delivered in desired state: %#v", state.GetServices())
	}
}

func TestAgentBootstrapTokensAreBoundDurableAndSingleUse(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	tokens := []config.AgentBootstrapToken{{AgentID: "node-1", Token: "one-time-secret"}}
	if err := store.ensureAgentBootstrapTokens(ctx, tokens); err != nil {
		t.Fatalf("ensureAgentBootstrapTokens: %v", err)
	}
	if err := store.consumeAgentBootstrapToken(ctx, "node-2", "one-time-secret"); !errors.Is(err, errInvalidBootstrapToken) {
		t.Fatalf("expected agent binding rejection, got %v", err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() {
			results <- store.consumeAgentBootstrapToken(ctx, "node-1", "one-time-secret")
		}()
	}
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		} else if !errors.Is(err, errInvalidBootstrapToken) {
			t.Fatalf("unexpected concurrent consumption error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly one token consumer, got %d", successes)
	}
	if err := store.ensureAgentBootstrapTokens(ctx, tokens); err != nil {
		t.Fatalf("reseed bootstrap tokens: %v", err)
	}
	if err := store.consumeAgentBootstrapToken(ctx, "node-1", "one-time-secret"); !errors.Is(err, errInvalidBootstrapToken) {
		t.Fatalf("expected consumed token rejection after reseed, got %v", err)
	}
}

func TestRemovedAgentBootstrapTokenIsRevoked(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.ensureAgentBootstrapTokens(ctx, []config.AgentBootstrapToken{{AgentID: "node-1", Token: "old-secret"}}); err != nil {
		t.Fatalf("seed old token: %v", err)
	}
	if err := store.ensureAgentBootstrapTokens(ctx, []config.AgentBootstrapToken{{AgentID: "node-1", Token: "new-secret"}}); err != nil {
		t.Fatalf("rotate token: %v", err)
	}
	if err := store.consumeAgentBootstrapToken(ctx, "node-1", "old-secret"); !errors.Is(err, errInvalidBootstrapToken) {
		t.Fatalf("expected removed token rejection, got %v", err)
	}
	if err := store.consumeAgentBootstrapToken(ctx, "node-1", "new-secret"); err != nil {
		t.Fatalf("consume replacement token: %v", err)
	}
}

func TestNetworkIdentityMigrationBackfillsExistingProjects(t *testing.T) {
	t.Parallel()

	dbURL := createTestDatabase(t)
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	now := time.Now().UTC()
	statements := []string{
		`CREATE TABLE schema_migrations (version INT8 PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL)`,
		`CREATE TABLE projects (
			id STRING PRIMARY KEY, name STRING NOT NULL, kind STRING NOT NULL DEFAULT 'user',
			system_key STRING NULL, owner_subject STRING NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE TABLE agents (
			id STRING PRIMARY KEY, desired_revision INT8 NOT NULL DEFAULT 0
		)`,
		`INSERT INTO projects(id, name, created_at) VALUES ('project-b', 'B', $1), ('project-a', 'A', $1)`,
		`INSERT INTO agents(id, desired_revision) VALUES ('node-1', 9)`,
	}
	for _, statement := range statements {
		var err error
		if strings.Contains(statement, "$1") {
			_, err = db.ExecContext(context.Background(), statement, now)
		} else {
			_, err = db.ExecContext(context.Background(), statement)
		}
		if err != nil {
			db.Close()
			t.Fatalf("prepare old schema with %q: %v", statement, err)
		}
	}
	for version := 1; version <= 5; version++ {
		if _, err := db.ExecContext(context.Background(), `INSERT INTO schema_migrations(version, applied_at) VALUES ($1, $2)`, version, now); err != nil {
			db.Close()
			t.Fatalf("mark migration %d: %v", version, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close old database: %v", err)
	}

	store, err := OpenStore(config.DatabaseConfig{URL: dbURL}, testMeshConfig())
	if err != nil {
		t.Fatalf("OpenStore migration: %v", err)
	}
	defer store.Close()
	rows, err := store.db.QueryContext(context.Background(), `SELECT network_identity FROM projects ORDER BY id`)
	if err != nil {
		t.Fatalf("query migrated identities: %v", err)
	}
	defer rows.Close()
	identities := make(map[int64]struct{})
	for rows.Next() {
		var identity int64
		if err := rows.Scan(&identity); err != nil {
			t.Fatalf("scan identity: %v", err)
		}
		if identity <= 0 {
			t.Fatalf("invalid migrated identity %d", identity)
		}
		identities[identity] = struct{}{}
	}
	if len(identities) != 2 {
		t.Fatalf("expected two unique migrated identities, got %v", identities)
	}
	var revision int64
	if err := store.db.QueryRowContext(context.Background(), `SELECT desired_revision FROM agents WHERE id = 'node-1'`).Scan(&revision); err != nil {
		t.Fatalf("query desired revision: %v", err)
	}
	if revision != 10 {
		t.Fatalf("expected migration to trigger agent reconciliation, got revision %d", revision)
	}
}
