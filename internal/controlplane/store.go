package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
	"slices"
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

type Store struct {
	db                      *sql.DB
	mesh                    config.ControlPlaneMeshConfig
	reservedAgentIDs        []string
	sourceArchives          SourceArchiveStore
	useReportedAllocationIP bool
}

func (s *Store) reserveAgents(agentIDs ...string) {
	for _, agentID := range agentIDs {
		agentID = strings.TrimSpace(agentID)
		if agentID != "" && !slices.Contains(s.reservedAgentIDs, agentID) {
			s.reservedAgentIDs = append(s.reservedAgentIDs, agentID)
		}
	}
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

func (s *Store) Ready(ctx context.Context) (databaseOK, migrationsOK bool) {
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

func (s *Store) migrate(ctx context.Context) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
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
			current := 0
			for version := range applied {
				if version > current {
					current = version
				}
			}
			if current == currentSchemaVersion && len(applied) == 1 {
				return nil
			}
			if err := applySchemaUpgrades(ctx, tx, current); err != nil {
				return err
			}
			return nil
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

func applySchemaUpgrades(ctx context.Context, tx *sql.Tx, fromVersion int) error {
	if fromVersion <= 0 {
		return fmt.Errorf("database schema is stale; recreate the database")
	}
	for version := fromVersion + 1; version <= currentSchemaVersion; version++ {
		stmts, ok := schemaUpgrades[version]
		if !ok {
			return fmt.Errorf("database schema is stale; recreate the database")
		}
		for _, stmt := range stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("upgrade schema to %d: %w", version, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = $1`, version-1); err != nil {
			return fmt.Errorf("replace schema version %d: %w", version-1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES ($1, $2)`, version, time.Now().UTC()); err != nil {
			return fmt.Errorf("record schema version %d: %w", version, err)
		}
	}
	return nil
}

func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	return crdb.ExecuteTx(ctx, s.db, nil, fn)
}

func (s *Store) EnsureBootstrap(ctx context.Context, bootstrap config.BootstrapConfig) error {
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
