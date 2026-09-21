//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/secretkeys"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	first, err := createService(ctx, store, "owner", environmentID, "first",
		directImageServiceSpec(pinnedImage("a"), &platformv1.ServiceRuntime{
			Env: map[string]string{"PUBLIC": "one"},
		}), "node-1")
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
	// Sealed values must not appear in revision JSON, deployment rows, the
	// journal, or their own ciphertext rows.
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
	services, err := store.reads.ListServices(ctx, testUser("owner"), duplicate.ID)
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
	rpc := NewPlatformService(store.platform(), noopNotifier{}, noopIngress{}, testDelivery(store))
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
	// Oversize values are caller errors, not internal failures, and the
	// value never appears in the error.
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
	// First release keeps MOVED public; the second drops it; then MOVED
	// moves to sealed.
	first := release(directImageServiceSpec(pinnedImage("b"), &platformv1.ServiceRuntime{
		Env: map[string]string{"MOVED": "public"},
	}))
	release(directImageServiceSpec(pinnedImage("c"), nil))
	if _, err := delivery.SealServiceSecret(ctx, testUser(userID), service.ID, "MOVED", []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	// Rolling back to the deployment whose spec still carries MOVED as
	// public must fail closed: resurrecting it as public while the
	// sealed value silently wins in desired state would lie to the
	// operator about what is running.
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
	// Concurrent seals for one name race on max(version)+1; every caller
	// must land a distinct version instead of one failing with a raw
	// duplicate-key error.
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
