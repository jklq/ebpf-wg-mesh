package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/logs"
)

func newDelivery(store *persistence, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, events *PlatformEvents, logEmitter *logs.LogEmitter) *deliverycore.Delivery {
	d := deliverycore.New(deliveryDependencies(store, notifier, ingress, events, logEmitter))
	store.reads.ReadModel = d.ReadModel()
	return d
}

func deliveryDependencies(store *persistence, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, events *PlatformEvents, logEmitter *logs.LogEmitter) deliverycore.Dependencies {
	return deliverycore.Dependencies{
		CreateEnvironment: store.catalog.createEnvironmentQuerier, CreateVolume: store.catalog.createVolumeTx, EnqueueSourceWork: store.source.EnqueueSourceWorkItemTx, SourceStore: store.source,
		DB: store.db, Mesh: store.mesh, Transaction: store.withTx, UnfencedTransaction: store.withTxUnfenced,
		ReservedAgentIDs: store.reservedAgentIDs,
		Notifier:         notifier, Ingress: ingress, Events: events, LogEmitter: logEmitter,
		UserFromContext: func(ctx context.Context) (deliverycore.UserIdentity, error) {
			user, err := identity.DelegatedUserFromContext(ctx)
			return deliverycore.UserIdentity{UserID: user.UserID}, err
		},
	}
}
