package controlplane

import (
	"context"
	"database/sql"
	"fmt"
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
}

func OpenStore(dbCfg config.DatabaseConfig, meshCfg config.ControlPlaneMeshConfig) (*Store, error) {
	db, err := sql.Open("pgx", dbCfg.URL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(16)
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
				projectID, err := s.ensureProjectNamedQuerier(ctx, tx, projectName, projectKindUser, "")
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO project_memberships(subject, project_id, role) VALUES ($1, $2, $3) ON CONFLICT(subject, project_id) DO NOTHING`,
					user.Subject, projectID, "owner",
				); err != nil {
					return fmt.Errorf("insert project membership: %w", err)
				}
			}
		}
		return nil
	})
}

func (s *Store) nextDesiredRevision(ctx context.Context) (int64, error) {
	var value int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		next, err := s.nextDesiredRevisionTx(ctx, tx)
		if err != nil {
			return err
		}
		value = next
		return nil
	})
	if err != nil {
		return 0, err
	}
	return value, nil
}

func (s *Store) nextDesiredRevisionTx(ctx context.Context, tx *sql.Tx) (int64, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE state_revisions SET value = value + 1 WHERE name = 'desired'`); err != nil {
		return 0, err
	}
	var value int64
	if err := tx.QueryRowContext(ctx, `SELECT value FROM state_revisions WHERE name = 'desired'`).Scan(&value); err != nil {
		return 0, err
	}
	return value, nil
}

func mustID() string {
	return uuid.NewString()
}

func (s *Store) desiredStateForAgent(ctx context.Context, agentID string) (*agentv1.DesiredNodeState, error) {
	revision, err := s.currentDesiredRevision(ctx)
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

func (s *Store) currentDesiredRevision(ctx context.Context) (int64, error) {
	var value int64
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM state_revisions WHERE name = 'desired'`).Scan(&value); err != nil {
		return 0, err
	}
	return value, nil
}
