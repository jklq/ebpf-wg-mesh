//go:build integration

package controlplane

import (
	"context"
	"errors"
	"testing"

	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func domainOperationFixture(t *testing.T, store *persistence, owner string) deliverycore.ServiceRecord {
	t.Helper()
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser(owner), owner+"-project")
	if err != nil {
		t.Fatal(err)
	}
	environment, err := store.catalog.productionEnvironmentByProjectInternal(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	service, err := store.createStagedServiceForTest(ctx, owner, environment.ID, "web", directImageServiceSpec("example.test/web:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestDomainOperationsDeleteCommitsCleanupAtomically(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	service := domainOperationFixture(t, store, "owner")
	ctx := contextWithDelegatedUser("owner", "owner@example.com")
	user := testUser("owner")
	ingress := &countingIngress{}
	operations := NewDomains(store.platform(), noopNotifier{}, ingress, "platform.example", staticCNAMEResolver{})
	generated, err := operations.GenerateDomainBinding(ctx, user, &platformv1.GenerateDomainBindingRequest{ServiceId: service.ID, TargetPort: 8080})
	if err != nil {
		t.Fatal(err)
	}
	for _, hostname := range []string{"one.example.com", "two.example.com"} {
		if _, err := operations.CreateDomainBinding(ctx, user, &platformv1.CreateDomainBindingRequest{Binding: &platformv1.DomainBindingInput{Hostname: hostname, ServiceId: service.ID, TargetPort: 8080}}); err != nil {
			t.Fatal(err)
		}
	}
	before := ingress.requests.Load()
	if _, err := operations.DeleteDomainBinding(ctx, user, &platformv1.DeleteDomainBindingRequest{Hostname: generated.Hostname}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete generated domain with custom bindings: %v", err)
	}
	if ingress.requests.Load() != before {
		t.Fatal("rejected deletion woke ingress")
	}
	if _, err := operations.DeleteDomainBinding(ctx, user, &platformv1.DeleteDomainBindingRequest{Hostname: "one.example.com"}); err != nil {
		t.Fatal(err)
	}
	if bindings, err := store.reads.ListDomainBindings(ctx, testUser("owner"), service.ID); err != nil || len(bindings) != 2 {
		t.Fatalf("generated binding removed too early: %v, %v", bindings, err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TABLE domain_delete_blocker (hostname TEXT PRIMARY KEY REFERENCES domain_bindings(hostname) ON DELETE RESTRICT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO domain_delete_blocker VALUES ($1)`, generated.Hostname); err != nil {
		t.Fatal(err)
	}
	before = ingress.requests.Load()
	revision, err := store.events.currentGlobalRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operations.DeleteDomainBinding(ctx, user, &platformv1.DeleteDomainBindingRequest{Hostname: "two.example.com"}); err == nil {
		t.Fatal("expected cleanup constraint failure")
	}
	if _, err := store.routing.DomainBindingByHostname(ctx, testUser("owner"), "two.example.com"); err != nil {
		t.Fatalf("custom deletion escaped rollback: %v", err)
	}
	if ingress.requests.Load() != before {
		t.Fatal("rolled-back deletion woke ingress")
	}
	if after, err := store.events.currentGlobalRevision(ctx); err != nil || after != revision {
		t.Fatalf("rolled-back deletion advanced event: %d -> %d: %v", revision, after, err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM domain_delete_blocker`); err != nil {
		t.Fatal(err)
	}
	if _, err := operations.DeleteDomainBinding(ctx, user, &platformv1.DeleteDomainBindingRequest{Hostname: "two.example.com"}); err != nil {
		t.Fatal(err)
	}
	if bindings, err := store.reads.ListDomainBindings(ctx, testUser("owner"), service.ID); err != nil || len(bindings) != 0 {
		t.Fatalf("cleanup left bindings: %v, %v", bindings, err)
	}
	if ingress.requests.Load() != before+1 {
		t.Fatal("committed cleanup did not wake ingress exactly once")
	}
}

func TestDomainOperationsReassignmentRequiresWriteAccessToBothServices(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	source := domainOperationFixture(t, store, "owner")
	target := domainOperationFixture(t, store, "attacker")
	ingress := &countingIngress{}
	operations := NewDomains(store.platform(), noopNotifier{}, ingress, "platform.example", staticCNAMEResolver{})
	owner := contextWithDelegatedUser("owner", "owner@example.com")
	attacker := contextWithDelegatedUser("attacker", "attacker@example.com")
	for _, entry := range []struct {
		ctx     context.Context
		user    authz.User
		service deliverycore.ServiceRecord
	}{{owner, testUser("owner"), source}, {attacker, testUser("attacker"), target}} {
		if _, err := operations.GenerateDomainBinding(entry.ctx, entry.user, &platformv1.GenerateDomainBindingRequest{ServiceId: entry.service.ID, TargetPort: 8080}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := operations.CreateDomainBinding(owner, testUser("owner"), &platformv1.CreateDomainBindingRequest{Binding: &platformv1.DomainBindingInput{Hostname: "owned.example.com", ServiceId: source.ID, TargetPort: 8080}}); err != nil {
		t.Fatal(err)
	}
	// Read access to the source must not authorize moving its hostname away.
	if _, err := store.db.ExecContext(owner, `INSERT INTO project_memberships(user_id, project_id, role) VALUES ('attacker', $1, 'viewer')`, source.ProjectID); err != nil {
		t.Fatal(err)
	}
	before := ingress.requests.Load()
	if _, err := operations.UpdateDomainBinding(attacker, testUser("attacker"), &platformv1.UpdateDomainBindingRequest{Hostname: "owned.example.com", Binding: &platformv1.DomainBindingTarget{ServiceId: target.ID, TargetPort: 8080}}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer steal = %v, want PermissionDenied", err)
	}
	binding, err := store.routing.DomainBindingByHostname(owner, testUser("owner"), "owned.example.com")
	if err != nil || binding.ServiceID != source.ID {
		t.Fatalf("failed update changed binding: %#v: %v", binding, err)
	}
	if ingress.requests.Load() != before {
		t.Fatal("unauthorized update woke ingress")
	}
	// Writes report denials as PermissionDenied even when the binding is missing;
	// the uniform deny() is deliberately indistinguishable.
	if _, err := operations.UpdateDomainBinding(attacker, testUser("attacker"), &platformv1.UpdateDomainBindingRequest{Hostname: "missing.example.com", Binding: &platformv1.DomainBindingTarget{ServiceId: target.ID, TargetPort: 8080}}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("update of missing domain = %v, want PermissionDenied", err)
	}
	if _, err := store.db.ExecContext(owner, `UPDATE project_memberships SET role = 'editor' WHERE user_id = 'attacker' AND project_id = $1`, source.ProjectID); err != nil {
		t.Fatal(err)
	}
	generated, err := store.routing.PlatformDomainBindingForService(owner, testUser("owner"), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operations.UpdateDomainBinding(attacker, testUser("attacker"), &platformv1.UpdateDomainBindingRequest{Hostname: generated.Hostname, Binding: &platformv1.DomainBindingTarget{ServiceId: target.ID, TargetPort: 8080}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("reassigned service-specific generated hostname: %v", err)
	}
	if _, err := operations.UpdateDomainBinding(attacker, testUser("attacker"), &platformv1.UpdateDomainBindingRequest{Hostname: "owned.example.com", Binding: &platformv1.DomainBindingTarget{ServiceId: target.ID, TargetPort: 8080}}); err != nil {
		t.Fatalf("authorized custom reassignment: %v", err)
	}
	binding, err = store.routing.DomainBindingByHostname(attacker, testUser("attacker"), "owned.example.com")
	if err != nil || binding.ServiceID != target.ID || ingress.requests.Load() != before+1 {
		t.Fatalf("custom reassignment did not finish: %#v: %v", binding, err)
	}

}

// TestDomainOperationsCrossProjectDuplicateHostname pins the 23505 mapping: a
// hostname taken in one project is AlreadyExists in another, even though the
// pre-insert existence check is project-scoped.
func TestDomainOperationsCrossProjectDuplicateHostname(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	first := domainOperationFixture(t, store, "owner")
	second := domainOperationFixture(t, store, "owner-two")
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, testUser("owner"), "first.mesh.test", first.ID, 8080); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, testUser("owner-two"), "second.mesh.test", second.ID, 8080); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.routing.CreateDomainBindingRecord(ctx, testUser("owner"), "shared.example.com", first.ID, 8080); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.routing.CreateDomainBindingRecord(ctx, testUser("owner-two"), "shared.example.com", second.ID, 8080); !errors.Is(err, deliverycore.ErrDomainAlreadyExists) {
		t.Fatalf("cross-project duplicate = %v, want ErrDomainAlreadyExists", err)
	}
}
