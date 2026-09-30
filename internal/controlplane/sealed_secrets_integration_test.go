//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/secretkeys"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestSealedSecretsLifecycle(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "sealed")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "owner", environmentID, "web",
		directImageServiceSpec(pinnedImage("a"), &platformv1.ServiceRuntime{
			Env: map[string]string{"PUBLIC": "one"},
		}), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	delivery := testDelivery(store)

	version, err := delivery.SealServiceSecret(ctx, testUser("owner"), service.ID, "TOKEN", []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("first seal version = %d", version)
	}
	if version, err := delivery.SealServiceSecret(ctx, testUser("owner"), service.ID, "TOKEN", []byte("second")); err != nil || version != 2 {
		t.Fatalf("second seal = %d, %v", version, err)
	}
	metas, err := delivery.ListServiceSecrets(ctx, testUser("owner"), service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].Name != "TOKEN" || metas[0].Version != 2 {
		t.Fatalf("masked list = %+v", metas)
	}

	// Names stay disjoint from public environment keys in both directions.
	if _, err := delivery.SealServiceSecret(ctx, testUser("owner"), service.ID, "PUBLIC", []byte("x")); !errors.Is(err, deliverycore.ErrSealedNameConflict) {
		t.Fatalf("seal over public key = %v", err)
	}
	conflictSpec := directImageServiceSpec(pinnedImage("a"), &platformv1.ServiceRuntime{
		Env: map[string]string{"PUBLIC": "one", "TOKEN": "public"},
	})
	if _, _, err := updateService(ctx, store, "owner", service.ID, service.Name, conflictSpec); !errors.Is(err, deliverycore.ErrSealedNameConflict) {
		t.Fatalf("public update over sealed name = %v", err)
	}
	for _, name := range []string{"", "has space", "has-dash", "9START", "PLATFORM_RESERVED", strings.Repeat("N", 129)} {
		if _, err := delivery.SealServiceSecret(ctx, testUser("owner"), service.ID, name, []byte("x")); !errors.Is(err, deliverycore.ErrInvalidSealedName) {
			t.Fatalf("seal invalid name %q = %v", name, err)
		}
	}

	// Deletion tombstones the name while pinned reads keep resolving.
	if err := delivery.DeleteServiceSecret(ctx, testUser("owner"), service.ID, "TOKEN"); err != nil {
		t.Fatal(err)
	}
	if metas, err := delivery.ListServiceSecrets(ctx, testUser("owner"), service.ID); err != nil || len(metas) != 0 {
		t.Fatalf("list after delete = %+v, %v", metas, err)
	}
	if err := delivery.DeleteServiceSecret(ctx, testUser("owner"), service.ID, "MISSING"); !errors.Is(err, secretkeys.ErrNoSuchSecret) {
		t.Fatalf("delete missing = %v", err)
	}
	pinned, err := store.secrets.Sealed().OpenVersion(ctx, store.db, service.ID, "TOKEN", 1)
	if err != nil {
		t.Fatalf("pinned open after delete: %v", err)
	}
	if string(pinned) != "first" {
		t.Fatalf("pinned open = %q", pinned)
	}
	// Re-sealing resurrects the name at a new version.
	if version, err := delivery.SealServiceSecret(ctx, testUser("owner"), service.ID, "TOKEN", []byte("third")); err != nil || version != 3 {
		t.Fatalf("resurrect seal = %d, %v", version, err)
	}
}

