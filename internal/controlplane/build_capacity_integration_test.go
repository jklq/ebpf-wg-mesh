//go:build integration

package controlplane

import (
	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"testing"
	"time"
)

func TestBuildCapacityHomePreferenceAndOfflineTakeover(t *testing.T) {
	store, ctx, _, _, service := setupSourceServiceForDeployment(t)
	if err := seedReadySourceState(t, store, service, "capacity"); err != nil {
		t.Fatal(err)
	}
	d := testDelivery(store)
	home := testBuilderOffer("home", "Home PC")
	home.HostType = config.HostIntermittent
	// Advertise an idle home builder before there is queued work.
	if job, err := d.ClaimNextBuild(ctx, home); err != nil || job.ID != "" {
		t.Fatalf("idle offer: %+v %v", job, err)
	}
	build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "capacity")
	if err != nil {
		t.Fatal(err)
	}
	vm := testBuilderOffer("vm", "Compact VM")
	vm.AvailableMemoryBytes = 1 << 30
	if job, err := d.ClaimNextBuild(ctx, vm); err != nil || job.ID != "" {
		t.Fatalf("capacity gate: %+v %v", job, err)
	}
	// Capacity waiting must neither claim nor expire a day-old queued build.
	if _, err := store.db.ExecContext(ctx, `UPDATE build_runs SET queued_at=statement_timestamp()-INTERVAL '1 day' WHERE id=$1`, build.ID); err != nil {
		t.Fatal(err)
	}
	vm.AvailableMemoryBytes = 16 << 30
	if job, err := d.ClaimNextBuild(ctx, vm); err != nil || job.ID != "" {
		t.Fatalf("home preference: %+v %v", job, err)
	}
	queued, err := d.BuildByID(ctx, build.ID)
	if err != nil || queued.State != deliverycore.BuildStateQueued || queued.AttemptCount != 0 {
		t.Fatalf("wait mutated build: %+v %v", queued, err)
	}
	if job, err := d.ClaimNextBuild(ctx, home); err != nil || job.ID != build.ID {
		t.Fatalf("home claim: %+v %v", job, err)
	}
	// A powered-off builder stops renewing; an online capable builder takes over.
	expireBuildLease(t, store, ctx, build.ID)
	if _, err := store.db.ExecContext(ctx, `UPDATE builder_workers SET last_heartbeat_at=$2 WHERE id=$1`, home.ID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	job, err := d.ClaimNextBuild(ctx, vm)
	if err != nil || job.ID != build.ID || job.AttemptCount != 2 || job.OwnerEpoch != 2 {
		t.Fatalf("offline takeover: %+v %v", job, err)
	}
}
