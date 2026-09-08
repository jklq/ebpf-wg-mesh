package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

func newDelivery(store *persistence, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, events *PlatformEvents) *deliverycore.Delivery {
	return deliverycore.New(deliveryDependencies(store, notifier, ingress, events))
}

func deliveryDependencies(store *persistence, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, events *PlatformEvents) deliverycore.Dependencies {
	return deliverycore.Dependencies{
		CreateEnvironment: store.catalog.createEnvironmentQuerier, CreateVolume: store.catalog.createVolumeTx, EnqueueSourceWork: store.source.enqueueSourceWorkItemTx,
		DB: store.db, Mesh: store.mesh, Transaction: store.withTx, UnfencedTransaction: store.withTxUnfenced,
		ReservedAgentIDs: store.reservedAgentIDs, UseReportedAllocationIP: store.useReportedAllocationIP,
		Notifier: notifier, Ingress: ingress, Events: events,
		UserFromContext: func(ctx context.Context) (deliverycore.UserIdentity, error) {
			user, err := DelegatedUserFromContext(ctx)
			return deliverycore.UserIdentity{UserID: user.UserID}, err
		},
	}
}

func (s *readsPersistence) deliveryQueries() *deliverycore.ReadModel { return s.model }
