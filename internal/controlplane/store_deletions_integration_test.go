//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

// deletionFixture bootstraps one owner with one project and returns the
// project plus its production environment.
func deletionFixture(t *testing.T, store *persistence, owner, projectName string) (deliverycore.ProjectRecord, deliverycore.EnvironmentRecord) {
	t.Helper()
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: owner, Email: owner + "@example.com", Projects: []string{projectName}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser(owner), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %#v: %v", projects, err)
	}
	environment, err := store.catalog.productionEnvironmentByProjectInternal(ctx, projects[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return projects[0], environment
}

func expireTombstone(t *testing.T, store *persistence, table, idColumn, id string) {
	t.Helper()
	past := time.Now().UTC().Add(-time.Hour)
	var query string
	switch table {
	case "domain_bindings":
		query = `UPDATE domain_bindings SET delete_expires_at = $1 WHERE hostname = $2`
	default:
		query = `UPDATE ` + table + ` SET delete_expires_at = $1 WHERE ` + idColumn + ` = $2`
	}
	if _, err := store.db.ExecContext(context.Background(), query, past, id); err != nil {
		t.Fatalf("expire %s %s: %v", table, id, err)
	}
}

func TestDeletionProjectTombstoneRestoreLifecycle(t *testing.T) {
	t.Parallel()
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
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, testUser("owner"), "web.example.test", service.ID, 8080); err != nil {
		t.Fatal(err)
	}

	preview, err := store.catalog.previewProjectDeletion(ctx, testUser("owner"), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Environments) != 1 || len(preview.Services) != 1 || len(preview.Domains) != 1 || len(preview.Volumes) != 1 {
		t.Fatalf("unexpected project deletion preview: %#v", preview)
	}

	if _, err := store.catalog.deleteProject(ctx, testUser("owner"), project.ID, ""); !errors.Is(err, deliverycore.ErrConfirmationMismatch) {
		t.Fatalf("delete project without confirmation: %v", err)
	}
	if _, err := store.catalog.deleteProject(ctx, testUser("owner"), project.ID, "demo"); err != nil {
		t.Fatal(err)
	}

	// Repeats are idempotent and do not need the confirmation again.
	if _, err := store.catalog.deleteProject(ctx, testUser("owner"), project.ID, ""); err != nil {
		t.Fatalf("repeat delete project: %v", err)
	}

	hidden, err := store.catalog.listProjects(ctx, testUser("owner"), false)
	if err != nil || len(hidden) != 0 {
		t.Fatalf("default project listing shows tombstones: %#v: %v", hidden, err)
	}
	shown, err := store.catalog.listProjects(ctx, testUser("owner"), true)
	if err != nil || len(shown) != 1 {
		t.Fatalf("recovery listing hides tombstones: %#v: %v", shown, err)
	}
	if shown[0].Deletion == nil || shown[0].Deletion.DeletedByUserID != "owner" || shown[0].Deletion.Inherited {
		t.Fatalf("tombstone does not record the requesting user: %#v", shown[0].Deletion)
	}
	if shown[0].Deletion.ExpiresAt.Before(time.Now().UTC()) {
		t.Fatalf("tombstone already expired: %#v", shown[0].Deletion)
	}

	// Children read as deleted through the tombstoned parent.
	services, err := store.reads.ListServices(ctx, testUser("owner"), environment.ID, false)
	if err != nil || len(services) != 0 {
		t.Fatalf("services under a deleted project stay listed: %#v: %v", services, err)
	}
	byID, err := store.reads.ServiceByID(ctx, testUser("owner"), service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if byID.Deletion == nil || !byID.Deletion.Inherited {
		t.Fatalf("service under a deleted project reads live: %#v", byID.Deletion)
	}

	restored, err := store.catalog.restoreProject(ctx, testUser("owner"), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Deletion != nil {
		t.Fatalf("restored project still deleted: %#v", restored.Deletion)
	}
	services, err = store.reads.ListServices(ctx, testUser("owner"), environment.ID, false)
	if err != nil || len(services) != 1 {
		t.Fatalf("services after project restore: %#v: %v", services, err)
	}

	// Deleting again after a restore starts a fresh grace period.
	if _, err := store.catalog.deleteProject(ctx, testUser("owner"), project.ID, "demo"); err != nil {
		t.Fatalf("delete after restore: %v", err)
	}
}

func TestDeletionEnvironmentConfirmationRestoreAndAncestry(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	project, production := deletionFixture(t, store, "owner", "demo")

	staging, err := store.catalog.createEnvironment(ctx, testUser("owner"), project.ID, "Staging")
	if err != nil {
		t.Fatal(err)
	}

	// Non-production environments delete without a typed confirmation.
	if _, err := store.catalog.deleteEnvironment(ctx, testUser("owner"), staging.ID, ""); err != nil {
		t.Fatalf("delete staging: %v", err)
	}
	if _, err := store.catalog.deleteEnvironment(ctx, testUser("owner"), staging.ID, ""); err != nil {
		t.Fatalf("repeat delete staging: %v", err)
	}
	restored, err := store.catalog.restoreEnvironment(ctx, testUser("owner"), staging.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Deletion != nil {
		t.Fatalf("restored staging still deleted: %#v", restored.Deletion)
	}

	// Production environments require a confirmation matching the current name.
	if _, err := store.catalog.deleteEnvironment(ctx, testUser("owner"), production.ID, "production"); !errors.Is(err, deliverycore.ErrConfirmationMismatch) {
		t.Fatalf("delete production with wrong-case confirmation: %v", err)
	}
	if _, err := store.catalog.deleteEnvironment(ctx, testUser("owner"), production.ID, production.Name); err != nil {
		t.Fatalf("delete production with confirmation: %v", err)
	}

	// Restoring a child with no tombstone of its own under a tombstoned
	// parent is refused: restore top-down.
	if _, err := store.catalog.deleteProject(ctx, testUser("owner"), project.ID, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.restoreEnvironment(ctx, testUser("owner"), staging.ID); !errors.Is(err, deliverycore.ErrAncestorDeleted) {
		t.Fatalf("restore environment under deleted project: %v", err)
	}
	// Clearing an independent child tombstone under a deleted parent is
	// allowed; the child stays effectively deleted until the parent
	// returns, and comes back live with it.
	if _, err := store.catalog.restoreEnvironment(ctx, testUser("owner"), production.ID); err != nil {
		t.Fatalf("restore production under deleted project: %v", err)
	}
	stillDeleted, err := store.reads.EnvironmentByID(ctx, testUser("owner"), production.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillDeleted.Deletion == nil || !stillDeleted.Deletion.Inherited {
		t.Fatalf("production reads live under a deleted project: %#v", stillDeleted.Deletion)
	}
	if _, err := store.catalog.restoreProject(ctx, testUser("owner"), project.ID); err != nil {
		t.Fatal(err)
	}
	after, err := store.reads.EnvironmentByID(ctx, testUser("owner"), production.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Deletion != nil {
		t.Fatalf("production still deleted after project restore: %#v", after.Deletion)
	}
}

func TestDeletionServiceWithdrawsWorkloadsAndRestores(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	_, environment := deletionFixture(t, store, "owner", "demo")
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "owner", environment.ID, "web", directImageServiceSpec("example.test/web:1", nil), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := releaseEnvironmentForTest(ctx, store, "owner", environment.ID); err != nil {
		t.Fatal(err)
	}
	before, err := desiredStateForAgent(ctx, store, "node-1")
	if err != nil || len(before.GetServices()) != 1 {
		t.Fatalf("desired state before delete: %#v: %v", before.GetServices(), err)
	}

	if err := deleteService(ctx, store, "owner", service.ID); err != nil {
		t.Fatal(err)
	}
	if err := deleteService(ctx, store, "owner", service.ID); err != nil {
		t.Fatalf("repeat delete service: %v", err)
	}
	after, err := desiredStateForAgent(ctx, store, "node-1")
	if err != nil || len(after.GetServices()) != 0 {
		t.Fatalf("deleted service still desired: %#v: %v", after.GetServices(), err)
	}
	// Placements are dropped at delete time so durable rows stay consistent
	// with the live view.
	if count := assertLiveAllocationsMatchDurable(t, store, service.ID); count != 0 {
		t.Fatalf("deleted service kept %d allocations", count)
	}
	if _, _, err := updateService(ctx, store, "owner", service.ID, "web-new", directImageServiceSpec("example.test/web:2", nil)); !errors.Is(err, deliverycore.ErrServiceDeleted) {
		t.Fatalf("update deleted service: %v", err)
	}

	restored, err := testDelivery(store).RestoreService(ctx, testUser("owner"), service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Deletion != nil {
		t.Fatalf("restored service still deleted: %#v", restored.Deletion)
	}
	// New work resumes after restore: update, then release.
	if _, _, err := updateService(ctx, store, "owner", service.ID, "web", directImageServiceSpec("example.test/web:2", nil)); err != nil {
		t.Fatalf("update after restore: %v", err)
	}
	released, err := releaseEnvironmentServiceForTest(ctx, store, "owner", environment.ID, service.ID)
	if err != nil {
		t.Fatalf("release after restore: %v", err)
	}
	if released.Deletion != nil {
		t.Fatalf("released service still deleted: %#v", released.Deletion)
	}
}

func TestDeletionEnvironmentDropsChildPlacements(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	project, _ := deletionFixture(t, store, "owner", "demo")
	staging, err := store.catalog.createEnvironment(ctx, testUser("owner"), project.ID, "Staging")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "owner", staging.ID, "web", directImageServiceSpec("example.test/web:1", nil), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := releaseEnvironmentForTest(ctx, store, "owner", staging.ID); err != nil {
		t.Fatal(err)
	}
	if count := assertLiveAllocationsMatchDurable(t, store, service.ID); count != 1 {
		t.Fatalf("allocations before delete = %d, want 1", count)
	}

	if _, err := store.catalog.deleteEnvironment(ctx, testUser("owner"), staging.ID, ""); err != nil {
		t.Fatal(err)
	}
	if count := assertLiveAllocationsMatchDurable(t, store, service.ID); count != 0 {
		t.Fatalf("environment delete kept %d child allocations", count)
	}
	after, err := desiredStateForAgent(ctx, store, "node-1")
	if err != nil || len(after.GetServices()) != 0 {
		t.Fatalf("deleted environment still desired: %#v: %v", after.GetServices(), err)
	}

	if _, err := store.catalog.restoreEnvironment(ctx, testUser("owner"), staging.ID); err != nil {
		t.Fatal(err)
	}
	// New work resumes after restore: update, then release.
	if _, _, err := updateService(ctx, store, "owner", service.ID, "web", directImageServiceSpec("example.test/web:2", nil)); err != nil {
		t.Fatalf("update after restore: %v", err)
	}
	if _, err := releaseEnvironmentServiceForTest(ctx, store, "owner", staging.ID, service.ID); err != nil {
		t.Fatalf("release after restore: %v", err)
	}
	if count := assertLiveAllocationsMatchDurable(t, store, service.ID); count != 1 {
		t.Fatalf("allocations after restore+release = %d, want 1", count)
	}
}

func TestDeletionDomainBindingRestoreLifecycle(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	_, environment := deletionFixture(t, store, "owner", "demo")
	service, err := createScheduledService(ctx, store, "owner", environment.ID, "web", directImageServiceSpec("example.test/web:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, testUser("owner"), "web.example.test", service.ID, 8080); err != nil {
		t.Fatal(err)
	}

	changed, err := store.routing.DeleteDomainBindingRecord(ctx, testUser("owner"), "web.example.test")
	if err != nil || !changed {
		t.Fatalf("delete domain binding: changed=%v err=%v", changed, err)
	}
	changed, err = store.routing.DeleteDomainBindingRecord(ctx, testUser("owner"), "web.example.test")
	if err != nil || changed {
		t.Fatalf("repeat delete domain binding: changed=%v err=%v", changed, err)
	}
	live, err := store.reads.ListDomainBindings(ctx, testUser("owner"), service.ID, false)
	if err != nil || len(live) != 0 {
		t.Fatalf("default binding listing shows tombstones: %#v: %v", live, err)
	}
	withDeleted, err := store.reads.ListDomainBindings(ctx, testUser("owner"), service.ID, true)
	if err != nil || len(withDeleted) != 1 || withDeleted[0].Deletion == nil {
		t.Fatalf("recovery binding listing: %#v: %v", withDeleted, err)
	}
	if withDeleted[0].Deletion.DeletedByUserID != "owner" {
		t.Fatalf("binding tombstone does not record the user: %#v", withDeleted[0].Deletion)
	}

	restored, err := store.routing.RestoreDomainBindingRecord(ctx, testUser("owner"), "web.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Deletion != nil {
		t.Fatalf("restored binding still deleted: %#v", restored.Deletion)
	}
	live, err = store.reads.ListDomainBindings(ctx, testUser("owner"), service.ID, false)
	if err != nil || len(live) != 1 {
		t.Fatalf("bindings after restore: %#v: %v", live, err)
	}
}

func TestDeletionVolumeFailsClosed(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	project, production := deletionFixture(t, store, "owner", "demo")
	staging, err := store.catalog.createEnvironment(ctx, testUser("owner"), project.ID, "Staging")
	if err != nil {
		t.Fatal(err)
	}

	stagingVolume, err := store.catalog.createScheduledVolume(ctx, testUser("owner"), staging.ID, "data", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	stagingService, err := createScheduledService(ctx, store, "owner", staging.ID, "web", directImageServiceSpec("example.test/web:1", &platformv1.ServiceRuntime{VolumeName: "data"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, testUser("owner"), "web.example.test", stagingService.ID, 8080); err != nil {
		t.Fatal(err)
	}

	if err := store.catalog.deleteVolume(ctx, testUser("owner"), stagingVolume.ID, "wrong"); !errors.Is(err, deliverycore.ErrConfirmationMismatch) {
		t.Fatalf("delete volume with wrong confirmation: %v", err)
	}
	preview, err := store.catalog.previewVolumeDeletion(ctx, testUser("owner"), stagingVolume.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Services) != 1 || preview.Services[0].ID != stagingService.ID {
		t.Fatalf("volume preview misses the attached service: %#v", preview.Services)
	}
	if len(preview.Domains) != 1 || preview.Domains[0].Hostname != "web.example.test" {
		t.Fatalf("volume preview misses the service domains: %#v", preview.Domains)
	}
	if err := store.catalog.deleteVolume(ctx, testUser("owner"), stagingVolume.ID, "data"); !errors.Is(err, deliverycore.ErrVolumeInUse) {
		t.Fatalf("delete attached volume: %v", err)
	}

	// Once no live service references it, a staging volume tombstones.
	if err := deleteService(ctx, store, "owner", stagingService.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.catalog.deleteVolume(ctx, testUser("owner"), stagingVolume.ID, "data"); err != nil {
		t.Fatalf("delete detached staging volume: %v", err)
	}
	if err := store.catalog.deleteVolume(ctx, testUser("owner"), stagingVolume.ID, ""); err != nil {
		t.Fatalf("repeat delete volume: %v", err)
	}

	// A production volume that was ever attached is refused as possibly
	// non-empty even after the referencing service is gone.
	prodVolume, err := store.catalog.createScheduledVolume(ctx, testUser("owner"), production.ID, "pdata", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	prodService, err := createScheduledService(ctx, store, "owner", production.ID, "db", directImageServiceSpec("example.test/db:1", &platformv1.ServiceRuntime{VolumeName: "pdata"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteService(ctx, store, "owner", prodService.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.catalog.deleteVolume(ctx, testUser("owner"), prodVolume.ID, "pdata"); !errors.Is(err, deliverycore.ErrVolumeNotEmpty) {
		t.Fatalf("delete ever-attached production volume: %v", err)
	}

	// A production volume that was never attached deletes normally.
	freshVolume, err := store.catalog.createScheduledVolume(ctx, testUser("owner"), production.ID, "fresh", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.catalog.deleteVolume(ctx, testUser("owner"), freshVolume.ID, "fresh"); err != nil {
		t.Fatalf("delete never-attached production volume: %v", err)
	}
}

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

func TestDeletionConcurrentDeleteAndDeployStayConsistent(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	_, environment := deletionFixture(t, store, "owner", "demo")
	service, err := createScheduledService(ctx, store, "owner", environment.ID, "web", directImageServiceSpec("example.test/web:1", nil))
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	deleteErrCh := make(chan error, 1)
	updateErrCh := make(chan error, 1)
	go func() {
		<-start
		deleteErrCh <- deleteService(ctx, store, "owner", service.ID)
	}()
	go func() {
		<-start
		_, _, err := updateService(ctx, store, "owner", service.ID, "web", directImageServiceSpec("example.test/web:2", nil))
		updateErrCh <- err
	}()
	close(start)
	if err := <-deleteErrCh; err != nil {
		t.Fatalf("concurrent delete: %v", err)
	}
	if err := <-updateErrCh; err != nil && !errors.Is(err, deliverycore.ErrServiceDeleted) {
		t.Fatalf("concurrent update: %v", err)
	}

	// Whatever won the race, the terminal state is consistent: the service
	// is tombstoned and its deployment is Removed.
	current, err := store.reads.ServiceByID(ctx, testUser("owner"), service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Deletion == nil {
		t.Fatal("service survived the concurrent delete")
	}
	var state string
	if err := store.db.QueryRowContext(ctx, `SELECT state FROM deployments WHERE service_id = $1 ORDER BY created_at DESC LIMIT 1`, service.ID).Scan(&state); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if state != "" && state != deliverycore.DeploymentStateRemoved {
		t.Fatalf("deleted service deployment state = %q, want %q", state, deliverycore.DeploymentStateRemoved)
	}
}

func TestDeletionManagedProjectCannotBeDeleted(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	deletionFixture(t, store, "owner", "demo")
	managed, err := store.catalog.ensureManagedProject(ctx, "Platform Dashboard", "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.deleteProject(ctx, testUser("owner"), managed.ID, managed.Name); err == nil {
		t.Fatal("deleted a managed project")
	}
}
