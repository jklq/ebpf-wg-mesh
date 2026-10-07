//go:build integration

package recovery_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys"
	"ebof-wg-mesh/internal/recovery"
)

func TestRecoveryReplacesTrustWithoutOverlapAndProtectsUnreachableRanges(t *testing.T) {
	ctx := context.Background()
	db, stop := restoreTestDB(t, t.TempDir())
	defer stop()
	if err := recovery.CheckEmptyDestination(ctx, db); err != nil {
		t.Fatal("fresh database is not an empty recovery destination", err)
	}
	keyring := filepath.Join(t.TempDir(), "keys.json")
	if err := controlplane.BootstrapInstallation(ctx, db, keyring); err != nil {
		t.Fatal(err)
	}
	if err := recovery.CheckEmptyDestination(ctx, db); err == nil {
		t.Fatal("platform-initialized destination accepted for full-cluster restore")
	}
	for _, statement := range []string{
		`INSERT INTO agent_registrations(id,name,local_store_id,region,failure_domain,workload_ipv4_subnet,workload_ipv6_subnet,wireguard_ipv6,created_at,updated_at) VALUES ('survivor','survivor','store','region','domain','10.0.0.0/24','fd00:1::/64','fd00:44::11/64',now(),now())`,
		`INSERT INTO projects(id,name,kind,owner_user_id,created_at) VALUES ('project','project','user','operator',now())`,
		`INSERT INTO environments(id,project_id,name,kind,auto_deploy,network_identity,created_at,updated_at) VALUES ('environment','project','environment','user',false,1,now(),now())`,
		`INSERT INTO services(id,environment_id,name,current_spec_revision,created_at,updated_at) VALUES ('service','environment','service',1,now(),now())`,
		`INSERT INTO allocation_assignments(id,service_id,deployment_id,agent_id,desired_spec_revision,desired_rollout_generation,allocation_ipv4,allocation_ipv6,rollout_state,intent,created_at,updated_at) VALUES ('matching','service','deployment','survivor',1,2,'10.0.0.2','fd00:1::2','serving','run',now(),now())`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	desired, err := recovery.ReadDesiredFleet(ctx, db)
	if err != nil || len(desired) != 1 || len(desired[0].Allocations) != 1 {
		t.Fatal("could not read restored SQL desired state", desired, err)
	}
	if desired[0].Allocations[0].DeploymentID != "deployment" || desired[0].Allocations[0].RolloutGeneration != 2 || !slices.ContainsFunc(desired[0].Reservations, func(r recovery.NetworkReservation) bool { return r.Prefix == "fd00:44::11/128" }) {
		t.Fatal("desired inventory lost allocation or WireGuard identity", desired)
	}
	resources, err := recovery.ReadDesiredResources(ctx, db)
	if err != nil || len(resources) != 1 || resources[0].Kind != "service" {
		t.Fatal("could not read restored desired resources", resources, err)
	}
	keys, err := secretkeys.Open(ctx, db, config.SecretKeysConfig{KeyringPath: keyring}, secretkeys.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Provider().Close()
	signing := signkeys.New(db, keys.Registry())
	before, err := signing.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.ControlPlaneConfig{StateDir: t.TempDir(), InternalGRPC: config.ListenerConfig{TLS: config.ServerTLSConfig{ServerNames: []string{"controlplane"}, ServerCertValidityHours: 24, ClientCertValidityHours: 24}}}
	authority, err := identity.NewTLSAuthority(ctx, cfg, signing, identity.NewSharedCertificateRevocations(db))
	if err != nil {
		t.Fatal(err)
	}
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "survivor"}}, private)
	if err != nil {
		t.Fatal(err)
	}
	old, err := authority.Enroll(ctx, &agentv1.EnrollRequest{AgentId: "survivor", CsrPem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))})
	if err != nil {
		t.Fatal(err)
	}
	if err := signing.ResetForRecovery(ctx, "installation", "generation"); err != nil {
		t.Fatal(err)
	}
	after, err := signing.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatal("recovery retained overlapping signing keys")
	}
	for _, a := range after {
		for _, b := range before {
			if a.ID == b.ID {
				t.Fatal("recovery reused old authority")
			}
		}
	}
	if err := signing.ResetForRecovery(ctx, "installation", "generation"); err != nil {
		t.Fatal("interrupted generation could not resume", err)
	}
	repeated, _ := signing.List(ctx)
	for i := range after {
		if after[i].ID != repeated[i].ID {
			t.Fatal("repetition replaced recovery authority")
		}
	}
	fresh, err := identity.NewTLSAuthority(ctx, cfg, signing, identity.NewSharedCertificateRevocations(db))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := fresh.TrustBundle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(bundle)
	block, _ := pem.Decode([]byte(old.GetCertPem()))
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
		t.Fatal("old authority client can reconnect after recovery")
	}
	report := recovery.FleetReport{Installation: "installation", Generation: "generation", Reservations: []recovery.NetworkReservation{{Owner: "unreachable", Prefix: "10.8.0.0/24"}, {Owner: "unreachable", Prefix: "fd00:8::/64"}, {Owner: "newer", Identity: 700, EnvironmentID: "after-backup"}}}
	if err := recovery.ReserveFleet(ctx, db, report); err != nil {
		t.Fatal(err)
	}
	if err := recovery.ReserveFleet(ctx, db, report); err != nil {
		t.Fatal("reservation repetition failed", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, prefix := range []string{"10.8.0.2/32", "10.8.0.0/24", "fd00:8::2/128"} {
		if free, err := recovery.RecoveryPrefixAvailable(ctx, tx, netip.MustParsePrefix(prefix), "fresh-host"); err != nil || free {
			t.Fatal("unreachable host range reused", prefix, free, err)
		}
	}
	if free, err := recovery.RecoveryPrefixAvailable(ctx, tx, netip.MustParsePrefix("10.9.0.0/24"), "fresh-host"); err != nil || !free {
		t.Fatal("free range unavailable", free, err)
	}
	var next int64
	if err := tx.QueryRowContext(ctx, `SELECT next_identity FROM environment_network_identity_counter WHERE id=TRUE`).Scan(&next); err != nil || next != 701 {
		t.Fatal("observed network identity can be reused", next, err)
	}
}
