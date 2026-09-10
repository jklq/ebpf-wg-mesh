package controlplane

import (
	"context"

	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
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
}
type buildsPersistence struct {
	*database
}
type catalogPersistence struct {
	*database
	reads *readsPersistence
}
type eventsPersistence struct {
	*database
}
type fleetPersistence struct {
	*database
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
	live ingressLiveReader
}

func (s *routingPersistence) WithLeaseGuard(ctx context.Context, fn func() error) error {
	return s.withLeaseGuard(ctx, fn)
}

func newPersistence(db *database) *persistence {
	live := deliverycore.NewLive()
	db.initJournal()
	db.journal.SetOnApplied(live.ApplyDurable)
	p := &persistence{database: db, liveImplementation: live, notifications: live, publication: live}
	p.builds = &buildsPersistence{database: db}
	p.catalog = &catalogPersistence{database: db}
	p.events = &eventsPersistence{database: db}
	p.fleet = &fleetPersistence{database: db, sessions: live, live: live}
	p.reads = &readsPersistence{database: db}
	p.routing = &routingPersistence{database: db, live: live}
	p.source = source.NewSQLStore(db.db, db.withTx, func(ctx context.Context, serviceID string) (source.Service, error) {
		rec, err := p.reads.ServiceSnapshot(ctx, serviceID)
		if err != nil {
			return source.Service{}, err
		}
		return source.Service{ID: rec.ID, ProjectID: rec.ProjectID, Spec: rec.Spec, SpecRevision: rec.SpecRevision}, nil
	})
	p.catalog.reads = p.reads
	p.fleet.reads = p.reads
	return p
}

type platformPersistence struct {
	*catalogPersistence
	*routingPersistence
	*readsPersistence
}

func (p *persistence) platform() platformPersistence {
	return platformPersistence{p.catalog, p.routing, p.reads}
}
