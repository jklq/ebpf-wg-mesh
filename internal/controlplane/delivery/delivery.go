// Package delivery owns service drafts, deployments, builds, allocation
// assignments, and agent observations. Delivery serializes scheduling decisions
// and commits desired state through the product journal; Live overlays agent
// presence and observations without changing durable intent.
package delivery

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/durablework"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/registry"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/source"
)

// Delivery is the entry point for delivery commands and authorized reads.
// Its persistence and scheduling policies stay private to this package.
type Delivery struct {
	schedulerMu    sync.Mutex
	store          *persistence
	live           *Live
	notifier       PlatformNotifier
	ingress        PlatformIngress
	events         Events
	logEmitter     *logs.LogEmitter
	rolloutNow     func() time.Time
	failoverNow    func() time.Time
	buildScheduler BuildSchedulerConfig
	allocSync      *allocSync
	imageResolver  registry.ImageResolver
}

// SetImageResolver installs the direct-image tag resolver. Nil resolves digest-pinned
// references only.
func (d *Delivery) SetImageResolver(resolver registry.ImageResolver) {
	if d == nil {
		return
	}
	d.imageResolver = resolver
}

type Transaction func(context.Context, func(context.Context, *sql.Tx) error) error

// ObservationTransaction runs fn in a transaction that bumps the global status revision on change.
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
	// SourceStore is delivery's only path to source tables, scoped to its transactions.
	SourceStore SourceStore

	DB                 *sql.DB
	Mesh               config.ControlPlaneMeshConfig
	Live               *Live
	ProductTransaction Transaction
	// Publishes status invalidations for workless observation changes. Nil in tests.
	ObservationTransaction ObservationTransaction
	ReadState              func(context.Context, func(*sql.Tx, journal.DurableState) error) error
	Authorizer             *authz.Authorizer
	Notifier               PlatformNotifier
	Ingress                PlatformIngress
	Events                 Events
	LogEmitter             *logs.LogEmitter
	ReservedAgentIDs       []string
	// Secrets is the sealed-secret backend, always wired in production. When nil, explicit
	// sealed operations fail closed while implicit paths skip sealed handling.
	Secrets *secretkeys.Service
	// DeletionGracePeriod is how long tombstones stay restorable. Zero selects the default.
	DeletionGracePeriod time.Duration
	// BuildScheduler tunes the lease-based build queue. Zero selects the default config.
	BuildScheduler BuildSchedulerConfig
	// ImageResolver pins direct-image tags. Nil resolves pinned refs only; tags fail closed.
	ImageResolver registry.ImageResolver
}

// DefaultDeletionGracePeriod keeps deleted resources restorable for a week.
const DefaultDeletionGracePeriod = 7 * 24 * time.Hour

type SourceStore interface {
	SourceBindingByServiceIDQuerier(context.Context, source.Querier, string) (source.SourceBindingRecord, error)
	SourceRevisionByBindingAndCommitTx(context.Context, source.Querier, string, string) (source.SourceRevisionRecord, error)
	SourceRevisionByIDTx(context.Context, source.Querier, string) (source.SourceRevisionRecord, error)
	SourceSnapshotByRevisionIDTx(context.Context, source.Querier, string) (source.SourceSnapshotRecord, error)
	LatestSourceRevisionByBindingIDTx(context.Context, source.Querier, string) (source.SourceRevisionRecord, error)
	SourceBindingHeadCommitTx(context.Context, source.Querier, string) (string, error)
	SetSourceBindingHeadCommitTx(context.Context, source.Querier, string, string) error
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
	scheduler := deps.BuildScheduler.WithDefaults()
	return &Delivery{
		buildScheduler: scheduler,
		allocSync:      newAllocSync(),
		imageResolver:  deps.ImageResolver,
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

func (d *Delivery) SetClocks(rollout, failover func() time.Time) {
	d.rolloutNow = rollout
	d.failoverNow = failover
}

type PlatformNotifier interface {
	Notify(agentID string)
}

type PlatformIngress interface {
	Sync(ctx context.Context) error
	RequestSync()
	Converged(ctx context.Context) (bool, error)
}
