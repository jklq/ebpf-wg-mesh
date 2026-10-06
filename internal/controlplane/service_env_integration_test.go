//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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

func envServiceSpec(image string, env map[string]string) *platformv1.ServiceSpec {
	return directImageServiceSpec(pinnedImage(image), &platformv1.ServiceRuntime{Env: env})
}

func TestServiceEnvRoundTripsEncryptedAtRest(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "env")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	canary := "canary-no-plaintext-" + project.ID
	service, err := createService(ctx, store, "owner", environmentID, "web",
		envServiceSpec("a", map[string]string{"TOKEN": canary}), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	updated, _, err := updateService(ctx, store, "owner", service.ID, service.Name,
		envServiceSpec("a", map[string]string{"TOKEN": canary, "SECOND": canary + "-2"}))
	if err != nil {
		t.Fatal(err)
	}
	// Readers see env exactly as written.
	read, err := store.reads.ServiceByID(ctx, testUser("owner"), service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if env := read.Spec.GetRuntime().GetEnv(); len(env) != 2 || env["TOKEN"] != canary || env["SECOND"] != canary+"-2" {
		t.Fatalf("read env = %v", redactEnvForTest(env))
	}
	if updated.SpecRevision != read.SpecRevision {
		t.Fatalf("read revision = %d, want %d", read.SpecRevision, updated.SpecRevision)
	}
	if got := desiredEnvForTest(t, store, ctx, "node-1", service.ID)["TOKEN"]; got != canary {
		t.Fatal("agent desired state did not carry the decrypted env")
	}
	// Plaintext must not appear in revisions, deployments, the journal, or key rows.
	var withEnv int
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM service_revisions WHERE service_id = $1 AND env_ciphertext IS NOT NULL`, service.ID).Scan(&withEnv); err != nil {
		t.Fatal(err)
	}
	if withEnv != 2 {
		t.Fatalf("revisions with env ciphertext = %d, want 2", withEnv)
	}
	scoped := []string{
		`SELECT spec_json::TEXT FROM service_revisions WHERE service_id = $1`,
		`SELECT encode(env_ciphertext, 'escape') FROM service_revisions WHERE service_id = $1`,
		`SELECT row_to_json(d)::TEXT FROM deployments d WHERE service_id = $1`,
	}
	global := []string{
		`SELECT payload::TEXT FROM cluster_journal`,
		`SELECT payload::TEXT FROM cluster_journal_receipts`,
		`SELECT encode(wrapped_dek, 'escape') FROM envelope_data_keys`,
		`SELECT provider_ref FROM envelope_keys`,
	}
	for _, query := range scoped {
		assertNoCanary(t, store, ctx, query, canary, service.ID)
	}
	for _, query := range global {
		assertNoCanary(t, store, ctx, query, canary)
	}
	// Ciphertext is bound to its revision: transplanting it to another revision fails closed.
	if _, err := store.db.ExecContext(ctx,
		`UPDATE service_revisions SET env_ciphertext = (SELECT env_ciphertext FROM service_revisions WHERE service_id = $1 AND spec_revision = $2)
		  WHERE service_id = $1 AND spec_revision = $3`, service.ID, service.SpecRevision, updated.SpecRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := store.reads.ServiceByID(ctx, testUser("owner"), service.ID); err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("transplanted env ciphertext read = %v", err)
	}
}

func TestServiceEnvValidation(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "env")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "owner", environmentID, "web", envServiceSpec("a", nil), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"has space", "has-dash", "9START", "PLATFORM_RESERVED", strings.Repeat("N", 129)} {
		if _, _, err := updateService(ctx, store, "owner", service.ID, service.Name,
			envServiceSpec("a", map[string]string{name: "x"})); !errors.Is(err, deliverycore.ErrInvalidServiceEnv) {
			t.Fatalf("update with env name %q = %v", name, err)
		}
	}
	oversize := strings.Repeat("S", deliverycore.MaxEnvValueBytes+1)
	if _, _, err := updateService(ctx, store, "owner", service.ID, service.Name,
		envServiceSpec("a", map[string]string{"BIG": oversize})); !errors.Is(err, deliverycore.ErrInvalidServiceEnv) {
		t.Fatalf("oversize env value = %v", err)
	}
	rpc := newPlatformService(store.platform(), noopNotifier{}, noopIngress{}, testDelivery(store))
	userCtx := contextWithDelegatedUser("owner", "owner@example.com")
	_, err = rpc.UpdateService(userCtx, &platformv1.UpdateServiceRequest{
		ExpectedSpecRevision: service.SpecRevision,
		ServiceId:            service.ID,
		Service:              &platformv1.ServiceUpdate{Name: service.Name, Spec: envServiceSpec("a", map[string]string{"BIG": oversize})},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("rpc oversize env code = %v, want InvalidArgument", err)
	}
	if strings.Contains(err.Error(), oversize) {
		t.Fatal("oversize value leaked into the error")
	}
}

func TestServiceEnvDesiredStateMerge(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "env")
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
	if _, err := createService(ctx, store, "owner", environmentID, "first",
		envServiceSpec("a", map[string]string{"TOKEN": "node-1-secret"}), "node-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := createService(ctx, store, "owner", environmentID, "second", envServiceSpec("a", nil), "node-2"); err != nil {
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
	if env["TOKEN"] != "node-1-secret" || env["PLATFORM_SERVICE_NAME"] != "first" {
		t.Fatalf("node-1 env = %v", redactEnvForTest(env))
	}
	// The other agent's service carries no env from this one.
	other, err := desiredStateForAgent(ctx, store, "node-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(other.GetServices()) != 1 {
		t.Fatalf("node-2 services = %d", len(other.GetServices()))
	}
	if _, ok := other.GetServices()[0].GetSpec().GetRuntime().GetEnv()["TOKEN"]; ok {
		t.Fatal("env leaked to another agent's service")
	}
}

func TestAgentSyncDecryptsOnlyRenderedAllocations(t *testing.T) {
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
	first, err := createScheduledService(ctx, store, "owner", environmentID, "first",
		envServiceSpec("a", map[string]string{"TOKEN": "first-secret"}))
	if err != nil {
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
	second, err := createScheduledService(ctx, store, "owner", environmentID, "second",
		envServiceSpec("b", map[string]string{"TOKEN": "second-secret"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := releaseEnvironmentServiceForTest(ctx, store, "owner", environmentID, second.ID); err != nil {
		t.Fatal(err)
	}
	// Only the new allocation renders, so only its revision env decrypts.
	plan, err := delivery.PlanAgentSync(ctx, deliverycore.AgentSyncRequest{AgentID: "node-1", BaseRevision: baseline.Cursor, OverlayVersion: baseline.OverlayVersion})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Checkpoint != nil || len(plan.Diffs) != 1 || len(plan.Diffs[0].GetStarts()) != 1 || len(plan.Diffs[0].GetUpdates()) != 0 {
		t.Fatalf("expected only the new allocation in a diff: %+v", plan)
	}
	added := plan.Diffs[0].GetStarts()[0]
	if added.GetServiceId() != second.ID || added.GetSpec().GetRuntime().GetEnv()["TOKEN"] != "second-secret" || added.GetRegistryPassword() != "" {
		t.Fatal("diff did not carry the new allocation's decrypted environment without registry credentials")
	}
	// Corrupted ciphertext reaching the first allocation's revision must fail
	// closed instead of shipping the workload without its env.
	firstRevision := baseline.Checkpoint.GetServices()[0].GetDesiredSpecRevision()
	if err := store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := journal.RevisionRow(first.ID, firstRevision).Exec(ctx, tx,
			`UPDATE service_revisions SET env_ciphertext = $1 WHERE service_id = $2 AND spec_revision = $3`, []byte("corrupt-ciphertext"), first.ID, firstRevision)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := delivery.PlanAgentSync(ctx, deliverycore.AgentSyncRequest{AgentID: "node-1", BaseRevision: plan.Cursor, OverlayVersion: plan.OverlayVersion}); err == nil {
		t.Fatal("changed allocation silently ignored corrupted env ciphertext")
	}
}

func TestAgentSyncKeepsRevisionEnvAcrossCoexistingDeployments(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "overlapping-env")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	delivery := testDelivery(store)
	service, err := createService(ctx, store, "owner", environmentID, "web", envServiceSpec("a", nil), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	release := func(image string, env map[string]string) {
		t.Helper()
		if _, _, err := updateService(ctx, store, "owner", service.ID, service.Name, envServiceSpec(image, env)); err != nil {
			t.Fatal(err)
		}
		if _, err := releaseEnvironmentServiceForTest(ctx, store, "owner", environmentID, service.ID); err != nil {
			t.Fatal(err)
		}
	}
	release("b", map[string]string{"TOKEN": "v1-secret"})
	completeActionRollout(t, store, service.ID)
	firstDeployment := currentDeploymentForTest(t, store, ctx, service.ID)
	baseline, err := delivery.PlanAgentSync(ctx, deliverycore.AgentSyncRequest{AgentID: "node-1", RequireCheckpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline.Checkpoint.GetServices()) != 1 || baseline.Checkpoint.GetServices()[0].GetSpec().GetRuntime().GetEnv()["TOKEN"] != "v1-secret" {
		t.Fatal("first deployment did not establish its environment")
	}
	delivery.AgentCheckpointSent(baseline.Checkpoint)
	// A draft env change reaches no allocation until it is released.
	if _, _, err := updateService(ctx, store, "owner", service.ID, service.Name,
		envServiceSpec("b", map[string]string{"TOKEN": "draft", "LATER": "draft"})); err != nil {
		t.Fatal(err)
	}
	if env := desiredEnvForTest(t, store, ctx, "node-1", service.ID); env["TOKEN"] != "v1-secret" || env["LATER"] != "" {
		t.Fatal("draft env changed an existing deployment's environment")
	}
	release("c", map[string]string{"TOKEN": "v2-secret", "LATER": "new-name"}) // Keep the prior serving allocation until the replacement is ready.
	secondDeployment := currentDeploymentForTest(t, store, ctx, service.ID)
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
		env := svc.GetSpec().GetRuntime().GetEnv()
		if env["TOKEN"] != wanted[svc.GetDeploymentId()] {
			t.Fatalf("deployment %s resolved another deployment's env", svc.GetDeploymentId())
		}
		later, exists := env["LATER"]
		if svc.GetDeploymentId() == firstDeployment.ID && exists || svc.GetDeploymentId() == secondDeployment.ID && later != "new-name" {
			t.Fatalf("new variable did not follow its deployment: %s", svc.GetDeploymentId())
		}
		if !proto.Equal(accepted[svc.GetAllocationId()], svc) {
			t.Fatalf("checkpoint and applied diffs disagree for allocation %s", svc.GetAllocationId())
		}
	}
}

func TestServiceEnvRollbackRestoresDeployedValues(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	userID := "owner"
	project, err := store.catalog.createProject(ctx, testUser(userID), "env")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, userID, environmentID, "web", envServiceSpec("a", nil), "node-1")
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
	first := release(envServiceSpec("b", map[string]string{"TOKEN": "v1-secret"}))
	if got := first.ResolvedSpec.GetRuntime().GetEnv()["TOKEN"]; got != "v1-secret" {
		t.Fatal("deployment record did not resolve its revision env")
	}
	release(envServiceSpec("c", map[string]string{"TOKEN": "v2-secret"}))
	if got := desiredEnvForTest(t, store, ctx, "node-1", service.ID)["TOKEN"]; got != "v2-secret" {
		t.Fatal("desired env before rollback did not follow the second release")
	}

	if _, _, err := applyDeploymentActionForTest(ctx, store, userID, service.ID, first.ID,
		platformv1.DeploymentAction_DEPLOYMENT_ACTION_ROLLBACK, "rollback-1", ""); err != nil {
		t.Fatal(err)
	}
	completeActionRollout(t, store, service.ID)
	if got := desiredEnvForTest(t, store, ctx, "node-1", service.ID)["TOKEN"]; got != "v1-secret" {
		t.Fatal("desired env after rollback did not restore the first release")
	}
	read, err := store.reads.ServiceByID(ctx, testUser(userID), service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := read.Spec.GetRuntime().GetEnv()["TOKEN"]; got != "v1-secret" {
		t.Fatal("rolled-back service spec did not carry the restored env")
	}
}

func TestServiceEnvDuplicateEnvironmentReencrypts(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "env")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	delivery := testDelivery(store)
	if _, err := createService(ctx, store, "owner", environmentID, "web",
		envServiceSpec("a", map[string]string{"TOKEN": "original"}), "node-1"); err != nil {
		t.Fatal(err)
	}
	for _, copyVariables := range []bool{true, false} {
		name := "preview"
		if !copyVariables {
			name = "bare"
		}
		duplicate, err := delivery.DuplicateEnvironment(ctx, testUser("owner"), environmentID, name, copyVariables)
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
		env := services[0].Spec.GetRuntime().GetEnv()
		if copyVariables && env["TOKEN"] != "original" || !copyVariables && len(env) != 0 {
			t.Fatalf("copyVariables=%v env = %v", copyVariables, redactEnvForTest(env))
		}
		if !copyVariables {
			continue
		}
		// Copied env is re-encrypted under the duplicate environment's own key.
		var dekID string
		if err := store.db.QueryRowContext(ctx,
			`SELECT env_dek_id FROM service_revisions WHERE service_id = $1`, services[0].ID).Scan(&dekID); err != nil {
			t.Fatal(err)
		}
		if dekID != dekIDForScope(t, store, ctx, duplicate.ID) || dekID == dekIDForScope(t, store, ctx, environmentID) {
			t.Fatalf("duplicate env encrypted under %s", dekID)
		}
	}
}

func TestServiceEnvSurvivesRolledBackKeyMint(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "env")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	// The environment's first encryption mints its DEK inside a transaction that rolls back.
	rollback := errors.New("rollback")
	if err := store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := store.secrets.Encrypt(ctx, tx, secretkeys.EnvironmentScope(environmentID), []byte("aad"), []byte("value")); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatalf("rolled-back mint = %v", err)
	}
	// The discarded mint must not be reused for later writes.
	service, err := createService(ctx, store, "owner", environmentID, "web",
		envServiceSpec("a", map[string]string{"TOKEN": "after-rollback"}), "node-1")
	if err != nil {
		t.Fatalf("create after rolled-back mint: %v", err)
	}
	read, err := store.reads.ServiceByID(ctx, testUser("owner"), service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.Spec.GetRuntime().GetEnv()["TOKEN"] != "after-rollback" {
		t.Fatal("env did not round-trip after a rolled-back key mint")
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
