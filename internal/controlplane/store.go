package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"

	"github.com/cockroachdb/cockroach-go/v2/crdb"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const initialSchemaVersion = 1

type Store struct {
	db   *sql.DB
	mesh config.ControlPlaneMeshConfig
}

type migration struct {
	version int
	stmts   []string
}

var storeMigrations = []migration{
	{
		version: initialSchemaVersion,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS users (
				subject STRING PRIMARY KEY,
				email STRING NOT NULL,
				created_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS projects (
				id STRING PRIMARY KEY,
				name STRING NOT NULL,
				kind STRING NOT NULL DEFAULT 'user',
				system_key STRING NULL,
				owner_subject STRING NOT NULL DEFAULT '',
				created_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS agents (
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
			`CREATE INDEX IF NOT EXISTS idx_agents_last_seen_id
			    ON agents(last_seen_at DESC, id)
			    STORING (cpu_millis_capacity, memory_mebibytes_capacity)`,
			`CREATE TABLE IF NOT EXISTS project_memberships (
				subject STRING NOT NULL REFERENCES users(subject),
				project_id STRING NOT NULL REFERENCES projects(id),
				role STRING NOT NULL,
				PRIMARY KEY (subject, project_id)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_project_memberships_subject ON project_memberships(subject, project_id)`,
			`CREATE TABLE IF NOT EXISTS volumes (
				id STRING PRIMARY KEY,
				project_id STRING NOT NULL REFERENCES projects(id),
				name STRING NOT NULL,
				size_bytes INT8 NOT NULL,
				bound_agent_id STRING NOT NULL REFERENCES agents(id),
				created_at TIMESTAMPTZ NOT NULL,
				UNIQUE (project_id, name)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_volumes_project_created_at ON volumes(project_id, created_at, id)`,
			`CREATE INDEX IF NOT EXISTS idx_volumes_bound_agent_id ON volumes(bound_agent_id, created_at, id)`,
			`CREATE TABLE IF NOT EXISTS services (
				id STRING PRIMARY KEY,
				project_id STRING NOT NULL REFERENCES projects(id),
				name STRING NOT NULL,
				current_spec_revision INT8 NOT NULL,
				current_rollout_generation INT8 NOT NULL,
				allocated_agent_id STRING NOT NULL REFERENCES agents(id),
				current_resolved_image STRING NOT NULL DEFAULT '',
				last_successful_commit_sha STRING NOT NULL DEFAULT '',
				latest_build_id STRING NOT NULL DEFAULT '',
				created_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL,
				UNIQUE (project_id, name)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_services_project_created_at ON services(project_id, created_at, id)`,
			`CREATE INDEX IF NOT EXISTS idx_services_allocated_agent_id ON services(allocated_agent_id, created_at, id)`,
			`CREATE TABLE IF NOT EXISTS service_revisions (
				service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
				spec_revision INT8 NOT NULL,
				spec_json JSONB NOT NULL,
				created_at TIMESTAMPTZ NOT NULL,
				PRIMARY KEY (service_id, spec_revision)
			)`,
			`CREATE TABLE IF NOT EXISTS domain_bindings (
				hostname STRING PRIMARY KEY,
				project_id STRING NOT NULL REFERENCES projects(id),
				service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
				target_port INT8 NOT NULL DEFAULT 8080,
				created_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_domain_bindings_service_id ON domain_bindings(service_id, hostname)`,
			`CREATE TABLE IF NOT EXISTS allocations (
				id STRING PRIMARY KEY,
				service_id STRING NOT NULL UNIQUE REFERENCES services(id) ON DELETE CASCADE,
				project_id STRING NOT NULL REFERENCES projects(id),
				agent_id STRING NOT NULL REFERENCES agents(id),
				desired_spec_revision INT8 NOT NULL,
				applied_spec_revision INT8 NOT NULL,
				desired_rollout_generation INT8 NOT NULL,
				applied_rollout_generation INT8 NOT NULL,
				phase STRING NOT NULL,
				message STRING NOT NULL,
				allocation_ip STRING NOT NULL DEFAULT '',
				healthy_ports JSONB NOT NULL DEFAULT '[]',
				healthy BOOL NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_allocations_agent_id ON allocations(agent_id, updated_at, id)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_owner_subject_name_unique
			    ON projects(owner_subject, name)
			  WHERE kind = 'user'`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_system_key_unique ON projects(system_key) WHERE system_key IS NOT NULL`,
			`CREATE TABLE IF NOT EXISTS service_rollouts (
				service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
				rollout_generation INT8 NOT NULL,
				spec_revision INT8 NOT NULL,
				reason STRING NOT NULL,
				build_id STRING NOT NULL DEFAULT '',
				requested_by_subject STRING NOT NULL DEFAULT '',
				requested_by_email STRING NOT NULL DEFAULT '',
				created_at TIMESTAMPTZ NOT NULL,
				PRIMARY KEY (service_id, rollout_generation)
			)`,
			`CREATE TABLE IF NOT EXISTS builder_workers (
				id STRING PRIMARY KEY,
				name STRING NOT NULL,
				current_build_id STRING NOT NULL DEFAULT '',
				last_heartbeat_at TIMESTAMPTZ NOT NULL,
				created_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS build_runs (
				id STRING PRIMARY KEY,
				service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
				project_id STRING NOT NULL REFERENCES projects(id),
				commit_sha STRING NOT NULL,
				commit_message STRING NOT NULL DEFAULT '',
				commit_author STRING NOT NULL DEFAULT '',
				state STRING NOT NULL,
				image_digest STRING NOT NULL DEFAULT '',
				failure_reason STRING NOT NULL DEFAULT '',
				repo_owner STRING NOT NULL DEFAULT '',
				repo_name STRING NOT NULL DEFAULT '',
				installation_id INT8 NOT NULL DEFAULT 0,
				tracked_branch STRING NOT NULL DEFAULT '',
				dockerfile_path STRING NOT NULL DEFAULT '',
				context_dir STRING NOT NULL DEFAULT '',
				builder_id STRING NOT NULL DEFAULT '',
				target_rollout_generation INT8 NOT NULL DEFAULT 0,
				queued_at TIMESTAMPTZ NOT NULL,
				started_at TIMESTAMPTZ NULL,
				finished_at TIMESTAMPTZ NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_build_runs_service_queued_at
			    ON build_runs(service_id, queued_at DESC, id)`,
			`CREATE INDEX IF NOT EXISTS idx_build_runs_service_commit_queued_at
			    ON build_runs(service_id, commit_sha, queued_at DESC, id DESC)`,
			`CREATE INDEX IF NOT EXISTS idx_build_runs_state_queued_at
			    ON build_runs(state, queued_at ASC, id)`,
			`CREATE TABLE IF NOT EXISTS github_installations (
				installation_id INT8 PRIMARY KEY,
				account_login STRING NOT NULL,
				account_type STRING NOT NULL,
				target_type STRING NOT NULL,
				active BOOL NOT NULL,
				created_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS github_installation_repositories (
				installation_id INT8 NOT NULL REFERENCES github_installations(installation_id) ON DELETE CASCADE,
				repository_id INT8 NOT NULL,
				owner STRING NOT NULL,
				repo STRING NOT NULL,
				full_name STRING NOT NULL,
				private BOOL NOT NULL,
				default_branch STRING NOT NULL,
				created_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL,
				PRIMARY KEY (installation_id, repository_id),
				UNIQUE (installation_id, full_name)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_github_installation_repositories_full_name
			    ON github_installation_repositories(full_name, installation_id)`,
			`CREATE TABLE IF NOT EXISTS github_webhook_deliveries (
				id STRING PRIMARY KEY,
				delivery_id STRING NOT NULL UNIQUE,
				event_type STRING NOT NULL,
				state STRING NOT NULL,
				processor_id STRING NOT NULL DEFAULT '',
				payload JSONB NOT NULL,
				last_error STRING NOT NULL DEFAULT '',
				received_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL,
				processed_at TIMESTAMPTZ NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_github_webhook_deliveries_state_received_at
			    ON github_webhook_deliveries(state, received_at ASC, id)`,
			`CREATE TABLE IF NOT EXISTS service_build_sources (
				service_id STRING PRIMARY KEY REFERENCES services(id) ON DELETE CASCADE,
				project_id STRING NOT NULL REFERENCES projects(id),
				provider STRING NOT NULL,
				owner STRING NOT NULL,
				repo STRING NOT NULL,
				full_name STRING NOT NULL,
				tracked_branch STRING NOT NULL,
				installation_id INT8 NOT NULL DEFAULT 0,
				created_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_service_build_sources_lookup
			    ON service_build_sources(provider, owner, repo, tracked_branch, installation_id, service_id)`,
			`CREATE INDEX IF NOT EXISTS idx_service_build_sources_full_name
			    ON service_build_sources(full_name, installation_id, service_id)`,
			`CREATE TABLE IF NOT EXISTS github_repository_snapshots (
				full_name STRING PRIMARY KEY,
				repository_id INT8 NOT NULL DEFAULT 0,
				owner STRING NOT NULL,
				repo STRING NOT NULL,
				private BOOL NOT NULL DEFAULT FALSE,
				default_branch STRING NOT NULL DEFAULT '',
				deleted BOOL NOT NULL DEFAULT FALSE,
				created_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_github_repository_snapshots_owner_repo
			    ON github_repository_snapshots(owner, repo)`,
		},
	},
	{
		version: 2,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS github_work_items (
				id STRING PRIMARY KEY,
				kind STRING NOT NULL,
				state STRING NOT NULL,
				processor_id STRING NOT NULL DEFAULT '',
				idempotency_key STRING NOT NULL UNIQUE,
				service_id STRING NOT NULL DEFAULT '',
				spec_revision INT8 NOT NULL DEFAULT 0,
				installation_id INT8 NOT NULL DEFAULT 0,
				commit_sha STRING NOT NULL DEFAULT '',
				last_error STRING NOT NULL DEFAULT '',
				attempt_count INT8 NOT NULL DEFAULT 0,
				available_at TIMESTAMPTZ NOT NULL,
				created_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_github_work_items_state_available_at
			    ON github_work_items(state, available_at ASC, created_at ASC, id)`,
		},
	},
	{
		version: 3,
		stmts: []string{
			`ALTER TABLE build_runs ADD COLUMN IF NOT EXISTS source_revision_id STRING NOT NULL DEFAULT ''`,
			`ALTER TABLE build_runs ADD COLUMN IF NOT EXISTS source_snapshot_id STRING NOT NULL DEFAULT ''`,
			`ALTER TABLE build_runs ADD COLUMN IF NOT EXISTS source_snapshot_digest STRING NOT NULL DEFAULT ''`,
			`ALTER TABLE build_runs ADD COLUMN IF NOT EXISTS build_recipe_json JSONB NOT NULL DEFAULT '{}'`,
			`CREATE TABLE IF NOT EXISTS source_bindings (
				id STRING PRIMARY KEY,
				service_id STRING NOT NULL UNIQUE REFERENCES services(id) ON DELETE CASCADE,
				project_id STRING NOT NULL REFERENCES projects(id),
				provider STRING NOT NULL,
				repository_selector STRING NOT NULL DEFAULT '',
				tracked_ref STRING NOT NULL DEFAULT '',
				provider_repository_external_id STRING NOT NULL DEFAULT '',
				provider_scope_external_id STRING NOT NULL DEFAULT '',
				access_state STRING NOT NULL DEFAULT '',
				installation_id INT8 NOT NULL DEFAULT 0,
				build_recipe_json JSONB NOT NULL DEFAULT '{}',
				resolved_at TIMESTAMPTZ NOT NULL,
				fresh_until TIMESTAMPTZ NOT NULL,
				created_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_source_bindings_provider_repo_ref
			    ON source_bindings(provider, provider_repository_external_id, tracked_ref, service_id)`,
			`CREATE INDEX IF NOT EXISTS idx_source_bindings_installation
			    ON source_bindings(provider, installation_id, service_id)`,
			`CREATE TABLE IF NOT EXISTS source_revisions (
				id STRING PRIMARY KEY,
				source_binding_id STRING NOT NULL REFERENCES source_bindings(id) ON DELETE CASCADE,
				service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
				provider STRING NOT NULL,
				provider_repository_external_id STRING NOT NULL DEFAULT '',
				tracked_ref STRING NOT NULL DEFAULT '',
				commit_sha STRING NOT NULL,
				commit_message STRING NOT NULL DEFAULT '',
				commit_author STRING NOT NULL DEFAULT '',
				observed_at TIMESTAMPTZ NOT NULL,
				created_at TIMESTAMPTZ NOT NULL,
				UNIQUE (source_binding_id, commit_sha)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_source_revisions_service_observed
			    ON source_revisions(service_id, observed_at DESC, id)`,
			`CREATE INDEX IF NOT EXISTS idx_source_revisions_provider_repo_commit
			    ON source_revisions(provider, provider_repository_external_id, commit_sha, id)`,
			`CREATE TABLE IF NOT EXISTS source_snapshots (
				id STRING PRIMARY KEY,
				provider STRING NOT NULL,
				provider_repository_external_id STRING NOT NULL DEFAULT '',
				commit_sha STRING NOT NULL,
				digest STRING NOT NULL DEFAULT '',
				archive_tgz BYTES NOT NULL DEFAULT b'',
				ready BOOL NOT NULL DEFAULT FALSE,
				fetched_at TIMESTAMPTZ NULL,
				created_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL,
				UNIQUE (provider, provider_repository_external_id, commit_sha)
			)`,
			`CREATE TABLE IF NOT EXISTS source_work_items (
				id STRING PRIMARY KEY,
				kind STRING NOT NULL,
				state STRING NOT NULL,
				processor_id STRING NOT NULL DEFAULT '',
				idempotency_key STRING NOT NULL UNIQUE,
				service_id STRING NOT NULL DEFAULT '',
				spec_revision INT8 NOT NULL DEFAULT 0,
				provider STRING NOT NULL DEFAULT '',
				provider_repository_external_id STRING NOT NULL DEFAULT '',
				tracked_ref STRING NOT NULL DEFAULT '',
				commit_sha STRING NOT NULL DEFAULT '',
				commit_message STRING NOT NULL DEFAULT '',
				commit_author STRING NOT NULL DEFAULT '',
				installation_id INT8 NOT NULL DEFAULT 0,
				owner STRING NOT NULL DEFAULT '',
				repo STRING NOT NULL DEFAULT '',
				last_error STRING NOT NULL DEFAULT '',
				attempt_count INT8 NOT NULL DEFAULT 0,
				available_at TIMESTAMPTZ NOT NULL,
				created_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_source_work_items_state_available_at
			    ON source_work_items(state, available_at ASC, created_at ASC, id)`,
		},
	},
	{
		version: 4,
		stmts: []string{
			`DROP TABLE IF EXISTS service_build_sources`,
			`ALTER TABLE build_runs DROP COLUMN IF EXISTS repo_owner`,
			`ALTER TABLE build_runs DROP COLUMN IF EXISTS repo_name`,
			`ALTER TABLE build_runs DROP COLUMN IF EXISTS installation_id`,
			`ALTER TABLE build_runs DROP COLUMN IF EXISTS tracked_branch`,
			`ALTER TABLE build_runs DROP COLUMN IF EXISTS dockerfile_path`,
			`ALTER TABLE build_runs DROP COLUMN IF EXISTS context_dir`,
			`ALTER TABLE source_bindings DROP COLUMN IF EXISTS installation_id`,
			`DROP INDEX IF EXISTS idx_source_bindings_installation`,
			`CREATE INDEX IF NOT EXISTS idx_source_bindings_provider_scope
			    ON source_bindings(provider, provider_scope_external_id, service_id)`,
			`ALTER TABLE source_snapshots ADD COLUMN IF NOT EXISTS source_revision_id STRING NOT NULL DEFAULT ''`,
			`UPDATE source_snapshots
			    SET source_revision_id = COALESCE((
			      SELECT r.id
			        FROM source_revisions r
			       WHERE r.provider = source_snapshots.provider
			         AND r.provider_repository_external_id = source_snapshots.provider_repository_external_id
			         AND r.commit_sha = source_snapshots.commit_sha
			       ORDER BY r.created_at DESC, r.id DESC
			       LIMIT 1
			    ), source_revision_id)
			  WHERE source_revision_id = ''`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_source_snapshots_source_revision_id
			    ON source_snapshots(source_revision_id)`,
			`ALTER TABLE source_work_items ADD COLUMN IF NOT EXISTS provider_scope_external_id STRING NOT NULL DEFAULT ''`,
			`ALTER TABLE source_work_items DROP COLUMN IF EXISTS installation_id`,
			`ALTER TABLE source_work_items DROP COLUMN IF EXISTS owner`,
			`ALTER TABLE source_work_items DROP COLUMN IF EXISTS repo`,
		},
	},
	{
		version: 5,
		stmts: []string{
			`ALTER TABLE source_work_items ADD COLUMN IF NOT EXISTS commit_message STRING NOT NULL DEFAULT ''`,
			`ALTER TABLE source_work_items ADD COLUMN IF NOT EXISTS commit_author STRING NOT NULL DEFAULT ''`,
			`ALTER TABLE source_revisions ADD COLUMN IF NOT EXISTS commit_message STRING NOT NULL DEFAULT ''`,
			`ALTER TABLE source_revisions ADD COLUMN IF NOT EXISTS commit_author STRING NOT NULL DEFAULT ''`,
			`ALTER TABLE build_runs ADD COLUMN IF NOT EXISTS commit_message STRING NOT NULL DEFAULT ''`,
			`ALTER TABLE build_runs ADD COLUMN IF NOT EXISTS commit_author STRING NOT NULL DEFAULT ''`,
			`ALTER TABLE build_runs ADD COLUMN IF NOT EXISTS target_rollout_generation INT8 NOT NULL DEFAULT 0`,
			`ALTER TABLE service_rollouts ADD COLUMN IF NOT EXISTS build_id STRING NOT NULL DEFAULT ''`,
			`ALTER TABLE build_runs DROP CONSTRAINT IF EXISTS build_runs_service_id_commit_sha_key`,
			`DROP INDEX IF EXISTS build_runs_service_id_commit_sha_key`,
			`CREATE INDEX IF NOT EXISTS idx_build_runs_service_commit_queued_at
			    ON build_runs(service_id, commit_sha, queued_at DESC, id DESC)`,
		},
	},
}

func OpenStore(dbCfg config.DatabaseConfig, meshCfg config.ControlPlaneMeshConfig) (*Store, error) {
	normalizeDatabaseConfig(&dbCfg)
	db, err := sql.Open("pgx", dbCfg.URL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(dbCfg.MaxOpenConns)
	db.SetMaxIdleConns(dbCfg.MaxIdleConns)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	store := &Store{db: db, mesh: meshCfg}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INT8 PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL)`); err != nil {
			return fmt.Errorf("create schema_migrations: %w", err)
		}

		applied := make(map[int]struct{}, len(storeMigrations))
		rows, err := tx.QueryContext(ctx, `SELECT version FROM schema_migrations`)
		if err != nil {
			return fmt.Errorf("list schema migrations: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var version int
			if err := rows.Scan(&version); err != nil {
				return fmt.Errorf("scan schema migration version: %w", err)
			}
			applied[version] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate schema migrations: %w", err)
		}

		for _, migration := range storeMigrations {
			if _, ok := applied[migration.version]; ok {
				continue
			}
			for _, stmt := range migration.stmts {
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("apply schema migration %d: %w", migration.version, err)
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES ($1, $2)`, migration.version, time.Now().UTC()); err != nil {
				return fmt.Errorf("record schema migration %d: %w", migration.version, err)
			}
		}
		return nil
	})
}

func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	return crdb.ExecuteTx(ctx, s.db, nil, fn)
}

func (s *Store) EnsureBootstrap(ctx context.Context, bootstrap config.BootstrapConfig) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, user := range bootstrap.Users {
			if user.Subject == "" || user.Email == "" {
				continue
			}
			now := time.Now().UTC()
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO users(subject, email, created_at) VALUES ($1, $2, $3) ON CONFLICT(subject) DO UPDATE SET email=excluded.email`,
				user.Subject, user.Email, now,
			); err != nil {
				return fmt.Errorf("upsert bootstrap user %s: %w", user.Subject, err)
			}
			for _, projectName := range user.Projects {
				projectID, err := s.ensureUserProjectNamedQuerier(ctx, tx, user.Subject, projectName)
				if err != nil {
					return err
				}
				if err := s.ensureProjectOwnerMembershipQuerier(ctx, tx, user.Subject, projectID); err != nil {
					return fmt.Errorf("insert project membership: %w", err)
				}
			}
		}
		return nil
	})
}

func mustID() string {
	return uuid.NewString()
}

func (s *Store) desiredStateForAgent(ctx context.Context, agentID string) (*agentv1.DesiredNodeState, error) {
	revision, err := s.currentDesiredRevisionForAgent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	state := &agentv1.DesiredNodeState{
		AgentId:     agentID,
		Revision:    revision,
		GeneratedAt: ts(time.Now().UTC()),
	}
	vols, err := s.listDesiredVolumes(ctx, agentID)
	if err != nil {
		return nil, err
	}
	state.Volumes = vols
	services, err := s.listDesiredServices(ctx, agentID)
	if err != nil {
		return nil, err
	}
	state.Services = services
	nodeConfig, err := s.assignedNodeConfigForAgent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	state.NodeConfig = nodeConfig
	return state, nil
}

func (s *Store) currentDesiredRevisionForAgent(ctx context.Context, agentID string) (int64, error) {
	var value int64
	if err := s.db.QueryRowContext(ctx, `SELECT desired_revision FROM agents WHERE id = $1`, agentID).Scan(&value); err != nil {
		return 0, err
	}
	return value, nil
}

func (s *Store) bumpDesiredRevisionsTx(ctx context.Context, tx *sql.Tx, agentIDs []string) error {
	if len(agentIDs) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(agentIDs))
	uniqueIDs := make([]string, 0, len(agentIDs))
	for _, agentID := range agentIDs {
		agentID = strings.TrimSpace(agentID)
		if agentID == "" {
			continue
		}
		if _, ok := seen[agentID]; ok {
			continue
		}
		seen[agentID] = struct{}{}
		uniqueIDs = append(uniqueIDs, agentID)
	}
	if len(uniqueIDs) == 0 {
		return nil
	}
	sort.Strings(uniqueIDs)

	args := make([]any, 0, len(uniqueIDs))
	placeholders := make([]string, 0, len(uniqueIDs))
	for i, agentID := range uniqueIDs {
		args = append(args, agentID)
		var b strings.Builder
		b.Grow(len(strconv.Itoa(i+1)) + 1)
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(i + 1))
		placeholders = append(placeholders, b.String())
	}
	query := fmt.Sprintf(
		`UPDATE agents SET desired_revision = desired_revision + 1 WHERE id IN (%s)`,
		strings.Join(placeholders, ", "),
	)
	_, err := tx.ExecContext(ctx, query, args...)
	return err
}

func (s *Store) bumpAllDesiredRevisionsTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE agents SET desired_revision = desired_revision + 1`)
	return err
}

func (s *Store) bumpDesiredRevisions(ctx context.Context, agentIDs []string) error {
	if len(agentIDs) == 0 {
		return nil
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		return s.bumpDesiredRevisionsTx(ctx, tx, agentIDs)
	})
}

func normalizeDatabaseConfig(dbCfg *config.DatabaseConfig) {
	if dbCfg == nil {
		return
	}
	if dbCfg.MaxOpenConns <= 0 {
		dbCfg.MaxOpenConns = maxInt(32, runtime.GOMAXPROCS(0)*8)
	}
	if dbCfg.MaxIdleConns <= 0 {
		dbCfg.MaxIdleConns = minInt(dbCfg.MaxOpenConns, maxInt(16, runtime.GOMAXPROCS(0)*4))
	}
	if dbCfg.MaxIdleConns > dbCfg.MaxOpenConns {
		dbCfg.MaxIdleConns = dbCfg.MaxOpenConns
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
