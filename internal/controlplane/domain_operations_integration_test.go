//go:build integration

package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func domainOperationFixture(t *testing.T, store *persistence, owner string) deliverycore.ServiceRecord {
	t.Helper()
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, owner, owner+"-project")
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
	ingress := &countingIngress{}
	operations := NewDomains(store.platform(), noopNotifier{}, ingress, "platform.example", staticCNAMEResolver{})
	generated, err := operations.GenerateDomainBinding(ctx, &platformv1.GenerateDomainBindingRequest{ServiceId: service.ID, TargetPort: 8080})
	if err != nil {
		t.Fatal(err)
	}
	for _, hostname := range []string{"one.example.com", "two.example.com"} {
		if _, err := operations.CreateDomainBinding(ctx, &platformv1.CreateDomainBindingRequest{Binding: &platformv1.DomainBindingInput{Hostname: hostname, ServiceId: service.ID, TargetPort: 8080}}); err != nil {
			t.Fatal(err)
		}
	}
	before := ingress.requests.Load()
	if _, err := operations.DeleteDomainBinding(ctx, &platformv1.DeleteDomainBindingRequest{Hostname: generated.Hostname}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete generated domain with custom bindings: %v", err)
	}
	if ingress.requests.Load() != before {
		t.Fatal("rejected deletion woke ingress")
	}
	if _, err := operations.DeleteDomainBinding(ctx, &platformv1.DeleteDomainBindingRequest{Hostname: "one.example.com"}); err != nil {
		t.Fatal(err)
	}
	if bindings, err := store.reads.ListDomainBindings(ctx, "owner", service.ID); err != nil || len(bindings) != 2 {
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
	if _, err := operations.DeleteDomainBinding(ctx, &platformv1.DeleteDomainBindingRequest{Hostname: "two.example.com"}); err == nil {
		t.Fatal("expected cleanup constraint failure")
	}
	if _, err := store.routing.DomainBindingByHostname(ctx, "owner", "two.example.com"); err != nil {
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
	if _, err := operations.DeleteDomainBinding(ctx, &platformv1.DeleteDomainBindingRequest{Hostname: "two.example.com"}); err != nil {
		t.Fatal(err)
	}
	if bindings, err := store.reads.ListDomainBindings(ctx, "owner", service.ID); err != nil || len(bindings) != 0 {
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
		service deliverycore.ServiceRecord
	}{{owner, source}, {attacker, target}} {
		if _, err := operations.GenerateDomainBinding(entry.ctx, &platformv1.GenerateDomainBindingRequest{ServiceId: entry.service.ID, TargetPort: 8080}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := operations.CreateDomainBinding(owner, &platformv1.CreateDomainBindingRequest{Binding: &platformv1.DomainBindingInput{Hostname: "owned.example.com", ServiceId: source.ID, TargetPort: 8080}}); err != nil {
		t.Fatal(err)
	}
	// Read access to the source must not authorize moving its hostname away.
	if _, err := store.db.ExecContext(owner, `INSERT INTO project_memberships(user_id, project_id, role) VALUES ('attacker', $1, 'viewer')`, source.ProjectID); err != nil {
		t.Fatal(err)
	}
	before := ingress.requests.Load()
	if _, err := operations.UpdateDomainBinding(attacker, &platformv1.UpdateDomainBindingRequest{Hostname: "owned.example.com", Binding: &platformv1.DomainBindingTarget{ServiceId: target.ID, TargetPort: 8080}}); err == nil {
		t.Fatal("viewer stole domain into writable target")
	}
	binding, err := store.routing.DomainBindingByHostname(owner, "owner", "owned.example.com")
	if err != nil || binding.ServiceID != source.ID {
		t.Fatalf("failed update changed binding: %#v: %v", binding, err)
	}
	if ingress.requests.Load() != before {
		t.Fatal("unauthorized update woke ingress")
	}
	if _, err := operations.UpdateDomainBinding(attacker, &platformv1.UpdateDomainBindingRequest{Hostname: "missing.example.com", Binding: &platformv1.DomainBindingTarget{ServiceId: target.ID, TargetPort: 8080}}); status.Code(err) != codes.NotFound {
		t.Fatalf("update created a missing domain: %v", err)
	}
	if _, err := store.db.ExecContext(owner, `UPDATE project_memberships SET role = 'editor' WHERE user_id = 'attacker' AND project_id = $1`, source.ProjectID); err != nil {
		t.Fatal(err)
	}
	generated, err := store.routing.PlatformDomainBindingForService(owner, "owner", source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operations.UpdateDomainBinding(attacker, &platformv1.UpdateDomainBindingRequest{Hostname: generated.Hostname, Binding: &platformv1.DomainBindingTarget{ServiceId: target.ID, TargetPort: 8080}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("reassigned service-specific generated hostname: %v", err)
	}
	if _, err := operations.UpdateDomainBinding(attacker, &platformv1.UpdateDomainBindingRequest{Hostname: "owned.example.com", Binding: &platformv1.DomainBindingTarget{ServiceId: target.ID, TargetPort: 8080}}); err != nil {
		t.Fatalf("authorized custom reassignment: %v", err)
	}
	binding, err = store.routing.DomainBindingByHostname(attacker, "attacker", "owned.example.com")
	if err != nil || binding.ServiceID != target.ID || ingress.requests.Load() != before+1 {
		t.Fatalf("custom reassignment did not finish: %#v: %v", binding, err)
	}

}
