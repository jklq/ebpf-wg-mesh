package delivery

import (
	"context"
	"database/sql"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/durablework"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/source"
)

type Transaction func(context.Context, func(context.Context, *sql.Tx) error) error

// ObservationTransaction runs fn in a transaction that bumps the global
// status revision when fn reports a change.
type ObservationTransaction func(context.Context, func(context.Context, *sql.Tx) (bool, error)) error

type ServiceQueryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Events interface {
	Current(context.Context) (int64, error)
}

type Dependencies struct {
	CreateEnvironment func(context.Context, ServiceQueryer, string, string, bool, string) (EnvironmentRecord, error)
	CreateVolume      func(context.Context, *sql.Tx, authz.Project, string, string, int64) (VolumeRecord, error)
	EnqueueSourceWork func(context.Context, *sql.Tx, durablework.EnqueueParams) (bool, error)
	// SourceStore supplies source-table reads/writes scoped to delivery's
	// transactions. Delivery never touches source tables directly.
	SourceStore SourceStore

	DB                 *sql.DB
	Mesh               config.ControlPlaneMeshConfig
	Live               *Live
	ProductTransaction Transaction
	// ObservationTransaction publishes status invalidations for observation
	// changes that carry no product work. It may be nil in tests.
	ObservationTransaction ObservationTransaction
	ReadState              func(context.Context, func(*sql.Tx, journal.DurableState) error) error
	Authorizer             *authz.Authorizer
	Notifier               PlatformNotifier
	Ingress                PlatformIngress
	Events                 Events
	LogEmitter             *logs.LogEmitter
	ReservedAgentIDs       []string
	// Secrets is the sealed-secret backend. It is always wired in
	// production; when nil, explicit sealed operations fail closed while
	// implicit paths (public spec updates, deployment capture, desired
	// merge) skip sealed handling.
	Secrets *secretkeys.Service
	// DeletionGracePeriod is how long tombstones stay restorable before
	// garbage collection destroys them. Zero selects DefaultDeletionGracePeriod.
	DeletionGracePeriod time.Duration
}

// DefaultDeletionGracePeriod keeps deleted resources restorable for a week.
const DefaultDeletionGracePeriod = 7 * 24 * time.Hour

type SourceStore interface {
	SourceBindingByServiceIDQuerier(context.Context, source.Querier, string) (source.SourceBindingRecord, error)
	SourceRevisionByBindingAndCommitTx(context.Context, source.Querier, string, string) (source.SourceRevisionRecord, error)
	SourceRevisionByIDTx(context.Context, source.Querier, string) (source.SourceRevisionRecord, error)
	SourceSnapshotByRevisionIDTx(context.Context, source.Querier, string) (source.SourceSnapshotRecord, error)
	LatestSourceRevisionByBindingIDTx(context.Context, source.Querier, string) (source.SourceRevisionRecord, error)
	UpsertSourceSnapshotTx(context.Context, *sql.Tx, source.SourceSnapshotRecord) (source.SourceSnapshotRecord, error)
	ServiceHasUnbuiltSourceRevisionTx(context.Context, source.Querier, string) (bool, error)
	ServicesWithUnbuiltSourceRevisionsTx(context.Context, source.Querier, string) ([]string, error)
	DeletePendingServiceWorkTx(context.Context, *sql.Tx, string) error
}

type persistence struct {
	createEnvironmentQuerier func(context.Context, ServiceQueryer, string, string, bool, string) (EnvironmentRecord, error)
	createVolumeTx           func(context.Context, *sql.Tx, authz.Project, string, string, int64) (VolumeRecord, error)
	enqueueSourceWorkItemTx  func(context.Context, *sql.Tx, durablework.EnqueueParams) (bool, error)
	sourceStore              SourceStore
	authz                    *authz.Authorizer

	db                *sql.DB
	mesh              config.ControlPlaneMeshConfig
	live              *Live
	reservedAgentIDs  []string
	deletionGrace     time.Duration
	withProductTx     Transaction
	withObservationTx ObservationTransaction
	readState         func(context.Context, func(*sql.Tx, journal.DurableState) error) error
	secrets           *secretkeys.Service
}

func New(deps Dependencies) *Delivery {
	live := deps.Live
	if live == nil {
		live = NewLive()
	}
	grace := deps.DeletionGracePeriod
	if grace <= 0 {
		grace = DefaultDeletionGracePeriod
	}
	return &Delivery{
		store: &persistence{
			db:                       deps.DB,
			mesh:                     deps.Mesh,
			live:                     live,
			reservedAgentIDs:         append([]string(nil), deps.ReservedAgentIDs...),
			deletionGrace:            grace,
			withProductTx:            deps.ProductTransaction,
			withObservationTx:        deps.ObservationTransaction,
			readState:                deps.ReadState,
			createEnvironmentQuerier: deps.CreateEnvironment,
			createVolumeTx:           deps.CreateVolume,
			enqueueSourceWorkItemTx:  deps.EnqueueSourceWork,
			sourceStore:              deps.SourceStore,
			authz:                    deps.Authorizer,
			secrets:                  deps.Secrets,
		},
		live:       live,
		notifier:   deps.Notifier,
		ingress:    deps.Ingress,
		events:     deps.Events,
		logEmitter: deps.LogEmitter,
	}
}

func (d *Delivery) LivePosition() LivePosition {
	if d == nil {
		return LivePosition{}
	}
	return d.live.Position()
}

func (d *Delivery) LiveAllocationsByEnvironment(environmentID string) (map[string][]AllocationRecord, error) {
	if d == nil || d.live == nil {
		return nil, ErrNotLiveOwner
	}
	return d.live.AllocationsByEnvironmentIfServing(environmentID)
}

type ReadModel interface {
	// ListAgents requires platform-operator membership.
	ListAgents(context.Context, authz.User) ([]AgentRecord, error)
	ListServiceDeployments(context.Context, authz.User, string, int32) ([]DeploymentRecord, error)
	ListDomainBindings(context.Context, authz.User, string, bool) ([]DomainBindingRecord, error)
	ServiceStatus(context.Context, authz.User, string) (ServiceRecord, []AllocationRecord, error)
	EnvironmentByID(context.Context, authz.User, string) (EnvironmentRecord, error)
	ListServices(context.Context, authz.User, string, bool) ([]ServiceRecord, error)
	ServiceByID(context.Context, authz.User, string) (ServiceRecord, error)
	// AgentByID, AgentIDs, BuildByID, ServiceSnapshot, and
	// ListAllocationsByServiceID are system reads without user
	// authorization. They serve internal reconciliation, notification
	// fan-out, and builder paths; user requests must go through the
	// authorized methods above.
	AgentByID(context.Context, string) (AgentRecord, error)
	AgentIDs(context.Context) ([]string, error)
	ListAllocationsByServiceID(context.Context, string) ([]AllocationRecord, error)
	BuildByID(context.Context, string) (BuildRunRecord, error)
	ServiceSnapshot(context.Context, string) (ServiceRecord, error)
}

type readModel struct{ store *persistence }

func (d *Delivery) ReadModel() ReadModel { return &readModel{store: d.store} }

func (d *Delivery) SetClocks(rollout, failover func() time.Time) {
	d.rolloutNow = rollout
	d.failoverNow = failover
}
