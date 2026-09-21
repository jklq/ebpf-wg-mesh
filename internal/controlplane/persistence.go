package controlplane

import (
	"context"

	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/source"
)

type persistence struct {
	*database
	liveImplementation *deliverycore.Live
	notifications      liveNotifications
	publication        publicationFence
	builds             *buildsPersistence
	catalog            *catalogPersistence
	events             *eventsPersistence
	fleet              *fleetPersistence
	reads              *readsPersistence
	routing            *routingPersistence
	source             *source.SQLStore
	secrets            *secretkeys.Service
}

// attachSecrets wires the sealed-secret backend. Production always attaches
// before serving; delivery skips sealed handling while it is nil.
func (p *persistence) attachSecrets(svc *secretkeys.Service) {
	p.secrets = svc
}

type buildsPersistence struct {
	*database
}
type catalogPersistence struct {
	*database
	authz  *authz.Authorizer
	reads  *readsPersistence
	source deliverycore.SourceStore
}
type eventsPersistence struct {
	*database
}
type fleetPersistence struct {
	*database
	authz    *authz.Authorizer
	sessions agentSessions
	live     fleetLiveReader
	reads    *readsPersistence
}
type readsPersistence struct {
	*database
	deliverycore.ReadModel
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
	db.journal.SetOnApplied(live.ApplyDurable)
	p := &persistence{database: db, liveImplementation: live, notifications: live, publication: live}
	authorizer := authz.NewAuthorizer(db.db)
	p.builds = &buildsPersistence{database: db}
	p.catalog = &catalogPersistence{database: db, authz: authorizer}
	p.events = &eventsPersistence{database: db}
	p.fleet = &fleetPersistence{database: db, authz: authorizer, sessions: live, live: live}
	p.reads = &readsPersistence{database: db}
	p.routing = &routingPersistence{database: db, authz: authorizer, live: live}
	p.source = source.NewSQLStore(db.db, db.withCoordinationTx, func(ctx context.Context, serviceID string) (source.Service, error) {
		rec, err := p.reads.ServiceSnapshot(ctx, serviceID)
		if err != nil {
			return source.Service{}, err
		}
		return source.Service{ID: rec.ID, ProjectID: rec.ProjectID, Spec: rec.Spec, SpecRevision: rec.SpecRevision, Deleted: rec.Deletion != nil}, nil
	})
	p.catalog.reads = p.reads
	p.catalog.source = p.source
	p.fleet.reads = p.reads
	return p
}

// authorizer returns the persistence-wide Authorizer. All scopes mint from this
// single instance; nothing constructs a second one per handle.
func (p *persistence) authorizer() *authz.Authorizer { return p.catalog.authz }

type platformPersistence struct {
	*catalogPersistence
	*routingPersistence
	*readsPersistence
}

func (p *persistence) platform() platformPersistence {
	return platformPersistence{p.catalog, p.routing, p.reads}
}
