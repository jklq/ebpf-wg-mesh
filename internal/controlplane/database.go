package controlplane

import (
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/journal"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/cockroach-go/v2/crdb"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type database struct {
	db               *sql.DB
	mesh             config.ControlPlaneMeshConfig
	reservedAgentIDs []string
	journalOnce      sync.Once
	journal          *journal.Store
}

func (s *database) reserveAgents(agentIDs ...string) {
	for _, agentID := range agentIDs {
		agentID = strings.TrimSpace(agentID)
		if agentID != "" && !slices.Contains(s.reservedAgentIDs, agentID) {
			s.reservedAgentIDs = append(s.reservedAgentIDs, agentID)
		}
	}
}

func openPersistence(dbCfg config.DatabaseConfig, meshCfg config.ControlPlaneMeshConfig) (*persistence, error) {
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

	store := newPersistence(&database{db: db, mesh: meshCfg})
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	store.initJournal()
	if _, err := store.journal.Snapshot(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("replay cluster journal: %w", err)
	}
	if err := store.fleet.validateWorkloadIPv4Pool(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *database) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *database) Ready(ctx context.Context) (databaseOK, migrationsOK bool) {
	if s == nil || s.db == nil {
		return false, false
	}
	if err := s.db.PingContext(ctx); err != nil {
		return false, false
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return true, false
	}
	return true, version >= currentSchemaVersion
}

func (s *database) migrate(ctx context.Context) error {
	return s.withTxUnfenced(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INT8 PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL)`); err != nil {
			return fmt.Errorf("create schema_migrations: %w", err)
		}

		applied := make(map[int]struct{})
		rows, err := tx.QueryContext(ctx, `SELECT version FROM schema_migrations`)
		if err != nil {
			return fmt.Errorf("list schema versions: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var version int
			if err := rows.Scan(&version); err != nil {
				return fmt.Errorf("scan schema version: %w", err)
			}
			applied[version] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate schema versions: %w", err)
		}
		if len(applied) > 0 {
			if len(applied) == 1 {
				if _, ok := applied[currentSchemaVersion]; ok {
					return nil
				}
			}
			return fmt.Errorf("database schema is stale; recreate the database")
		}

		for _, stmt := range currentSchema {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("apply schema: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES ($1, $2)`, currentSchemaVersion, time.Now().UTC()); err != nil {
			return fmt.Errorf("record schema version: %w", err)
		}
		return nil
	})
}

func (s *database) initJournal() {
	s.journalOnce.Do(func() {
		s.journal = journal.New(s.db, "default", func(ctx context.Context, tx *sql.Tx) (int64, error) {
			if err := assertLeaseTx(ctx, tx); err != nil {
				return 0, err
			}
			var epoch int64
			err := tx.QueryRowContext(ctx, `SELECT epoch FROM agent_authority WHERE id = 1 FOR UPDATE`).Scan(&epoch)
			return epoch, err
		})
	})
}

func (s *database) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	s.initJournal()
	_, err := s.journal.Execute(ctx, func(tx *sql.Tx) error {
		if err := assertLeaseTx(ctx, tx); err != nil {
			return err
		}
		// Hold command authority throughout planning as well as append. A
		// cutover cannot relabel a plan computed before it with the new epoch.
		var epoch int64
		if err := tx.QueryRowContext(ctx, `SELECT epoch FROM agent_authority WHERE id = 1 FOR UPDATE`).Scan(&epoch); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		return bumpGlobalEnvironmentEventTx(ctx, tx)
	})
	return err
}

func (s *database) withTxUnfenced(ctx context.Context, fn func(*sql.Tx) error) error {
	return crdb.ExecuteTx(ctx, s.db, nil, fn)
}

func (s *database) withObservationTx(ctx context.Context, fn func(*sql.Tx) error) error {
	return s.withTxUnfenced(ctx, func(tx *sql.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return bumpGlobalEnvironmentEventTx(ctx, tx)
	})
}

func (s *database) withCommittedState(ctx context.Context, fn func(*sql.Tx) error) error {
	s.initJournal()
	return s.journal.Read(ctx, func(tx *sql.Tx, _ journal.DurableState) error { return fn(tx) })
}

func (s *database) readLiveState(ctx context.Context, fn func(*sql.Tx, journal.DurableState) error) error {
	s.initJournal()
	return s.journal.Read(ctx, fn)
}

func (s *catalogPersistence) EnsureBootstrap(ctx context.Context, bootstrap config.BootstrapConfig) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, user := range bootstrap.Users {
			if user.ID == "" {
				continue
			}
			for _, projectName := range user.Projects {
				projectID, err := s.ensureUserProjectNamedQuerier(ctx, tx, user.ID, projectName)
				if err != nil {
					return err
				}
				if err := s.ensureProjectOwnerMembershipQuerier(ctx, tx, user.ID, projectID); err != nil {
					return fmt.Errorf("insert project membership: %w", err)
				}
			}
			if user.Operator {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO platform_operators(user_id, created_at) VALUES ($1, $2)
					 ON CONFLICT(user_id) DO NOTHING`, user.ID, time.Now().UTC()); err != nil {
					return fmt.Errorf("insert platform operator: %w", err)
				}
			}
		}
		return nil
	})
}

func normalizeDatabaseConfig(dbCfg *config.DatabaseConfig) {
	if dbCfg == nil {
		return
	}
	if dbCfg.MaxOpenConns <= 0 {
		dbCfg.MaxOpenConns = max(32, runtime.GOMAXPROCS(0)*8)
	}
	if dbCfg.MaxIdleConns <= 0 {
		dbCfg.MaxIdleConns = min(dbCfg.MaxOpenConns, max(16, runtime.GOMAXPROCS(0)*4))
	}
	if dbCfg.MaxIdleConns > dbCfg.MaxOpenConns {
		dbCfg.MaxIdleConns = dbCfg.MaxOpenConns
	}
}
