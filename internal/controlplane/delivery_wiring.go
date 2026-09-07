package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

func newDelivery(store *Store, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, events *PlatformEvents) *deliverycore.Delivery {
	return deliverycore.New(deliveryDependencies(store, notifier, ingress, events))
}

func deliveryDependencies(store *Store, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, events *PlatformEvents) deliverycore.Dependencies {
	return deliverycore.Dependencies{
		CreateEnvironment: store.createEnvironmentQuerier, CreateVolume: store.createVolumeTx, EnqueueSourceWork: store.enqueueSourceWorkItemTx,
		DB: store.db, Mesh: store.mesh, Transaction: store.withTx, UnfencedTransaction: store.withTxUnfenced,
		ReservedAgentIDs: store.reservedAgentIDs, UseReportedAllocationIP: store.useReportedAllocationIP,
		Notifier: notifier, Ingress: ingress, Events: events,
		UserFromContext: func(ctx context.Context) (deliverycore.UserIdentity, error) {
			user, err := DelegatedUserFromContext(ctx)
			return deliverycore.UserIdentity{UserID: user.UserID}, err
		},
	}
}

func (s *Store) deliveryQueries() *deliverycore.ReadModel {
	return newDelivery(s, nil, nil, nil).ReadModel()
}
