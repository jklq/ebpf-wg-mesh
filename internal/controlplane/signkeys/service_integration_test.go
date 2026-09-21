//go:build integration

package signkeys

import (
	"bytes"
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/testdb"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// openTestService builds a Service over an isolated test database carrying
// the envelope and signing-key tables plus one active dev envelope key.
func openTestService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	testServer, err := testdb.Start("")
	if err != nil {
		t.Fatalf("start test database: %v", err)
	}
	t.Cleanup(func() { testServer.Stop() })
	pgURL := testdb.NormalizeURL(testServer.PGURL())
	db, err := sql.Open("pgx", pgURL.String())
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	name := fmt.Sprintf("signkeys_%d", time.Now().UnixNano())
	if _, err := db.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	scopedURL := *pgURL
	scopedURL.Path = "/" + name
	scoped, err := sql.Open("pgx", scopedURL.String())
	if err != nil {
		t.Fatalf("open scoped database: %v", err)
	}
	t.Cleanup(func() { scoped.Close() })
	for _, stmt := range []string{
		`CREATE TABLE envelope_keys (
			id STRING PRIMARY KEY,
			provider STRING NOT NULL,
			provider_ref STRING NOT NULL,
			state STRING NOT NULL CHECK (state IN ('active', 'retired')),
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE UNIQUE INDEX idx_envelope_keys_single_active ON envelope_keys(state) WHERE state = 'active'`,
		`CREATE TABLE envelope_data_keys (
			id STRING PRIMARY KEY,
			scope_kind STRING NOT NULL,
			scope_id STRING NOT NULL,
			wrapping_key_id STRING NOT NULL REFERENCES envelope_keys(id),
			wrapped_dek BYTES NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			UNIQUE (scope_kind, scope_id)
		)`,
		`CREATE INDEX idx_envelope_data_keys_wrapping ON envelope_data_keys(wrapping_key_id)`,
		`CREATE TABLE platform_signing_keys (
			id STRING PRIMARY KEY,
			scope STRING NOT NULL CHECK (scope IN ('internal-ca', 'registry', 'user-assertion', 'dashboard-session')),
			kid STRING NOT NULL UNIQUE,
			state STRING NOT NULL CHECK (state IN ('active', 'retiring')),
			key_type STRING NOT NULL CHECK (key_type IN ('ecdsa-p256', 'hmac-256')),
			wrapping_key_id STRING NOT NULL REFERENCES envelope_keys(id),
			wrapped_key BYTES NOT NULL,
			public_pem STRING NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			retired_at TIMESTAMPTZ NULL
		)`,
		`CREATE UNIQUE INDEX idx_platform_signing_keys_scope_state ON platform_signing_keys(scope, state)`,
		`CREATE INDEX idx_platform_signing_keys_wrapping ON platform_signing_keys(wrapping_key_id)`,
	} {
		if _, err := scoped.Exec(stmt); err != nil {
			t.Fatalf("create test schema: %v", err)
		}
	}
	ctx := context.Background()
	provider, err := secretkeys.NewKeyring(t.TempDir()+"/keys.json", secretkeys.KeyringOptions{AllowGenerate: true})
	if err != nil {
		t.Fatalf("open test keyring: %v", err)
	}
	t.Cleanup(func() { provider.Close() })
	registry := secretkeys.NewRegistry(scoped, provider)
	if _, err := registry.EnsureActiveKey(ctx); err != nil {
		t.Fatalf("ensure envelope key: %v", err)
	}
	return New(scoped, registry), scoped
}

