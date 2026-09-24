//go:build integration

package controlplane

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
)

func TestOpenCurrentMissingSecretReturnsSentinel(t *testing.T) {
	store := openTestStore(t)
	_, _, err := store.secrets.Sealed().OpenCurrent(context.Background(), store.db, "missing-service", "TOKEN")
	if !errors.Is(err, secretkeys.ErrNoSuchSecret) {
		t.Fatalf("missing secret error = %v, want ErrNoSuchSecret", err)
	}
}

func TestSecretEnvelopeRotateRewrapAcrossReplicas(t *testing.T) {
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

	// Replica 2 shares the database and keyring file but holds its own
	// caches. The new version is provisioned to the shared keyring first,
	// then activated from replica 2.
	replica2 := secretkeys.New(store.db, store.secrets.Provider())
	first, err := store.secrets.Registry().ActiveKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	version := provisionVersion(t, store.secrets)
	rotated, err := replica2.Registry().Activate(ctx, version)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.ID == first.ID {
		t.Fatal("activation kept the same active key")
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
	// Activate without rewrapping: the DEK stays wrapped by the retired key.
	version := provisionVersion(t, store.secrets)
	if _, err := store.secrets.Registry().Activate(ctx, version); err != nil {
		t.Fatal(err)
	}
	// A restarted replica (cold caches) still unwraps through the retired
	// key with no manual unlock.
	restarted := secretkeys.New(store.db, store.secrets.Provider())
	plaintext, _, err := restarted.Sealed().OpenCurrent(ctx, store.db, service.ID, "TOKEN")
	if err != nil {
		t.Fatalf("restarted open: %v", err)
	}
	if string(plaintext) != "restart-secret" {
		t.Fatalf("restarted open = %q", plaintext)
	}
}

func TestSecretEnvelopeInterruptedRewrapResumes(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	// Two DEKs so a crash can strand one row behind.
	if _, _, err := store.secrets.DEKs().DEKForScope(ctx, store.db, secretkeys.DEKScopeKindEnvironment, "env-a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.secrets.DEKs().DEKForScope(ctx, store.db, secretkeys.DEKScopeKindEnvironment, "env-b"); err != nil {
		t.Fatal(err)
	}
	active, err := store.secrets.Registry().ActiveKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	version := provisionVersion(t, store.secrets)
	rotated, err := store.secrets.Registry().Activate(ctx, version)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the first row commits: advance one row to the
	// active key exactly as one RewrapAll iteration would, then restart
	// with cold caches.
	var dekID, wrappingKeyID string
	var wrapped []byte
	if err := store.db.QueryRowContext(ctx,
		`SELECT id, wrapping_key_id, wrapped_dek FROM envelope_data_keys WHERE scope_id = 'env-a'`,
	).Scan(&dekID, &wrappingKeyID, &wrapped); err != nil {
		t.Fatal(err)
	}
	raw, err := store.secrets.Registry().Unwrap(ctx, wrappingKeyID, secretkeys.DEKWrapPurpose(dekID), wrapped)
	if err != nil {
		t.Fatal(err)
	}
	newKeyID, newWrapped, err := store.secrets.Registry().WrapWithActive(ctx, secretkeys.DEKWrapPurpose(dekID), raw)
	if err != nil {
		t.Fatal(err)
	}
	if newKeyID != rotated.ID {
		t.Fatalf("manual advance wrapped under %s, want %s", newKeyID, rotated.ID)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE envelope_data_keys SET wrapping_key_id = $1, wrapped_dek = $2 WHERE id = $3 AND wrapping_key_id = $4`,
		newKeyID, newWrapped, dekID, wrappingKeyID); err != nil {
		t.Fatal(err)
	}
	restarted := secretkeys.New(store.db, store.secrets.Provider())
	rewrapped, err := restarted.DEKs().RewrapAll(ctx)
	if err != nil {
		t.Fatalf("resumed rewrap: %v", err)
	}
	if rewrapped != 1 {
		t.Fatalf("resumed rewrap = %d, want 1", rewrapped)
	}
	counts, err := restarted.Registry().WrappedCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts[active.ID] != 0 || counts[rotated.ID] != 2 {
		t.Fatalf("wrapped counts after resume = %v", counts)
	}
	if rewrapped, err := restarted.DEKs().RewrapAll(ctx); err != nil || rewrapped != 0 {
		t.Fatalf("second rewrap = %d, %v; want 0, nil", rewrapped, err)
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
	if _, err := store.secrets.Registry().Unwrap(ctx, "kek-missing", "test/v1", []byte("x")); !isUnwrapReason(t, err, secretkeys.UnwrapReasonUnknownKey) {
		t.Fatalf("unknown key unwrap = %v", err)
	}
	// Corrupt wrapped bytes fail authentication.
	active, err := store.secrets.Registry().ActiveKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := []byte{0x01, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}
	if _, err := store.secrets.Registry().Unwrap(ctx, active.ID, "test/v1", corrupt); !isUnwrapReason(t, err, secretkeys.UnwrapReasonCorruptCiphertext) {
		t.Fatalf("corrupt unwrap = %v", err)
	}
	// Missing provider material (a keyring file never provisioned against
	// the same shared database) is distinguished from corruption.
	keyring, err := secretkeys.NewKeyring(filepath.Join(t.TempDir(), "keys.json"), secretkeys.KeyringOptions{})
	if err != nil {
		t.Fatal(err)
	}
	orphaned := secretkeys.New(store.db, keyring)
	dekID := dekIDForScope(t, store, ctx, environmentID)
	var wrapped []byte
	if err := store.db.QueryRowContext(ctx, `SELECT wrapped_dek FROM envelope_data_keys WHERE id = $1`, dekID).Scan(&wrapped); err != nil {
		t.Fatal(err)
	}
	if _, err := orphaned.Registry().Unwrap(ctx, active.ID, secretkeys.DEKWrapPurpose(dekID), wrapped); !isUnwrapReason(t, err, secretkeys.UnwrapReasonMissingMaterial) {
		t.Fatalf("orphaned unwrap = %v", err)
	}
}

func TestSecretEnvelopeInconsistentKeyring(t *testing.T) {
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
	if _, err := testDelivery(store).SealServiceSecret(ctx, testUser("owner"), service.ID, "TOKEN", []byte("consistent-secret")); err != nil {
		t.Fatal(err)
	}
	active, err := store.secrets.Registry().ActiveKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Replica B provisions the SAME version ID with DIFFERENT random
	// material: a split-brain keyring the operator must fix, never silently
	// accept.
	keyringB, err := secretkeys.NewKeyring(filepath.Join(t.TempDir(), "keys.json"), secretkeys.KeyringOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyringB.GenerateKey(ctx, active.ProviderRef); err != nil {
		t.Fatal(err)
	}
	replicaB := secretkeys.New(store.db, keyringB)
	// Presence checks pass (the version exists) but every unwrap fails
	// authentication: wrong material is corruption, not a fallback.
	if err := replicaB.Registry().VerifyLocalCoverage(ctx); err != nil {
		t.Fatalf("coverage with wrong material: %v", err)
	}
	if _, _, err := replicaB.Sealed().OpenCurrent(ctx, store.db, service.ID, "TOKEN"); !isUnwrapReason(t, err, secretkeys.UnwrapReasonCorruptCiphertext) {
		t.Fatalf("inconsistent open = %v", err)
	}
	if _, err := replicaB.DEKs().VerifyAll(ctx); !isUnwrapReason(t, err, secretkeys.UnwrapReasonCorruptCiphertext) {
		t.Fatalf("inconsistent verify = %v", err)
	}
	// Activate a new version through the healthy replica: replica B's
	// rewrap then fails unwrapping the still-old row with its wrong
	// material instead of migrating anything.
	healthyVersion := provisionVersion(t, store.secrets)
	if _, err := store.secrets.Registry().Activate(ctx, healthyVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := replicaB.DEKs().RewrapAll(ctx); !isUnwrapReason(t, err, secretkeys.UnwrapReasonCorruptCiphertext) {
		t.Fatalf("inconsistent rewrap = %v", err)
	}
	// The healthy replica is unaffected.
	if _, err := store.secrets.DEKs().VerifyAll(ctx); err != nil {
		t.Fatalf("healthy verify: %v", err)
	}
}

func TestSecretEnvelopeCoverageFailsClosed(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if !store.secrets.Ready(ctx) {
		t.Fatal("healthy replica is not ready")
	}
	keyring, err := secretkeys.NewKeyring(filepath.Join(t.TempDir(), "keys.json"), secretkeys.KeyringOptions{})
	if err != nil {
		t.Fatal(err)
	}
	orphaned := secretkeys.New(store.db, keyring)
	if err := orphaned.Registry().VerifyLocalCoverage(ctx); !errors.Is(err, secretkeys.ErrProviderKeyNotFound) {
		t.Fatalf("orphaned coverage = %v", err)
	}
	if orphaned.Ready(ctx) {
		t.Fatal("replica without key material reports ready")
	}
}

func TestSecretEnvelopeActivateValidation(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.secrets.Registry().Activate(ctx, ""); err == nil {
		t.Fatal("empty activation was accepted")
	}
	// A version never provisioned to this replica cannot activate.
	if _, err := store.secrets.Registry().Activate(ctx, "v-never-provisioned"); !errors.Is(err, secretkeys.ErrProviderKeyNotFound) {
		t.Fatalf("unprovisioned activation = %v", err)
	}
	// Each version activates once.
	version := provisionVersion(t, store.secrets)
	if _, err := store.secrets.Registry().Activate(ctx, version); err != nil {
		t.Fatal(err)
	}
	if _, err := store.secrets.Registry().Activate(ctx, version); !errors.Is(err, secretkeys.ErrKeyVersionExists) {
		t.Fatalf("duplicate activation = %v", err)
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
	version := provisionVersion(t, store.secrets)
	if _, err := store.secrets.Registry().Activate(ctx, version); err != nil {
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

// provisionVersion mints a keyring version through the service's own
// keyring, as `controlplane keys provision` would before activation.
func provisionVersion(t *testing.T, svc *secretkeys.Service) string {
	t.Helper()
	version, err := svc.Provider().GenerateKey(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return version
}
