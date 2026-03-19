package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
	"sort"
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
				name STRING NOT NULL UNIQUE,
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
				updated_at TIMESTAMPTZ NOT NULL
			)`,
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
				current_revision INT8 NOT NULL,
				allocated_agent_id STRING NOT NULL REFERENCES agents(id),
				created_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL,
				UNIQUE (project_id, name)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_services_project_created_at ON services(project_id, created_at, id)`,
			`CREATE INDEX IF NOT EXISTS idx_services_allocated_agent_id ON services(allocated_agent_id, created_at, id)`,
			`CREATE TABLE IF NOT EXISTS service_revisions (
				service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
				revision INT8 NOT NULL,
				spec_json JSONB NOT NULL,
				created_at TIMESTAMPTZ NOT NULL,
				PRIMARY KEY (service_id, revision)
			)`,
			`CREATE TABLE IF NOT EXISTS service_domains (
				domain STRING PRIMARY KEY,
				project_id STRING NOT NULL REFERENCES projects(id),
				service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
				created_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_service_domains_service_id ON service_domains(service_id, domain)`,
			`CREATE TABLE IF NOT EXISTS allocations (
				id STRING PRIMARY KEY,
				service_id STRING NOT NULL UNIQUE REFERENCES services(id) ON DELETE CASCADE,
				project_id STRING NOT NULL REFERENCES projects(id),
				agent_id STRING NOT NULL REFERENCES agents(id),
				desired_revision INT8 NOT NULL,
				applied_revision INT8 NOT NULL,
				phase STRING NOT NULL,
				message STRING NOT NULL,
				endpoint_addr STRING NOT NULL,
				healthy BOOL NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_allocations_agent_id ON allocations(agent_id, updated_at, id)`,
			`CREATE TABLE IF NOT EXISTS state_revisions (
				name STRING PRIMARY KEY,
				value INT8 NOT NULL
			)`,
			`INSERT INTO state_revisions(name, value) VALUES ('desired', 0) ON CONFLICT(name) DO NOTHING`,
		},
	},
	{
		version: 2,
		stmts: []string{
			`ALTER TABLE projects ADD COLUMN IF NOT EXISTS kind STRING NOT NULL DEFAULT 'user'`,
			`ALTER TABLE projects ADD COLUMN IF NOT EXISTS system_key STRING NULL`,
			`UPDATE projects SET kind = 'user' WHERE kind = ''`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_system_key_unique ON projects(system_key) WHERE system_key IS NOT NULL`,
		},
	},
	{
		version: 3,
		stmts: []string{
			`ALTER TABLE services RENAME COLUMN current_revision TO current_spec_revision`,
			`ALTER TABLE services ADD COLUMN IF NOT EXISTS current_rollout_generation INT8`,
			`UPDATE services SET current_rollout_generation = current_spec_revision WHERE current_rollout_generation IS NULL`,
			`ALTER TABLE services ALTER COLUMN current_rollout_generation SET NOT NULL`,
			`ALTER TABLE service_revisions RENAME COLUMN revision TO spec_revision`,
			`CREATE TABLE IF NOT EXISTS service_rollouts (
				service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
				rollout_generation INT8 NOT NULL,
				spec_revision INT8 NOT NULL,
				reason STRING NOT NULL,
				requested_by_subject STRING NOT NULL DEFAULT '',
				requested_by_email STRING NOT NULL DEFAULT '',
				created_at TIMESTAMPTZ NOT NULL,
				PRIMARY KEY (service_id, rollout_generation)
			)`,
			`INSERT INTO service_rollouts(service_id, rollout_generation, spec_revision, reason, created_at)
			 SELECT service_id, spec_revision, spec_revision, 'migration', created_at
			   FROM service_revisions
			 ON CONFLICT(service_id, rollout_generation) DO NOTHING`,
			`ALTER TABLE allocations RENAME COLUMN desired_revision TO desired_spec_revision`,
			`ALTER TABLE allocations RENAME COLUMN applied_revision TO applied_spec_revision`,
			`ALTER TABLE allocations ADD COLUMN IF NOT EXISTS desired_rollout_generation INT8`,
			`ALTER TABLE allocations ADD COLUMN IF NOT EXISTS applied_rollout_generation INT8`,
			`UPDATE allocations
			    SET desired_rollout_generation = desired_spec_revision,
			        applied_rollout_generation = applied_spec_revision
			  WHERE desired_rollout_generation IS NULL OR applied_rollout_generation IS NULL`,
			`ALTER TABLE allocations ALTER COLUMN desired_rollout_generation SET NOT NULL`,
			`ALTER TABLE allocations ALTER COLUMN applied_rollout_generation SET NOT NULL`,
			`ALTER TABLE service_domains RENAME TO domain_bindings`,
			`ALTER TABLE domain_bindings RENAME COLUMN domain TO hostname`,
			`ALTER TABLE domain_bindings ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ`,
			`UPDATE domain_bindings SET updated_at = created_at WHERE updated_at IS NULL`,
			`ALTER TABLE domain_bindings ALTER COLUMN updated_at SET NOT NULL`,
			`CREATE INDEX IF NOT EXISTS idx_domain_bindings_service_id ON domain_bindings(service_id, hostname)`,
			`ALTER TABLE agents ADD COLUMN IF NOT EXISTS desired_revision INT8 NOT NULL DEFAULT 0`,
			`UPDATE agents
			    SET desired_revision = COALESCE((SELECT value FROM state_revisions WHERE name = 'desired'), 0)`,
			`CREATE INDEX IF NOT EXISTS idx_agents_last_seen_id
			    ON agents(last_seen_at DESC, id)
			    STORING (cpu_millis_capacity, memory_mebibytes_capacity)`,
		},
	},
	{
		version: 4,
		stmts: []string{
			`ALTER TABLE projects ADD COLUMN IF NOT EXISTS owner_subject STRING NOT NULL DEFAULT ''`,
			`UPDATE projects
			    SET owner_subject = COALESCE((
			        SELECT subject
			          FROM project_memberships m
			         WHERE m.project_id = projects.id AND m.role = 'owner'
			         ORDER BY subject ASC
			         LIMIT 1
			    ), '')
			  WHERE kind = 'user' AND owner_subject = ''`,
			`DROP INDEX IF EXISTS projects@projects_name_key CASCADE`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_owner_subject_name_unique
			    ON projects(owner_subject, name)
			  WHERE kind = 'user'`,
		},
	},
	{
		version: 5,
		stmts: []string{
			`UPDATE projects
			    SET owner_subject = COALESCE((
			        SELECT subject
			          FROM project_memberships m
			         WHERE m.project_id = projects.id AND m.role = 'owner'
			         ORDER BY subject ASC
			         LIMIT 1
			    ), '')
			  WHERE kind = 'user' AND owner_subject = ''`,
			`INSERT INTO project_memberships(subject, project_id, role)
			    SELECT owner_subject, id, 'owner'
			      FROM projects
			     WHERE kind = 'user' AND owner_subject <> ''
			  ON CONFLICT(subject, project_id) DO UPDATE SET role = excluded.role`,
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
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
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
