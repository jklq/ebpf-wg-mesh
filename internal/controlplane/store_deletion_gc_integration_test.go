//go:build integration

package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

func TestDeletionGCCollectsExpiredAcrossKinds(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	project, environment := deletionFixture(t, store, "owner", "demo")

	// Each expired tombstone sits in its own subtree so foreign-key
	// cascades cannot overlap: one collection per tombstone.
	service, err := createScheduledService(ctx, store, "owner", environment.ID, "web", directImageServiceSpec("example.test/web:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	sited, err := createScheduledService(ctx, store, "owner", environment.ID, "sited", directImageServiceSpec("example.test/sited:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, testUser("owner"), "web.example.test", sited.ID, 8080); err != nil {
		t.Fatal(err)
	}
	volume, err := store.catalog.createScheduledVolume(ctx, testUser("owner"), environment.ID, "data", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	staging, err := store.catalog.createEnvironment(ctx, testUser("owner"), project.ID, "Staging")
	if err != nil {
		t.Fatal(err)
	}
	keeper, err := createScheduledService(ctx, store, "owner", staging.ID, "keeper", directImageServiceSpec("example.test/keeper:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	doomed, err := store.catalog.createProject(ctx, testUser("owner"), "doomed")
	if err != nil {
		t.Fatal(err)
	}

	if err := deleteService(ctx, store, "owner", service.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.routing.DeleteDomainBindingRecord(ctx, testUser("owner"), "web.example.test"); err != nil {
		t.Fatal(err)
	}
	if err := store.catalog.deleteVolume(ctx, testUser("owner"), volume.ID, "data"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.deleteEnvironment(ctx, testUser("owner"), staging.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.deleteProject(ctx, testUser("owner"), doomed.ID, "doomed"); err != nil {
		t.Fatal(err)
	}
	// The keeper service has no tombstone of its own; it goes down with the
	// expired staging environment through the foreign-key cascade.

	// A fresh tombstone must survive a pass with an older cutoff.
	fresh, err := createScheduledService(ctx, store, "owner", environment.ID, "fresh", directImageServiceSpec("example.test/fresh:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteService(ctx, store, "owner", fresh.ID); err != nil {
		t.Fatal(err)
	}

	expireTombstone(t, store, "projects", "id", doomed.ID)
	expireTombstone(t, store, "environments", "id", staging.ID)
	expireTombstone(t, store, "services", "id", service.ID)
	expireTombstone(t, store, "volumes", "id", volume.ID)
	expireTombstone(t, store, "domain_bindings", "hostname", "web.example.test")

	gc := NewDeletionGC(store, nil, nil, time.Minute)
	stats, err := gc.CollectOnce(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("collect expired: %v", err)
	}
	if stats.Collected != 5 {
		t.Fatalf("collected %d, want 5 (%v)", stats.Collected, stats.ByKind)
	}
	if stats.ByKind[ExpiredDeletionProject] != 1 || stats.ByKind[ExpiredDeletionEnvironment] != 1 ||
		stats.ByKind[ExpiredDeletionService] != 1 || stats.ByKind[ExpiredDeletionVolume] != 1 ||
		stats.ByKind[ExpiredDeletionDomain] != 1 {
		t.Fatalf("collection by kind: %v", stats.ByKind)
	}

	counts := []struct {
		table, column, id string
	}{
		{"projects", "id", doomed.ID},
		{"environments", "id", staging.ID},
		{"services", "id", service.ID},
		{"services", "id", keeper.ID},
		{"volumes", "id", volume.ID},
		{"domain_bindings", "hostname", "web.example.test"},
	}
	for _, item := range counts {
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+item.table+` WHERE `+item.column+` = $1`, item.id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s %s survived collection", item.table, item.id)
		}
	}

	// The unexpired tombstone survives and stays restorable.
	if _, err := testDelivery(store).RestoreService(ctx, testUser("owner"), fresh.ID); err != nil {
		t.Fatalf("restore unexpired service after GC: %v", err)
	}
	// Collection is idempotent: a second pass finds nothing.
	again, err := gc.CollectOnce(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("second collect: %v", err)
	}
	if again.Collected != 0 {
		t.Fatalf("second pass collected %d, want 0", again.Collected)
	}
	// Restore after expiry fails: the row is gone.
	if _, err := testDelivery(store).RestoreService(ctx, testUser("owner"), service.ID); err == nil {
		t.Fatal("restored a collected service")
	}
}

func TestDeletionGCCollectsProjectWithLiveDescendants(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	project, environment := deletionFixture(t, store, "owner", "demo")
	service, err := createScheduledService(ctx, store, "owner", environment.ID, "web", directImageServiceSpec("example.test/web:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.createScheduledVolume(ctx, testUser("owner"), environment.ID, "data", 64<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.deleteProject(ctx, testUser("owner"), project.ID, "demo"); err != nil {
		t.Fatal(err)
	}
	expireTombstone(t, store, "projects", "id", project.ID)
	stats, err := NewDeletionGC(store, nil, nil, time.Minute).CollectOnce(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if stats.ByKind[ExpiredDeletionProject] != 1 {
		t.Fatalf("project collection = %v", stats.ByKind)
	}
	for _, item := range []struct{ query, id string }{
		{`SELECT count(*) FROM environments WHERE project_id=$1`, project.ID},
		{`SELECT count(*) FROM services WHERE id=$1`, service.ID},
	} {
		var remaining int
		if err := store.db.QueryRowContext(ctx, item.query, item.id).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != 0 {
			t.Fatalf("descendant remains after project collection: %s", item.query)
		}
	}
}

func TestExpiredTombstoneCannotBeRestoredBeforeCollection(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	project, environment := deletionFixture(t, store, "owner", "demo")
	service, err := createScheduledService(ctx, store, "owner", environment.ID, "web", directImageServiceSpec("example.test/web:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteService(ctx, store, "owner", service.ID); err != nil {
		t.Fatal(err)
	}
	expireTombstone(t, store, "services", "id", service.ID)
	if _, err := testDelivery(store).RestoreService(ctx, testUser("owner"), service.ID); !errors.Is(err, deliverycore.ErrDeletionExpired) {
		t.Fatalf("restore expired service: %v", err)
	}
	if _, err := store.catalog.deleteEnvironment(ctx, testUser("owner"), environment.ID, environment.Name); err != nil {
		t.Fatal(err)
	}
	expireTombstone(t, store, "environments", "id", environment.ID)
	if _, err := store.catalog.restoreEnvironment(ctx, testUser("owner"), environment.ID); !errors.Is(err, deliverycore.ErrDeletionExpired) {
		t.Fatalf("restore expired environment: %v", err)
	}
	if _, err := store.catalog.deleteProject(ctx, testUser("owner"), project.ID, "demo"); err != nil {
		t.Fatal(err)
	}
	expireTombstone(t, store, "projects", "id", project.ID)
	if _, err := store.catalog.restoreProject(ctx, testUser("owner"), project.ID); !errors.Is(err, deliverycore.ErrDeletionExpired) {
		t.Fatalf("restore expired project: %v", err)
	}
}

func TestDeletionGCPartialFailureRetriesLeftovers(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	_, environment := deletionFixture(t, store, "owner", "demo")
	first, err := createScheduledService(ctx, store, "owner", environment.ID, "first", directImageServiceSpec("example.test/first:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	second, err := createScheduledService(ctx, store, "owner", environment.ID, "second", directImageServiceSpec("example.test/second:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteService(ctx, store, "owner", first.ID); err != nil {
		t.Fatal(err)
	}
	if err := deleteService(ctx, store, "owner", second.ID); err != nil {
		t.Fatal(err)
	}
	expireTombstone(t, store, "services", "id", first.ID)
	expireTombstone(t, store, "services", "id", second.ID)

	gc := NewDeletionGC(store, nil, nil, time.Minute)
	gc.SetFailHook(func(kind, id string) error {
		if id == first.ID {
			return errors.New("injected external-cleanup failure")
		}
		return nil
	})
	stats, err := gc.CollectOnce(ctx, time.Now().UTC())
	if err == nil {
		t.Fatal("expected the injected failure to surface")
	}
	if stats.Collected != 1 {
		t.Fatalf("collected %d with one failure, want 1", stats.Collected)
	}
	var remaining int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM services WHERE id = $1`, first.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatal("failed tombstone was collected despite the failure")
	}

	gc.SetFailHook(nil)
	stats, err = gc.CollectOnce(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("retry collect: %v", err)
	}
	if stats.Collected != 1 {
		t.Fatalf("retry collected %d, want 1", stats.Collected)
	}
}
