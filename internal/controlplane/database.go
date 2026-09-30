package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/source"

	"github.com/cockroachdb/cockroach-go/v2/crdb"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type database struct {
	db               *sql.DB
	mesh             config.ControlPlaneMeshConfig
	reservedAgentIDs []string
	deletionGrace    time.Duration
	journalOnce      sync.Once
	journal          *journal.Store
}

// persistenceOption customizes persistence construction.
type persistenceOption func(*database)

// withDeletionGracePeriod overrides how long tombstones stay restorable.
// Zero selects deliverycore.DefaultDeletionGracePeriod.
func withDeletionGracePeriod(grace time.Duration) persistenceOption {
	return func(db *database) {
		db.deletionGrace = grace
	}
}

func (s *database) deletionGracePeriod() time.Duration {
	if s == nil || s.deletionGrace <= 0 {
		return deliverycore.DefaultDeletionGracePeriod
	}
	return s.deletionGrace
}

func (s *database) reserveAgents(agentIDs ...string) {
	for _, agentID := range agentIDs {
		agentID = strings.TrimSpace(agentID)
		if agentID != "" && !slices.Contains(s.reservedAgentIDs, agentID) {
			s.reservedAgentIDs = append(s.reservedAgentIDs, agentID)
		}
	}
}

func openPersistence(dbCfg config.DatabaseConfig, meshCfg config.ControlPlaneMeshConfig, opts ...persistenceOption) (*persistence, error) {
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

	handle := &database{db: db, mesh: meshCfg}
	for _, opt := range opts {
		if opt != nil {
			opt(handle)
		}
	}
	store := newPersistence(handle)
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
	return s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
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

func (s *database) withProductTx(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
	s.initJournal()
	_, err := s.journal.ExecuteWithMutation(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := assertLeaseTx(ctx, tx); err != nil {
			return err
		}
		if err := fn(ctx, tx); err != nil {
			return err
		}
		return nil
	}, func(ctx context.Context, tx *sql.Tx, base *journal.Projection, batch journal.Batch) error {
		if batch.Empty() {
			return nil
		}
		if err := s.bumpAffectedAgents(ctx, tx, base, batch); err != nil {
			return err
		}
		return bumpGlobalEnvironmentEventTx(ctx, tx)
	})
	return err
}

func (s *database) compactJournal(ctx context.Context, retain int64) (int64, error) {
	s.initJournal()
	return s.journal.Compact(ctx, retain, func(ctx context.Context, tx *sql.Tx) error {
		return assertLeaseTx(ctx, tx)
	})
}

func (s *database) withCoordinationTx(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
	return crdb.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error { return fn(ctx, tx) })
}

func (s *database) withObservationTx(ctx context.Context, fn func(context.Context, *sql.Tx) (bool, error)) error {
	return s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		changed, err := fn(ctx, tx)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
		return bumpGlobalEnvironmentEventTx(ctx, tx)
	})
}

func (s *database) withCommittedState(ctx context.Context, fn func(*sql.Tx) error) error {
	s.initJournal()
	return s.journal.Read(ctx, func(tx *sql.Tx, _ *journal.Projection) error { return fn(tx) })
}

func (s *database) readLiveState(ctx context.Context, fn func(*sql.Tx, *journal.Projection) error) error {
	s.initJournal()
	return s.journal.Read(ctx, fn)
}

