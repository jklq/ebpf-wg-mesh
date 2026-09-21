package controlplane

import (
	"time"

	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/logs"
)

func newDelivery(store *persistence, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, events *PlatformEvents, logEmitter *logs.LogEmitter) *deliverycore.Delivery {
	return newDeliveryWithScheduler(store, nil, notifier, ingress, events, logEmitter)
}

func newDeliveryWithScheduler(store *persistence, scheduler *deliverycore.BuildSchedulerConfig, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, events *PlatformEvents, logEmitter *logs.LogEmitter) *deliverycore.Delivery {
	deps := deliveryDependencies(store, notifier, ingress, events, logEmitter)
	if scheduler != nil {
		deps.BuildScheduler = *scheduler
	}
	d := deliverycore.New(deps)
	store.reads.ReadModel = d.ReadModel()
	return d
}

func buildSchedulerConfigFromControlPlane(cfg config.ControlPlaneBuilderConfig) deliverycore.BuildSchedulerConfig {
	return deliverycore.BuildSchedulerConfig{
		LeaseTTL:                time.Duration(cfg.HeartbeatTimeoutSeconds) * time.Second,
		AttemptLimit:            int64(cfg.MaxAttempts),
		MaxConcurrentGlobal:     cfg.MaxConcurrentGlobal,
		MaxConcurrentPerProject: cfg.MaxConcurrentPerProject,
		BuildTimeout:            time.Duration(cfg.BuildTimeoutSeconds) * time.Second,
		MaxQueueAge:             time.Duration(cfg.MaxQueueAgeSeconds) * time.Second,
	}.WithDefaults()
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
