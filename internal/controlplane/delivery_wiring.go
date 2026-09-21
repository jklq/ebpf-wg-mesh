package controlplane

import (
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/logs"
)

func newDelivery(store *persistence, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, events *PlatformEvents, logEmitter *logs.LogEmitter) *deliverycore.Delivery {
	d := deliverycore.New(deliveryDependencies(store, notifier, ingress, events, logEmitter))
	store.reads.ReadModel = d.ReadModel()
	return d
}

func deliveryDependencies(store *persistence, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, events *PlatformEvents, logEmitter *logs.LogEmitter) deliverycore.Dependencies {
	return deliverycore.Dependencies{
		CreateEnvironment: store.catalog.createEnvironmentQuerier, CreateVolume: store.catalog.createVolumeTx, EnqueueSourceWork: store.source.Work().EnqueueTx, SourceStore: store.source,
		DB: store.db, Mesh: store.mesh, Live: store.liveImplementation, ProductTransaction: store.withProductTx,
		ObservationTransaction: store.withObservationTx,
		ReadState:              store.readLiveState,
		ReservedAgentIDs:       store.reservedAgentIDs,
		Notifier:               notifier, Ingress: ingress, Events: events, LogEmitter: logEmitter,
		Authorizer: store.authorizer(), Secrets: store.secrets, DeletionGracePeriod: store.deletionGracePeriod(),
	}
}