func (s *catalogPersistence) EnsureBootstrap(ctx context.Context, bootstrap config.BootstrapConfig) error {
	return s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
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

type persistence struct {
	*database
	liveImplementation *deliverycore.Live
	notifications      liveNotifications
	publication        publicationFence
	catalog            *catalogPersistence
	fleet              *fleetPersistence
	reads              deliverycore.ReadModel
	routing            *routingPersistence
	source             *source.SQLStore
	secrets            *secretkeys.Service
}

// attachSecrets wires the sealed-secret backend. Production always attaches
// before serving; delivery skips sealed handling while it is nil.
func (p *persistence) attachSecrets(svc *secretkeys.Service) {
	p.secrets = svc
}

type catalogPersistence struct {
	*database
	authz  *authz.Authorizer
	source deliverycore.SourceStore
}

type fleetPersistence struct {
	*database
	authz    *authz.Authorizer
	sessions agentSessions
	live     fleetLiveReader
	reads    deliverycore.ReadModel
}

type routingPersistence struct {
	*database
	authz *authz.Authorizer
	live  ingressLiveReader
}

func (s *routingPersistence) WithLeaseGuard(ctx context.Context, fn func() error) error {
	return s.withLeaseGuard(ctx, fn)
}

func newPersistence(db *database) *persistence {
	live := deliverycore.NewLive()
	db.initJournal()
	db.journal.SetOnApplied(live.ApplyProduct)
	p := &persistence{database: db, liveImplementation: live, notifications: live, publication: live}
	authorizer := authz.NewAuthorizer(db.db)
	p.catalog = &catalogPersistence{database: db, authz: authorizer}
	p.fleet = &fleetPersistence{database: db, authz: authorizer, sessions: live, live: live}
	p.routing = &routingPersistence{database: db, authz: authorizer, live: live}
	p.source = source.NewSQLStore(db.db, db.withCoordinationTx, func(ctx context.Context, serviceID string) (source.Service, error) {
		rec, err := p.reads.ServiceSnapshot(ctx, serviceID)
		if err != nil {
			return source.Service{}, err
		}
		return source.Service{ID: rec.ID, ProjectID: rec.ProjectID, Spec: rec.Spec, SpecRevision: rec.SpecRevision, Deleted: rec.Deletion != nil}, nil
	})
	p.catalog.source = p.source
	return p
}

// authorizer returns the persistence-wide Authorizer. All scopes mint from this
// single instance; nothing constructs a second one per handle.
func (p *persistence) authorizer() *authz.Authorizer { return p.catalog.authz }

type platformPersistence struct {
	*catalogPersistence
	*routingPersistence
	deliverycore.ReadModel
}

func (p *persistence) platform() platformPersistence {
	return platformPersistence{p.catalog, p.routing, p.reads}
}

func newDeliveryWithScheduler(store *persistence, scheduler *deliverycore.BuildSchedulerConfig, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, events *platformEvents, logEmitter *logs.LogEmitter) *deliverycore.Delivery {
	deps := deliveryDependencies(store, notifier, ingress, events, logEmitter)
	if scheduler != nil {
		deps.BuildScheduler = *scheduler
	}
	d := deliverycore.New(deps)
	store.reads = d
	store.fleet.reads = d
	return d
}

func buildSchedulerConfigFromControlPlane(cfg config.ControlPlaneBuilderConfig) deliverycore.BuildSchedulerConfig {
	return deliverycore.BuildSchedulerConfig{
		LeaseTTL:                time.Duration(cfg.LeaseTTLSeconds) * time.Second,
		AttemptLimit:            int64(cfg.MaxAttempts),
		MaxConcurrentGlobal:     cfg.MaxConcurrentGlobal,
		MaxConcurrentPerProject: cfg.MaxConcurrentPerProject,
		BuildTimeout:            time.Duration(cfg.BuildTimeoutSeconds) * time.Second,
		MaxQueueAge:             time.Duration(cfg.MaxQueueAgeSeconds) * time.Second,
	}.WithDefaults()
}

func deliveryDependencies(store *persistence, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, events *platformEvents, logEmitter *logs.LogEmitter) deliverycore.Dependencies {
	return deliverycore.Dependencies{
		CreateEnvironment: store.catalog.createEnvironmentQuerier, CreateVolume: store.catalog.createVolumeTx, EnqueueSourceWork: store.source.Work().EnqueueTx, SourceStore: store.source,
		DB: store.db, Mesh: store.mesh, Live: store.liveImplementation, ProductTransaction: store.withProductTx,
		ObservationTransaction: store.withObservationTx,
		ReadState:              store.readLiveState,
		ReservedAgentIDs:       store.reservedAgentIDs,
		Notifier:               notifier, Ingress: ingress, Events: events, LogEmitter: logEmitter,
		Authorizer: store.authorizer(), Secrets: store.secrets, DeletionGracePeriod: store.deletionGracePeriod(),
	}
}

const controlPlaneStorageMarker = ".control-plane-storage-id"

func verifySharedControlPlaneDirectory(ctx context.Context, store *persistence, name, directory string) error {
	if store == nil || store.db == nil {
		return errors.New("control-plane store is required")
	}
	directory = filepath.Clean(strings.TrimSpace(directory))
	if directory == "" || directory == "." {
		return fmt.Errorf("control-plane %s directory is required", name)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create control-plane %s directory: %w", name, err)
	}
	markerPath := filepath.Join(directory, controlPlaneStorageMarker)
	storageID, err := readOrCreateStorageMarker(markerPath)
	if err != nil {
		return fmt.Errorf("initialize control-plane %s storage marker: %w", name, err)
	}

	var registered string
	err = store.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO control_plane_storage(name, storage_id, created_at)
			VALUES ($1, $2, statement_timestamp()) ON CONFLICT(name) DO NOTHING`, name, storageID); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT storage_id FROM control_plane_storage WHERE name = $1`, name).Scan(&registered)
	})
	if err != nil {
		return fmt.Errorf("register control-plane %s storage: %w", name, err)
	}
	if registered != storageID {
		return fmt.Errorf("control-plane %s directory is not the shared directory registered by this database", name)
	}
	return nil
}

func readOrCreateStorageMarker(path string) (string, error) {
	if raw, err := os.ReadFile(path); err == nil {
		return validateStorageID(raw)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	storageID := uuid.NewString()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", readErr
		}
		return validateStorageID(raw)
	}
	if err != nil {
		return "", err
	}
	if _, err := file.WriteString(storageID + "\n"); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return storageID, nil
}

func validateStorageID(raw []byte) (string, error) {
	storageID := strings.TrimSpace(string(raw))
	if _, err := uuid.Parse(storageID); err != nil {
		return "", fmt.Errorf("invalid storage marker: %w", err)
	}
	return storageID, nil
}