func TestInitAndReadPaths(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()

	rec, err := svc.Init(ctx, ScopeUserAssertion, RotateOptions{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if rec.State != KeyStateActive || rec.KeyType != KeyTypeHMAC256 || rec.KID == "" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if _, err := svc.Init(ctx, ScopeUserAssertion, RotateOptions{}); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second Init = %v, want ErrAlreadyInitialized", err)
	}

	mat, err := svc.Active(ctx, ScopeUserAssertion)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(mat.Private) == 0 {
		t.Fatal("active HMAC material is empty")
	}
	secret, err := svc.ActiveSecret(ctx, ScopeUserAssertion)
	if err != nil {
		t.Fatalf("ActiveSecret: %v", err)
	}
	if string(secret) != string(mat.Private) {
		t.Fatal("exported secret differs from active material")
	}
	if _, err := svc.ActiveSecret(ctx, ScopeInternalCA); !errors.Is(err, ErrNoActiveKey) {
		t.Fatalf("ActiveSecret(uninitialized) = %v, want ErrNoActiveKey", err)
	}

	ca, err := svc.Init(ctx, ScopeInternalCA, RotateOptions{})
	if err != nil {
		t.Fatalf("Init internal-ca: %v", err)
	}
	bundle, err := svc.PublicBundle(ctx, ScopeInternalCA)
	if err != nil {
		t.Fatalf("PublicBundle: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle) {
		t.Fatal("bundle holds no certificate")
	}
	verifying, err := svc.Verifying(ctx, ScopeInternalCA)
	if err != nil {
		t.Fatalf("Verifying: %v", err)
	}
	if len(verifying) != 1 || verifying[0].Record.ID != ca.ID || verifying[0].Cert == nil || verifying[0].Key == nil {
		t.Fatalf("unexpected verifying set: %+v", verifying)
	}
	if _, err := svc.PublicBundle(ctx, ScopeUserAssertion); err == nil {
		t.Fatal("PublicBundle(hmac) succeeded, want error")
	}
}

func TestRotateStartFinishOverlap(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()

	if _, err := svc.Init(ctx, ScopeRegistry, RotateOptions{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	before, err := svc.Active(ctx, ScopeRegistry)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	start := time.Now().UTC()
	active, retiring, err := svc.RotateStart(ctx, ScopeRegistry, RotateOptions{})
	if err != nil {
		t.Fatalf("RotateStart: %v", err)
	}
	if active.ID == retiring.ID || retiring.ID != before.Record.ID {
		t.Fatalf("rotation did not demote the active key: active=%s retiring=%s before=%s", active.ID, retiring.ID, before.Record.ID)
	}
	if retiring.State != KeyStateRetiring || retiring.RetiredAt == nil || retiring.RetiredAt.Before(start) {
		t.Fatalf("retiring record broke overlap bookkeeping: %+v", retiring)
	}
	if _, _, err := svc.RotateStart(ctx, ScopeRegistry, RotateOptions{}); !errors.Is(err, ErrRotationInProgress) {
		t.Fatalf("second RotateStart = %v, want ErrRotationInProgress", err)
	}

	mats, err := svc.Verifying(ctx, ScopeRegistry)
	if err != nil {
		t.Fatalf("Verifying: %v", err)
	}
	if len(mats) != 2 || mats[0].Record.ID != active.ID || mats[1].Record.ID != retiring.ID {
		t.Fatal("verifying set is not active-first through the overlap")
	}
	bundle, err := svc.PublicBundle(ctx, ScopeRegistry)
	if err != nil {
		t.Fatalf("PublicBundle: %v", err)
	}
	first, second := splitTestBundle(t, bundle)
	// Trust anchors rotate deterministically: the old certificate stays
	// last so verifiers keep finding it, then drops at finish.
	if string(first) != string(active.PublicPEM) || string(second) != string(retiring.PublicPEM) {
		t.Fatal("overlap bundle is not active-first")
	}

	// The floor holds: finish before the overlap elapsed is refused.
	if _, err := svc.RotateFinish(ctx, ScopeRegistry, FinishOptions{}); !errors.Is(err, ErrOverlapNotElapsed) {
		t.Fatalf("early RotateFinish = %v, want ErrOverlapNotElapsed", err)
	}
	// A larger operator-requested overlap raises the bar.
	floor, err := MinOverlapForScope(ScopeRegistry)
	if err != nil {
		t.Fatalf("MinOverlapForScope: %v", err)
	}
	if _, err := svc.RotateFinish(ctx, ScopeRegistry, FinishOptions{Now: start.Add(floor), MinOverlap: floor + time.Hour}); !errors.Is(err, ErrOverlapNotElapsed) {
		t.Fatalf("raised-overlap RotateFinish = %v, want ErrOverlapNotElapsed", err)
	}
	deleted, err := svc.RotateFinish(ctx, ScopeRegistry, FinishOptions{Now: start.Add(floor).Add(time.Minute)})
	if err != nil {
		t.Fatalf("RotateFinish: %v", err)
	}
	if deleted.ID != retiring.ID {
		t.Fatalf("finish deleted %s, want retiring %s", deleted.ID, retiring.ID)
	}
	mats, err = svc.Verifying(ctx, ScopeRegistry)
	if err != nil {
		t.Fatalf("Verifying: %v", err)
	}
	if len(mats) != 1 || mats[0].Record.ID != active.ID {
		t.Fatalf("post-finish verifying set: %+v", mats)
	}
	if _, err := svc.RotateFinish(ctx, ScopeRegistry, FinishOptions{}); !errors.Is(err, ErrNoRotationInProgress) {
		t.Fatalf("second RotateFinish = %v, want ErrNoRotationInProgress", err)
	}
}

func TestRotateFinishForce(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()
	if _, err := svc.Init(ctx, ScopeUserAssertion, RotateOptions{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, _, err := svc.RotateStart(ctx, ScopeUserAssertion, RotateOptions{}); err != nil {
		t.Fatalf("RotateStart: %v", err)
	}
	if _, err := svc.RotateFinish(ctx, ScopeUserAssertion, FinishOptions{Force: true}); err != nil {
		t.Fatalf("forced RotateFinish: %v", err)
	}
}

func TestOperatorSuppliedHMACSecret(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()
	const supplied = "operator-supplied-secret-at-least-32-bytes!"
	if _, err := svc.Init(ctx, ScopeUserAssertion, RotateOptions{HMACSecret: []byte("short")}); err == nil {
		t.Fatal("short HMAC secret was accepted")
	}
	if _, err := svc.Init(ctx, ScopeUserAssertion, RotateOptions{HMACSecret: []byte(supplied)}); err != nil {
		t.Fatalf("Init with supplied secret: %v", err)
	}
	got, err := svc.ActiveSecret(ctx, ScopeUserAssertion)
	if err != nil {
		t.Fatalf("ActiveSecret: %v", err)
	}
	if string(got) != supplied {
		t.Fatalf("active secret %q, want supplied value", got)
	}
	if _, _, err := svc.RotateStart(ctx, ScopeInternalCA, RotateOptions{HMACSecret: []byte(supplied)}); err == nil {
		t.Fatal("HMAC secret was accepted for an ECDSA scope")
	}
}

func TestConcurrentInitAndRotateConverge(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()

	const racers = 8
	var wg sync.WaitGroup
	errs := make(chan error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.EnsureActiveKey(ctx, ScopeDashboardSession, EnsureOptions{})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("EnsureActiveKey: %v", err)
		}
	}
	records, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	active := 0
	for _, rec := range records {
		if rec.Scope == ScopeDashboardSession && rec.State == KeyStateActive {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("%d active dashboard-session keys, want 1", active)
	}

	startErrs := make(chan error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.RotateStart(ctx, ScopeDashboardSession, RotateOptions{})
			startErrs <- err
		}()
	}
	wg.Wait()
	close(startErrs)
	started := 0
	for err := range startErrs {
		if err == nil {
			started++
		} else if !errors.Is(err, ErrRotationInProgress) && !errors.Is(err, ErrConcurrentRotation) {
			t.Fatalf("RotateStart: %v", err)
		}
	}
	if started != 1 {
		t.Fatalf("%d rotations started, want 1", started)
	}
}

func TestRewrapAndVerifyAndCounts(t *testing.T) {
	svc, db := openTestService(t)
	ctx := context.Background()
	for _, scope := range AllScopes() {
		if _, err := svc.Init(ctx, scope, RotateOptions{}); err != nil {
			t.Fatalf("Init %s: %v", scope, err)
		}
	}
	var wrapping string
	if err := db.QueryRowContext(ctx, `SELECT wrapping_key_id FROM platform_signing_keys LIMIT 1`).Scan(&wrapping); err != nil {
		t.Fatalf("wrapping key: %v", err)
	}
	counts, err := svc.WrappingCounts(ctx)
	if err != nil {
		t.Fatalf("WrappingCounts: %v", err)
	}
	if counts[wrapping] != int64(len(AllScopes())) {
		t.Fatalf("wrapping counts: %+v", counts)
	}
	if verified, err := svc.VerifyAll(ctx); err != nil || verified != len(AllScopes()) {
		t.Fatalf("VerifyAll = %d, %v", verified, err)
	}
	if _, err := svc.Active(ctx, ScopeInternalCA); err != nil {
		t.Fatalf("Active: %v", err)
	}
}

func TestListAndReady(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()
	if svc.Ready(ctx, []string{ScopeInternalCA}) {
		t.Fatal("Ready with no keys")
	}
	for _, scope := range AllScopes() {
		if _, err := svc.Init(ctx, scope, RotateOptions{}); err != nil {
			t.Fatalf("Init %s: %v", scope, err)
		}
	}
	if !svc.Ready(ctx, AllScopes()) {
		t.Fatal("not Ready after init")
	}
	records, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != len(AllScopes()) {
		t.Fatalf("List holds %d records, want %d", len(records), len(AllScopes()))
	}
	if _, _, err := svc.RotateStart(ctx, ScopeInternalCA, RotateOptions{}); err != nil {
		t.Fatalf("RotateStart: %v", err)
	}
	records, err = svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != len(AllScopes())+1 {
		t.Fatalf("List holds %d records through rotation, want %d", len(records), len(AllScopes())+1)
	}
}

func splitTestBundle(t *testing.T, bundle []byte) ([]byte, []byte) {
	t.Helper()
	var blocks [][]byte
	rest := bundle
	for len(bytes.TrimSpace(rest)) > 0 {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			t.Fatal("decode trust bundle")
		}
		blocks = append(blocks, pem.EncodeToMemory(block))
	}
	if len(blocks) != 2 {
		t.Fatalf("bundle holds %d certificates, want 2", len(blocks))
	}
	return blocks[0], blocks[1]
}
