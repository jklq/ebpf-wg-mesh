package controlplane

import deliverycore "ebof-wg-mesh/internal/controlplane/delivery"

// persistence wires module-owned stores. It deliberately has no domain methods.
type persistence struct {
	*database
	builds  *buildsPersistence
	catalog *catalogPersistence
	events  *eventsPersistence
	fleet   *fleetPersistence
	reads   *readsPersistence
	routing *routingPersistence
	source  *sourcePersistence
}
type buildsPersistence struct {
	source *sourcePersistence
	reads  *readsPersistence
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
	model    *deliverycore.ReadModel
	delivery *deliverycore.Delivery
}
type routingPersistence struct {
	*database
	reads *readsPersistence
}
type sourcePersistence struct {
	sourceArchives SourceArchiveStore
	*database
	reads *readsPersistence
}

func newPersistence(db *database) *persistence {
	p := &persistence{database: db}
	p.builds = &buildsPersistence{database: db}
	p.catalog = &catalogPersistence{database: db}
	p.events = &eventsPersistence{database: db}
	p.fleet = &fleetPersistence{database: db}
	p.reads = &readsPersistence{database: db}
	p.routing = &routingPersistence{database: db}
	p.source = &sourcePersistence{database: db}
	p.builds.source = p.source
	p.builds.reads = p.reads
	p.catalog.reads = p.reads
	p.fleet.reads = p.reads
	p.routing.reads = p.reads
	p.source.reads = p.reads
	p.reads.delivery = newDelivery(p, nil, nil, nil)
	p.reads.model = p.reads.delivery.ReadModel()
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