func TestSealedSecretsDesiredStateMerge(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "sealed")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-2")); err != nil {
		t.Fatal(err)
	}
	delivery := testDelivery(store)
	first, err := createScheduledService(ctx, store, "owner", environmentID, "first",
		directImageServiceSpec(pinnedImage("a"), &platformv1.ServiceRuntime{
			Env: map[string]string{"PUBLIC": "one"},
		}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createService(ctx, store, "owner", environmentID, "second",
		directImageServiceSpec(pinnedImage("a"), nil), "node-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := delivery.SealServiceSecret(ctx, testUser("owner"), first.ID, "TOKEN", []byte("node-1-secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := releaseEnvironmentServiceForTest(ctx, store, "owner", environmentID, first.ID); err != nil {
		t.Fatal(err)
	}

	state, err := desiredStateForAgent(ctx, store, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.GetServices()) != 1 {
		t.Fatalf("node-1 services = %d", len(state.GetServices()))
	}
	env := state.GetServices()[0].GetSpec().GetRuntime().GetEnv()
	if env["PUBLIC"] != "one" || env["TOKEN"] != "node-1-secret" {
		t.Fatalf("node-1 env = %v", redactEnvForTest(env))
	}
	// The other agent's service carries no sealed values from this one.
	other, err := desiredStateForAgent(ctx, store, "node-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(other.GetServices()) != 1 {
		t.Fatalf("node-2 services = %d", len(other.GetServices()))
	}
	if _, ok := other.GetServices()[0].GetSpec().GetRuntime().GetEnv()["TOKEN"]; ok {
		t.Fatal("sealed value leaked to another agent's service")
	}
}

func TestAgentSyncDecryptsOnlyChangedAllocations(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "scoped-sync")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	delivery := testDelivery(store)
	first, err := createScheduledService(ctx, store, "owner", environmentID, "first", directImageServiceSpec(pinnedImage("a"), nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := delivery.SealServiceSecret(ctx, testUser("owner"), first.ID, "TOKEN", []byte("first-secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := releaseEnvironmentServiceForTest(ctx, store, "owner", environmentID, first.ID); err != nil {
		t.Fatal(err)
	}
	baseline, err := delivery.PlanAgentSync(ctx, deliverycore.AgentSyncRequest{AgentID: "node-1", RequireCheckpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline.Checkpoint.GetServices()) != 1 || baseline.Checkpoint.GetServices()[0].GetSpec().GetRuntime().GetEnv()["TOKEN"] != "first-secret" {
		t.Fatal("checkpoint did not establish the decrypted allocation baseline")
	}
	delivery.AgentCheckpointSent(baseline.Checkpoint)
	// Fault injection makes any unnecessary re-decryption of the unchanged
	// allocation observable. No product row or desired revision changed.
	if _, err := store.db.ExecContext(ctx, `UPDATE service_secret_versions SET ciphertext = $1 WHERE service_id = $2`, []byte("corrupt"), first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := createScheduledService(ctx, store, "owner", environmentID, "second", directImageServiceSpec(pinnedImage("b"), nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := delivery.SealServiceSecret(ctx, testUser("owner"), second.ID, "TOKEN", []byte("second-secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := releaseEnvironmentServiceForTest(ctx, store, "owner", environmentID, second.ID); err != nil {
		t.Fatal(err)
	}
	plan, err := delivery.PlanAgentSync(ctx, deliverycore.AgentSyncRequest{AgentID: "node-1", BaseRevision: baseline.Cursor, OverlayVersion: baseline.OverlayVersion})
	if err != nil {
		t.Fatalf("adding an allocation read the unchanged allocation's sealed values: %v", err)
	}
	if plan.Checkpoint != nil || len(plan.Diffs) != 1 || len(plan.Diffs[0].GetStarts()) != 1 || len(plan.Diffs[0].GetUpdates()) != 0 {
		t.Fatalf("expected only the new allocation in a diff: %+v", plan)
	}
	added := plan.Diffs[0].GetStarts()[0]
	if added.GetServiceId() != second.ID || added.GetSpec().GetRuntime().GetEnv()["TOKEN"] != "second-secret" || added.GetRegistryPassword() != "" {
		t.Fatal("diff did not carry the new allocation's decrypted environment without registry credentials")
	}
	// A real invalidation of the first allocation must still surface corruption.
	if err := store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := journal.AssignmentRow(baseline.Checkpoint.GetServices()[0].GetAllocationId()).Exec(ctx, tx,
			`UPDATE allocation_assignments SET operator_restart_nonce = operator_restart_nonce + 1 WHERE id = $1`, baseline.Checkpoint.GetServices()[0].GetAllocationId())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := delivery.PlanAgentSync(ctx, deliverycore.AgentSyncRequest{AgentID: "node-1", BaseRevision: plan.Cursor, OverlayVersion: plan.OverlayVersion}); err == nil {
		t.Fatal("changed allocation silently ignored corrupted sealed values")
	}
}

func TestAgentSyncPreservesSealedPinsAcrossCoexistingDeployments(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "overlapping-pins")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	delivery := testDelivery(store)
	service, err := createService(ctx, store, "owner", environmentID, "web", directImageServiceSpec(pinnedImage("a"), nil), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := delivery.SealServiceSecret(ctx, testUser("owner"), service.ID, "TOKEN", []byte("v1-secret")); err != nil {
		t.Fatal(err)
	}
	if _, exists := desiredEnvForTest(t, store, ctx, "node-1", service.ID)["TOKEN"]; exists {
		t.Fatal("new sealed name reached an allocation whose deployment captured no secrets")
	}
	release := func(image string) {
		t.Helper()
		if _, _, err := updateService(ctx, store, "owner", service.ID, service.Name, directImageServiceSpec(pinnedImage(image), nil)); err != nil {
			t.Fatal(err)
		}
		if _, err := releaseEnvironmentServiceForTest(ctx, store, "owner", environmentID, service.ID); err != nil {
			t.Fatal(err)
		}
	}
	release("b")
	completeActionRollout(t, store, service.ID)
	firstDeployment := currentDeploymentForTest(t, store, ctx, service.ID)
	baseline, err := delivery.PlanAgentSync(ctx, deliverycore.AgentSyncRequest{AgentID: "node-1", RequireCheckpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline.Checkpoint.GetServices()) != 1 || baseline.Checkpoint.GetServices()[0].GetSpec().GetRuntime().GetEnv()["TOKEN"] != "v1-secret" {
		t.Fatal("first deployment did not establish its pinned sealed environment")
	}
	delivery.AgentCheckpointSent(baseline.Checkpoint)
	if _, err := delivery.SealServiceSecret(ctx, testUser("owner"), service.ID, "TOKEN", []byte("v2-secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := delivery.SealServiceSecret(ctx, testUser("owner"), service.ID, "LATER", []byte("new-name")); err != nil {
		t.Fatal(err)
	}
	stillPinned := desiredEnvForTest(t, store, ctx, "node-1", service.ID)
	if stillPinned["TOKEN"] != "v1-secret" {
		t.Fatal("draft secret version changed an existing deployment's captured value")
	}
	if _, exists := stillPinned["LATER"]; exists {
		t.Fatal("new sealed name reached an existing pinned deployment before release")
	}
	release("c") // Keep the prior serving allocation until the replacement is ready.
	secondDeployment := currentDeploymentForTest(t, store, ctx, service.ID)
	if firstDeployment.SealedVersions["TOKEN"] != 1 || secondDeployment.SealedVersions["TOKEN"] != 2 {
		t.Fatal("fixture did not capture distinct immutable deployment versions")
	}
	incremental, err := delivery.PlanAgentSync(ctx, deliverycore.AgentSyncRequest{AgentID: "node-1", BaseRevision: baseline.Cursor, OverlayVersion: baseline.OverlayVersion})
	if err != nil {
		t.Fatal(err)
	}
	if incremental.Checkpoint != nil || len(incremental.Diffs) == 0 {
		t.Fatal("coexisting rollout should extend the retained diff baseline")
	}
	accepted := make(map[string]*agentv1.DesiredService)
	for _, svc := range baseline.Checkpoint.GetServices() {
		accepted[svc.GetAllocationId()] = svc
	}
	for _, diff := range incremental.Diffs {
		for _, svc := range append(append([]*agentv1.DesiredService(nil), diff.GetStarts()...), diff.GetUpdates()...) {
			accepted[svc.GetAllocationId()] = svc
		}
		for _, id := range diff.GetStops() {
			delete(accepted, id)
		}
	}
	checkpoint, err := delivery.PlanAgentSync(ctx, deliverycore.AgentSyncRequest{AgentID: "node-1", RequireCheckpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted) != 2 || len(checkpoint.Checkpoint.GetServices()) != 2 {
		t.Fatalf("expected old and new allocations together, got diff=%d checkpoint=%d", len(accepted), len(checkpoint.Checkpoint.GetServices()))
	}
	wanted := map[string]string{firstDeployment.ID: "v1-secret", secondDeployment.ID: "v2-secret"}
	for _, svc := range checkpoint.Checkpoint.GetServices() {
		if value := svc.GetSpec().GetRuntime().GetEnv()["TOKEN"]; value != wanted[svc.GetDeploymentId()] {
			t.Fatalf("deployment %s resolved another deployment's sealed version", svc.GetDeploymentId())
		}
		later, exists := svc.GetSpec().GetRuntime().GetEnv()["LATER"]
		if svc.GetDeploymentId() == firstDeployment.ID && exists || svc.GetDeploymentId() == secondDeployment.ID && later != "new-name" {
			t.Fatalf("new sealed name did not follow its captured deployment: %s", svc.GetDeploymentId())
		}
		if !proto.Equal(accepted[svc.GetAllocationId()], svc) {
			t.Fatalf("checkpoint and applied diffs disagree for allocation %s", svc.GetAllocationId())
		}
	}
}

func TestSealedSecretsRollbackRestoresPinned(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	userID := "owner"
	project, err := store.catalog.createProject(ctx, testUser(userID), "sealed")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	delivery := testDelivery(store)
	service, err := createService(ctx, store, userID, environmentID, "web",
		directImageServiceSpec(pinnedImage("a"), nil), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	// Seal v1, then release a spec change: the release pins v1.
	if _, err := delivery.SealServiceSecret(ctx, testUser(userID), service.ID, "TOKEN", []byte("v1-secret")); err != nil {
		t.Fatal(err)
	}
	release := func(spec *platformv1.ServiceSpec) deliverycore.DeploymentRecord {
		t.Helper()
		if _, _, err := updateService(ctx, store, userID, service.ID, service.Name, spec); err != nil {
			t.Fatal(err)
		}
		if _, err := releaseEnvironmentServiceForTest(ctx, store, userID, environmentID, service.ID); err != nil {
			t.Fatal(err)
		}
		completeActionRollout(t, store, service.ID)
		return currentDeploymentForTest(t, store, ctx, service.ID)
	}
	first := release(directImageServiceSpec(pinnedImage("b"), &platformv1.ServiceRuntime{
		Env: map[string]string{"STAGE": "two"},
	}))
	if first.SealedVersions["TOKEN"] != 1 {
		t.Fatalf("first deployment pins = %v", first.SealedVersions)
	}
	if _, ok := first.VariableVersions["TOKEN"]; ok {
		t.Fatalf("sealed pin leaked into public variable versions: %v", first.VariableVersions)
	}

	if _, err := delivery.SealServiceSecret(ctx, testUser(userID), service.ID, "TOKEN", []byte("v2-secret")); err != nil {
		t.Fatal(err)
	}
	second := release(directImageServiceSpec(pinnedImage("c"), &platformv1.ServiceRuntime{
		Env: map[string]string{"STAGE": "three"},
	}))
	if second.SealedVersions["TOKEN"] != 2 {
		t.Fatalf("second deployment pins = %v", second.SealedVersions)
	}
	if got := desiredEnvForTest(t, store, ctx, "node-1", service.ID)["TOKEN"]; got != "v2-secret" {
		t.Fatalf("desired before rollback TOKEN = %q", got)
	}

	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, first.ID,
		platformv1.DeploymentAction_DEPLOYMENT_ACTION_ROLLBACK, "rollback-1", ""); err != nil {
		t.Fatal(err)
	}
	completeActionRollout(t, store, service.ID)
	rolled := currentDeploymentForTest(t, store, ctx, service.ID)
	if rolled.SealedVersions["TOKEN"] != 1 {
		t.Fatalf("rolled deployment pins = %v", rolled.SealedVersions)
	}
	if got := desiredEnvForTest(t, store, ctx, "node-1", service.ID)["TOKEN"]; got != "v1-secret" {
		t.Fatalf("desired after rollback TOKEN = %q", got)
	}
}

func TestSealedSecretsNoPlaintextAtRest(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "sealed")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "owner", environmentID, "web",
		directImageServiceSpec(pinnedImage("a"), nil), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	canary := "canary-no-plaintext-" + service.ID
	if _, err := testDelivery(store).SealServiceSecret(ctx, testUser("owner"), service.ID, "TOKEN", []byte(canary)); err != nil {
		t.Fatal(err)
	}
	// Sealed values must not appear in revision JSON, deployments, journal, or ciphertext rows.
	scoped := []string{
		`SELECT spec_json::STRING FROM service_revisions WHERE service_id = $1`,
		`SELECT resolved_spec_json::STRING FROM deployments WHERE service_id = $1`,
		`SELECT variable_versions_json::STRING FROM deployments WHERE service_id = $1`,
		`SELECT sealed_versions_json::STRING FROM deployments WHERE service_id = $1`,
		`SELECT encode(nonce, 'hex') || encode(ciphertext, 'hex') FROM service_secret_versions WHERE service_id = $1`,
	}
	global := []string{
		`SELECT payload::STRING FROM cluster_journal`,
		`SELECT payload::STRING FROM cluster_journal_receipts`,
		`SELECT encode(wrapped_dek, 'hex') FROM envelope_data_keys`,
		`SELECT provider_ref FROM envelope_keys`,
	}
	for _, query := range scoped {
		assertNoCanary(t, store, ctx, query, canary, service.ID)
	}
	for _, query := range global {
		assertNoCanary(t, store, ctx, query, canary)
	}
}

func assertNoCanary(t *testing.T, store *persistence, ctx context.Context, query, canary string, args ...any) {
	t.Helper()
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var value sql.NullString
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		if value.Valid && strings.Contains(value.String, canary) {
			t.Fatalf("plaintext canary present at rest via %q", query)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestSealedSecretsDuplicateEnvironmentExcludes(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "sealed")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	delivery := testDelivery(store)
	service, err := createService(ctx, store, "owner", environmentID, "web",
		directImageServiceSpec(pinnedImage("a"), &platformv1.ServiceRuntime{
			Env: map[string]string{"PUBLIC": "one"},
		}), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := delivery.SealServiceSecret(ctx, testUser("owner"), service.ID, "TOKEN", []byte("original")); err != nil {
		t.Fatal(err)
	}
	duplicate, err := delivery.DuplicateEnvironment(ctx, testUser("owner"), environmentID, "preview", true)
	if err != nil {
		t.Fatal(err)
	}
	services, err := store.reads.ListServices(ctx, testUser("owner"), duplicate.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 {
		t.Fatalf("duplicate services = %d", len(services))
	}
	if got := services[0].Spec.GetRuntime().GetEnv()["PUBLIC"]; got != "one" {
		t.Fatalf("public env was not copied: %v", services[0].Spec.GetRuntime().GetEnv())
	}
	metas, err := delivery.ListServiceSecrets(ctx, testUser("owner"), services[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 0 {
		t.Fatalf("sealed secrets were copied: %+v", metas)
	}
}

func TestSealedSecretsRPCWiring(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "sealed")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "owner", environmentID, "web",
		directImageServiceSpec(pinnedImage("a"), nil), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	rpc := newPlatformService(store.platform(), noopNotifier{}, noopIngress{}, testDelivery(store))
	userCtx := contextWithDelegatedUser("owner", "owner@example.com")
	sealed, err := rpc.SealServiceSecret(userCtx, &platformv1.SealServiceSecretRequest{
		ServiceId: service.ID, Name: "TOKEN", Value: "rpc-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sealed.GetVersion() != 1 {
		t.Fatalf("rpc seal version = %d", sealed.GetVersion())
	}
	listed, err := rpc.ListServiceSecrets(userCtx, &platformv1.ListServiceSecretsRequest{ServiceId: service.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.GetSecrets()) != 1 || listed.GetSecrets()[0].GetName() != "TOKEN" {
		t.Fatalf("rpc list = %+v", listed.GetSecrets())
	}
	if _, err := rpc.DeleteServiceSecret(userCtx, &platformv1.DeleteServiceSecretRequest{
		ServiceId: service.ID, Name: "TOKEN",
	}); err != nil {
		t.Fatal(err)
	}
	listed, err = rpc.ListServiceSecrets(userCtx, &platformv1.ListServiceSecretsRequest{ServiceId: service.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.GetSecrets()) != 0 {
		t.Fatalf("rpc list after delete = %+v", listed.GetSecrets())
	}
	// Oversize values are caller errors, and the value never appears in the error.
	oversize := strings.Repeat("S", secretkeys.MaxSealedValueSize+1)
	if _, err := rpc.SealServiceSecret(userCtx, &platformv1.SealServiceSecretRequest{
		ServiceId: service.ID, Name: "BIG", Value: oversize,
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("oversize seal code = %v, want InvalidArgument", err)
	} else if strings.Contains(err.Error(), oversize) {
		t.Fatal("oversize value leaked into the error")
	}
}

func TestSealedSecretsRollbackRejectsSealedNameConflict(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	userID := "owner"
	project, err := store.catalog.createProject(ctx, testUser(userID), "sealed")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	delivery := testDelivery(store)
	service, err := createService(ctx, store, userID, environmentID, "web",
		directImageServiceSpec(pinnedImage("a"), &platformv1.ServiceRuntime{
			Env: map[string]string{"MOVED": "public"},
		}), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	release := func(spec *platformv1.ServiceSpec) deliverycore.DeploymentRecord {
		t.Helper()
		if _, _, err := updateService(ctx, store, userID, service.ID, service.Name, spec); err != nil {
			t.Fatal(err)
		}
		if _, err := releaseEnvironmentServiceForTest(ctx, store, userID, environmentID, service.ID); err != nil {
			t.Fatal(err)
		}
		completeActionRollout(t, store, service.ID)
		return currentDeploymentForTest(t, store, ctx, service.ID)
	}
	// First release keeps MOVED public; the second drops it; then MOVED moves to sealed.
	first := release(directImageServiceSpec(pinnedImage("b"), &platformv1.ServiceRuntime{
		Env: map[string]string{"MOVED": "public"},
	}))
	release(directImageServiceSpec(pinnedImage("c"), nil))
	if _, err := delivery.SealServiceSecret(ctx, testUser(userID), service.ID, "MOVED", []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	// Rolling back to a spec that still carries MOVED as public must fail closed:
	// resurrecting it while sealed silently wins would lie about what is running.
	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, first.ID,
		platformv1.DeploymentAction_DEPLOYMENT_ACTION_ROLLBACK, "rollback-1", ""); !errors.Is(err, deliverycore.ErrSealedNameConflict) {
		t.Fatalf("rollback with sealed conflict = %v, want ErrSealedNameConflict", err)
	}
}

func TestSealedSecretsConcurrentSealSameName(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	userID := "owner"
	project, err := store.catalog.createProject(ctx, testUser(userID), "sealed")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	delivery := testDelivery(store)
	service, err := createService(ctx, store, userID, environmentID, "web",
		directImageServiceSpec(pinnedImage("a"), nil), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	// Concurrent seals for one name race on max(version)+1; every caller lands a
	// distinct version instead of failing with a duplicate-key error.
	const racers = 8
	versions := make([]int64, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			version, err := delivery.SealServiceSecret(ctx, testUser(userID), service.ID, "TOKEN", []byte("racer"))
			versions[i], errs[i] = version, err
		}(i)
	}
	wg.Wait()
	seen := map[int64]bool{}
	for i := 0; i < racers; i++ {
		if errs[i] != nil {
			t.Fatalf("racer %d: %v", i, errs[i])
		}
		if versions[i] < 1 || versions[i] > racers || seen[versions[i]] {
			t.Fatalf("versions = %v, want distinct 1..%d", versions, racers)
		}
		seen[versions[i]] = true
	}
}

func desiredEnvForTest(t *testing.T, store *persistence, ctx context.Context, agentID, serviceID string) map[string]string {
	t.Helper()
	state, err := desiredStateForAgent(ctx, store, agentID)
	if err != nil {
		t.Fatal(err)
	}
	for _, svc := range state.GetServices() {
		if svc.GetServiceId() == serviceID {
			return svc.GetSpec().GetRuntime().GetEnv()
		}
	}
	t.Fatalf("no desired service %s for agent %s", serviceID, agentID)
	return nil
}

func redactEnvForTest(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for key := range env {
		out[key] = "<redacted>"
	}
	return out
}
