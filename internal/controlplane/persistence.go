package controlplane

import (
	"context"

	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/source"
)

// persistence wires module-owned stores. It deliberately has no domain methods.
type persistence struct {
	*database
	builds  *buildsPersistence
	catalog *catalogPersistence
	events  *eventsPersistence
	fleet   *fleetPersistence
	reads   *readsPersistence
	routing *routingPersistence
	source  *source.SQLStore
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
	reads *readsPersistence
}
type readsPersistence struct {
	*database
	deliverycore.ReadModel
}
type routingPersistence struct {
	*database
}

func (s *routingPersistence) WithLeaseGuard(ctx context.Context, fn func() error) error {
	return s.withLeaseGuard(ctx, fn)
}

func newPersistence(db *database) *persistence {
	p := &persistence{database: db}
	p.builds = &buildsPersistence{database: db}
	p.catalog = &catalogPersistence{database: db}
	p.events = &eventsPersistence{database: db}
	p.fleet = &fleetPersistence{database: db}
	p.reads = &readsPersistence{database: db}
	p.routing = &routingPersistence{database: db}
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

// platformPersistence supplies the transport's read/catalog/routing view.
// Delivery mutations are supplied separately through platformDelivery.
type platformPersistence struct {
	*catalogPersistence
	*routingPersistence
	*readsPersistence
}

func (p *persistence) platform() platformPersistence {
	return platformPersistence{p.catalog, p.routing, p.reads}
}
