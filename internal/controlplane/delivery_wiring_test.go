package controlplane

import (
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/logs"
)

func newDelivery(store *persistence, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, events *PlatformEvents, logEmitter *logs.LogEmitter) *deliverycore.Delivery {
	return newDeliveryWithScheduler(store, nil, notifier, ingress, events, logEmitter)
}

func TestBuildLeaseUsesItsOwnConfig(t *testing.T) {
	cfg := config.ControlPlaneBuilderConfig{HeartbeatTimeoutSeconds: 5, LeaseTTLSeconds: 120}
	if got := buildSchedulerConfigFromControlPlane(cfg).LeaseTTL; got != 120*time.Second {
		t.Fatalf("lease TTL = %s, want 120s", got)
	}
}
