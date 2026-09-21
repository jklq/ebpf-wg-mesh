//go:build integration

package controlplane

import (
	"context"
	"errors"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
)

func TestSecretEnvelopeRewrapAcrossReplicas(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "envelope")
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
	if _, err := delivery.SealServiceSecret(ctx, testUser("owner"), service.ID, "TOKEN", []byte("first-secret")); err != nil {
		t.Fatal(err)
	}

	// Replica 2 shares the database and provider but holds its own caches.
	replica2 := secretkeys.New(store.db, store.secrets.Provider())
	first, err := store.secrets.Registry().ActiveKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := replica2.Registry().Rotate(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.ID == first.ID {
		t.Fatal("rotation kept the same active key")
	}
	// New wraps use the new active key while the retired key still unwraps:
	// rewrap from replica 1 converges the shared rows.
	rewrapped, err := store.secrets.DEKs().RewrapAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rewrapped != 1 {
		t.Fatalf("rewrapped %d, want 1", rewrapped)
	}
	if rewrapped, err := replica2.DEKs().RewrapAll(ctx); err != nil || rewrapped != 0 {
		t.Fatalf("second rewrap = %d, %v; want 0, nil", rewrapped, err)
	}
	for name, svc := range map[string]*secretkeys.Service{"replica1": store.secrets, "replica2": replica2} {
		plaintext, version, err := svc.Sealed().OpenCurrent(ctx, store.db, service.ID, "TOKEN")
		if err != nil {
			t.Fatalf("%s open: %v", name, err)
		}
		if string(plaintext) != "first-secret" || version != 1 {
			t.Fatalf("%s open = %q v%d", name, plaintext, version)
		}
	}
	counts, err := store.secrets.Registry().WrappedCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts[first.ID] != 0 || counts[rotated.ID] != 1 {
		t.Fatalf("wrapped counts = %v", counts)
	}
	// The retired key deletes cleanly once nothing references it.
	if err := store.secrets.Registry().DeleteKey(ctx, first.ID); err != nil {
		t.Fatalf("delete retired key: %v", err)
	}
}

func TestSecretEnvelopeRestartWithRetiredKey(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "envelope")
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
	if _, err := testDelivery(store).SealServiceSecret(ctx, testUser("owner"), service.ID, "TOKEN", []byte("restart-secret")); err != nil {
		t.Fatal(err)
	}
	// Rotate without rewrapping: the DEK stays wrapped by the retired key.
	if _, err := store.secrets.Registry().Rotate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	// A restarted replica (cold caches) still unwraps through the retired key.
	restarted := secretkeys.New(store.db, store.secrets.Provider())
	plaintext, _, err := restarted.Sealed().OpenCurrent(ctx, store.db, service.ID, "TOKEN")
	if err != nil {
		t.Fatalf("restarted open: %v", err)
	}
	if string(plaintext) != "restart-secret" {
		t.Fatalf("restarted open = %q", plaintext)
	}
}

func TestSecretEnvelopeUnwrapDiagnostics(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "envelope")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, _, err := store.secrets.DEKs().DEKForScope(ctx, store.db, secretkeys.DEKScopeKindEnvironment, environmentID); err != nil {
		t.Fatal(err)
	}

	// Unknown key IDs are reported, not silently retried.
	if _, err := store.secrets.Registry().Unwrap(ctx, "kek-missing", []byte("x")); !isUnwrapReason(t, err, secretkeys.UnwrapReasonUnknownKey) {
		t.Fatalf("unknown key unwrap = %v", err)
	}
	// Corrupt wrapped bytes fail authentication.
	active, err := store.secrets.Registry().ActiveKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := []byte{0x01, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}
	if _, err := store.secrets.Registry().Unwrap(ctx, active.ID, corrupt); !isUnwrapReason(t, err, secretkeys.UnwrapReasonCorruptCiphertext) {
		t.Fatalf("corrupt unwrap = %v", err)
	}
	// Missing provider material (fresh empty key directory against the
	// same shared database) is distinguished from corruption.
	provider, err := secretkeys.NewFileProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	orphaned := secretkeys.New(store.db, provider)
	dekID := dekIDForScope(t, store, ctx, environmentID)
	var wrapped []byte
	if err := store.db.QueryRowContext(ctx, `SELECT wrapped_dek FROM envelope_data_keys WHERE id = $1`, dekID).Scan(&wrapped); err != nil {
		t.Fatal(err)
	}
	if _, err := orphaned.Registry().Unwrap(ctx, active.ID, wrapped); !isUnwrapReason(t, err, secretkeys.UnwrapReasonMissingMaterial) {
		t.Fatalf("orphaned unwrap = %v", err)
	}
}

func TestSecretEnvelopeDeleteRefusal(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	active, err := store.secrets.Registry().ActiveKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.secrets.Registry().DeleteKey(ctx, active.ID); !errors.Is(err, secretkeys.ErrKeyIsActive) {
		t.Fatalf("delete active = %v", err)
	}
	if err := store.secrets.Registry().DeleteKey(ctx, "kek-missing"); !errors.Is(err, secretkeys.ErrUnknownKey) {
		t.Fatalf("delete unknown = %v", err)
	}
	project, err := store.catalog.createProject(ctx, testUser("owner"), "envelope")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, _, err := store.secrets.DEKs().DEKForScope(ctx, store.db, secretkeys.DEKScopeKindEnvironment, environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.secrets.Registry().Rotate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.secrets.Registry().DeleteKey(ctx, active.ID); !errors.Is(err, secretkeys.ErrKeyHasLiveCiphertext) {
		t.Fatalf("delete with live ciphertext = %v", err)
	}
	if _, err := store.secrets.DEKs().RewrapAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.secrets.Registry().DeleteKey(ctx, active.ID); err != nil {
		t.Fatalf("delete after rewrap: %v", err)
	}
	if _, err := store.secrets.Registry().ActiveKey(ctx); err != nil {
		t.Fatal(err)
	}
}

func isUnwrapReason(t *testing.T, err error, reason secretkeys.UnwrapFailureReason) bool {
	t.Helper()
	var unwrapErr *secretkeys.UnwrapError
	if !errors.As(err, &unwrapErr) {
		return false
	}
	return unwrapErr.Reason == reason
}

func dekIDForScope(t *testing.T, store *persistence, ctx context.Context, environmentID string) string {
	t.Helper()
	var id string
	if err := store.db.QueryRowContext(ctx,
		`SELECT id FROM envelope_data_keys WHERE scope_kind = 'environment' AND scope_id = $1`, environmentID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
