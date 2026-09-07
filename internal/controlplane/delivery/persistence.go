package delivery

import (
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/config"
	"time"
)

// Transaction runs a retryable, lease-fenced transaction and advances the
// durable event index in the same commit.
type Transaction func(context.Context, func(*sql.Tx) error) error

type UserIdentity struct{ UserID string }
type Events interface {
	Publish(context.Context, string) (int64, error)
}

// Dependencies supplies connection infrastructure and post-commit effects.
// Delivery never lends its persistence or transaction helpers to callers.
type Dependencies struct {
	CreateEnvironment func(context.Context, ServiceQueryer, string, string, bool, string) (EnvironmentRecord, error)
	CreateVolume      func(context.Context, *sql.Tx, string, string, string, int64) (VolumeRecord, error)
	EnqueueSourceWork func(context.Context, *sql.Tx, SourceWorkItemRecord) (bool, error)

	DB                      *sql.DB
	Mesh                    config.ControlPlaneMeshConfig
	Transaction             Transaction
	UnfencedTransaction     Transaction
	UserFromContext         func(context.Context) (UserIdentity, error)
	Notifier                PlatformNotifier
	Ingress                 PlatformIngress
	Events                  Events
	ReservedAgentIDs        []string
	UseReportedAllocationIP bool
}

type persistence struct {
	createEnvironmentQuerier func(context.Context, ServiceQueryer, string, string, bool, string) (EnvironmentRecord, error)
	createVolumeTx           func(context.Context, *sql.Tx, string, string, string, int64) (VolumeRecord, error)
	enqueueSourceWorkItemTx  func(context.Context, *sql.Tx, SourceWorkItemRecord) (bool, error)

	db                      *sql.DB
	mesh                    config.ControlPlaneMeshConfig
	reservedAgentIDs        []string
	useReportedAllocationIP bool
	withTx                  Transaction
	withTxUnfenced          Transaction
}

func New(deps Dependencies) *Delivery {
	return &Delivery{
		store:    &persistence{db: deps.DB, mesh: deps.Mesh, reservedAgentIDs: append([]string(nil), deps.ReservedAgentIDs...), useReportedAllocationIP: deps.UseReportedAllocationIP, withTx: deps.Transaction, withTxUnfenced: deps.UnfencedTransaction},
		notifier: deps.Notifier, ingress: deps.Ingress, events: deps.Events, userFromContext: deps.UserFromContext,
	}
}

// ReadModel exposes the snapshots used by transports and other modules.
// Its persistence remains private to delivery.
type ReadModel struct{ store *persistence }

func (d *Delivery) ReadModel() *ReadModel { return &ReadModel{store: d.store} }

func (s *persistence) bumpAllDesiredRevisionsTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE agents SET desired_revision = desired_revision + 1`)
	return err
}
func (s *persistence) bumpDesiredRevisionsTx(ctx context.Context, tx *sql.Tx, agentIDs []string) error {
	seen := map[string]bool{}
	for _, id := range agentIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET desired_revision = desired_revision + 1 WHERE id = $1`, id); err != nil {
			return err
		}
	}
	return nil
}

// Clock overrides are useful for deterministic reconciliation tests.
func (d *Delivery) SetClocks(rollout, failover func() time.Time) {
	d.rolloutNow = rollout
	d.failoverNow = failover
}
