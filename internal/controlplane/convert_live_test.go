package controlplane

import (
	"testing"
	"time"

	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

type positionOnly struct{ position deliverycore.LivePosition }

func (r positionOnly) LivePosition() deliverycore.LivePosition { return r.position }

func TestLiveReadMetadata(t *testing.T) {
	freshness := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	meta := liveReadMeta(positionOnly{deliverycore.LivePosition{
		AcceptedDurable: 17, AppliedLive: 23, Ready: true, ObservationFreshness: freshness,
	}})
	if meta.AcceptedDurablePosition != 17 || meta.AppliedLivePosition != 23 ||
		!meta.LiveOwnerReady || !meta.ObservationFreshness.AsTime().Equal(freshness) {
		t.Fatalf("live metadata lost read position: %v", meta)
	}
	if liveReadMeta(nil) != nil {
		t.Fatal("absent reader should omit metadata")
	}
	meta = liveReadMeta(positionOnly{})
	if meta.LiveOwnerReady || meta.ObservationFreshness != nil {
		t.Fatalf("unready reader should not imply fresh observations: %v", meta)
	}
}
