package delivery

import (
	"context"
	"database/sql"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/source"
)

// Transaction runs a retryable, lease-fenced transaction and advances the
// durable event index in the same commit.
type Transaction func(context.Context, func(*sql.Tx) error) error

type ServiceQueryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type UserIdentity struct{ UserID string }

// Events reads the global revision advanced atomically by every committed
// transaction. Delivery never publishes: the bump already happened in withTx
// by the time these methods run, so callers only read.
type Events interface {
	Current(context.Context) (int64, error)
}

// Dependencies supplies connection infrastructure, transactional catalog/source
// operations, and post-commit effects. Catalog/source callbacks must use the
// supplied transaction so environment copies and source queues commit with delivery.
// Delivery never lends its persistence or transaction helpers to callers.
type Dependencies struct {
	CreateEnvironment func(context.Context, ServiceQueryer, string, string, bool, string) (EnvironmentRecord, error)
	CreateVolume      func(context.Context, *sql.Tx, string, string, string, int64) (VolumeRecord, error)
	EnqueueSourceWork func(context.Context, *sql.Tx, source.SourceWorkItemRecord) (bool, error)
	// SourceStore supplies source-table reads/writes scoped to delivery's
	// transactions. Delivery never touches source tables directly.
	SourceStore SourceStore

	DB                     *sql.DB
	Mesh                   config.ControlPlaneMeshConfig
	Transaction            Transaction
	UnfencedTransaction    Transaction
	ObservationTransaction Transaction
	ReadState              func(context.Context, func(*sql.Tx, journal.DurableState) error) error
	UserFromContext        func(context.Context) (UserIdentity, error)
	Notifier               PlatformNotifier
	Ingress                PlatformIngress
	Events                 Events
	// LogEmitter is optional; when set, delivery emits synthetic build/deploy
	// log lines for the builds it queues. A nil emitter is a no-op.
	LogEmitter       *logs.LogEmitter
	ReservedAgentIDs []string
}

// SourceStore is the source-table surface delivery needs inside its own
// transactions. It is implemented by *source.SQLStore.
type SourceStore interface {
	SourceBindingByServiceIDQuerier(context.Context, source.Querier, string) (source.SourceBindingRecord, error)
	SourceRevisionByBindingAndCommitTx(context.Context, source.Querier, string, string) (source.SourceRevisionRecord, error)
	SourceRevisionByIDTx(context.Context, source.Querier, string) (source.SourceRevisionRecord, error)
	SourceSnapshotByRevisionIDTx(context.Context, source.Querier, string) (source.SourceSnapshotRecord, error)
	LatestSourceRevisionByBindingIDTx(context.Context, source.Querier, string) (source.SourceRevisionRecord, error)
	UpsertSourceSnapshotTx(context.Context, *sql.Tx, source.SourceSnapshotRecord) (source.SourceSnapshotRecord, error)
}

type persistence struct {
	createEnvironmentQuerier func(context.Context, ServiceQueryer, string, string, bool, string) (EnvironmentRecord, error)
	createVolumeTx           func(context.Context, *sql.Tx, string, string, string, int64) (VolumeRecord, error)
	enqueueSourceWorkItemTx  func(context.Context, *sql.Tx, source.SourceWorkItemRecord) (bool, error)
	sourceStore              SourceStore

	db                *sql.DB
	mesh              config.ControlPlaneMeshConfig
	reservedAgentIDs  []string
	withTx            Transaction
	withTxUnfenced    Transaction
	withObservationTx Transaction
	readState         func(context.Context, func(*sql.Tx, journal.DurableState) error) error
}

func New(deps Dependencies) *Delivery {
	return &Delivery{
		store: &persistence{
			db:                       deps.DB,
			mesh:                     deps.Mesh,
			reservedAgentIDs:         append([]string(nil), deps.ReservedAgentIDs...),
			withTx:                   deps.Transaction,
			withTxUnfenced:           deps.UnfencedTransaction,
			withObservationTx:        deps.ObservationTransaction,
			readState:                deps.ReadState,
			createEnvironmentQuerier: deps.CreateEnvironment,
			createVolumeTx:           deps.CreateVolume,
			enqueueSourceWorkItemTx:  deps.EnqueueSourceWork,
			sourceStore:              deps.SourceStore,
		},
		notifier:        deps.Notifier,
		ingress:         deps.Ingress,
		events:          deps.Events,
		logEmitter:      deps.LogEmitter,
		userFromContext: deps.UserFromContext,
	}
}

// ReadModel exposes the snapshots used by transports and other modules.
// Its persistence remains private to delivery.
type ReadModel interface {
	ListAgents(context.Context) ([]AgentRecord, error)
	AgentByID(context.Context, string) (AgentRecord, error)
	AgentIDs(context.Context) ([]string, error)
	ListServiceDeployments(context.Context, string, string, int32) ([]DeploymentRecord, error)
	ListDomainBindings(context.Context, string, string) ([]DomainBindingRecord, error)
	ServiceStatus(context.Context, string, string) (ServiceRecord, []AllocationRecord, error)
	EnvironmentByID(context.Context, string, string) (EnvironmentRecord, error)
	AuthorizeOperator(context.Context, string) error
	ListAllocationsByServiceID(context.Context, string) ([]AllocationRecord, error)
	ListServices(context.Context, string, string) ([]ServiceRecord, error)
	ServiceByID(context.Context, string, string) (ServiceRecord, error)
	BuildByID(context.Context, string) (BuildRunRecord, error)
	ServiceSnapshot(context.Context, string) (ServiceRecord, error)
}

type readModel struct{ store *persistence }

func (d *Delivery) ReadModel() ReadModel { return &readModel{store: d.store} }

// Clock overrides are useful for deterministic reconciliation tests.
func (d *Delivery) SetClocks(rollout, failover func() time.Time) {
	d.rolloutNow = rollout
	d.failoverNow = failover
}
