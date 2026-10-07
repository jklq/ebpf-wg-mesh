package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/recovery"
)

type fakeDriver struct {
	failObserveHook    string
	paused             bool
	inv                Inventory
	done               map[string]bool
	count              map[string]int
	fail               string
	failHook           string
	unknownPurchase    bool
	purchaseDiscovered bool
}

func fake(inv Inventory) *fakeDriver {
	return &fakeDriver{inv: inv, done: map[string]bool{}, count: map[string]int{}}
}
func (d *fakeDriver) Inventory(context.Context, Installation, Release, State) (Inventory, error) {
	return d.inv, nil
}
func (d *fakeDriver) Observe(_ context.Context, p Plan, state State, op Operation) (bool, Evidence, error) {
	if d.failObserveHook != "" && d.failObserveHook == op.Hook && d.done[op.ID] {
		return false, Evidence{}, errors.New("live observation failed")
	}

	if op.Kind == "purchase" {
		return d.purchaseDiscovered, Evidence{}, nil
	}
	e := Evidence{}
	if op.Hook == "backup" {
		timestamp := testNow
		if restorationPlan(p) {
			timestamp = time.Now().UTC()
		}
		e = completeEvidence(p.Installation.ID, "s3://backups/production/points/production/complete?versionId=protected", timestamp)
	}
	if op.Hook == "recovery-verify" && state.LastRestore != nil {
		e = completeEvidence(p.Installation.ID, state.LastRestore.Backup, state.LastRestore.DataLossCutoff)
	}
	if restorationPlan(p) {
		e.Recovery = &RecoveryReceipt{Installation: p.Installation.ID, Generation: p.Generation, Checks: map[string]bool{}, Acknowledgements: map[string]CheckpointAcknowledgement{}}
		for _, check := range []string{"provider-or-host-fence", "prior-authority-disabled", "empty-destination", "schemas-not-initialized", "database-restored", "selected-release", "new-ca-without-overlap", "client-identities", "console-sessions-invalidated", "registry-authority", "all-participants-paused", "host-admin-admission", "network-reservations", "quarantine-preserved", "approved-desired-state", "complete-checkpoints", "stale-build-ownership-invalidated", "new-worker-leases", "external-effects-reconciled", "database-health", "key-access", "overlay-connectivity", "image-access", "ingress-acknowledgements", "certificate-trust", "console-login", "all-participants-resumed"} {
			e.Recovery.Checks[check] = true
		}
		if state.Recovery.Report != nil {
			e.Recovery.ReportDigest = state.Recovery.Report.ApprovalDigest()
		}
		input := recovery.FleetInput{CapturedAt: time.Now().UTC()}
		for _, pl := range p.Placements {
			if pl.Role == Agent {
				input.Observed = append(input.Observed, recovery.FleetHost{ID: pl.Instance, Reachable: true, AuthorityResolved: true, Generation: p.Generation})
				e.Recovery.Acknowledgements[pl.Instance] = CheckpointAcknowledgement{Generation: p.Generation, AuthorityEpoch: 1, Cursor: 0, Complete: true}
			}
		}
		e.Fleet = &input
	}
	if (op.Hook == "resume" || op.Hook == "recovery-resume") && d.paused {
		return false, e, nil
	}
	return d.done[op.ID], e, nil
}
func (d *fakeDriver) Execute(_ context.Context, _ Plan, _ State, op Operation) (Binding, error) {
	d.count[op.ID]++
	if op.Hook == "quiesce" {
		d.paused = true
	}
	if op.Hook == "resume" || op.Hook == "recovery-resume" {
		d.paused = false
	}
	if op.Kind == "purchase" {
		if d.unknownPurchase {
			return Binding{}, errors.New("connection lost after provider acceptance")
		}
		d.purchaseDiscovered = true
		return Binding{Provider: "hetzner", ServerID: "42"}, nil
	}
	if op.ID == d.fail || (d.failHook != "" && op.Hook == d.failHook) {
		return Binding{}, errors.New("interrupted SSH")
	}
	d.done[op.ID] = true
	return Binding{}, nil
}
func testStore(t *testing.T) (*Store, []byte, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.enc")
	key := bytes.Repeat([]byte{0x37}, 32)
	s, err := OpenState(path, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, key, path
}

func TestEncryptedStateAuthenticationAtomicityAndSingleWriter(t *testing.T) {
	s, key, path := testStore(t)
	i, r, _ := fixture(1)
	state := State{Version: 1, InstallationID: i.ID, Bindings: map[string]Binding{"a": {Provider: "hetzner", ServerID: "private-binding"}}, Policy: &i, Bundle: &r}
	if err := s.Write(state); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("private-binding")) || bytes.Contains(raw, []byte("production")) {
		t.Fatal("state contains plaintext")
	}
	if _, err := OpenState(path, key); err == nil {
		t.Fatal("concurrent writer admitted")
	}
	got, err := s.Read()
	if err != nil || Digest(state) != Digest(got) {
		t.Fatalf("round trip: %v", err)
	}
	first := append([]byte{}, raw...)
	if err := s.Write(state); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	if bytes.Equal(first, raw) {
		t.Fatal("nonce reused")
	}
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(); err == nil {
		t.Fatal("tampered state authenticated")
	}
}
func TestApplyResumesRecordedProgressAndIsRepeatable(t *testing.T) {
	i, r, inv := fixture(3)
	s, _, _ := testStore(t)
	initial, _ := s.Read()
	p := build(t, i, r, initial, inv, false)
	requireComplete(t, p)
	d := fake(inv)
	d.fail = p.Operations[3].ID
	e := Engine{Store: s, Driver: d}
	if err := e.Apply(context.Background(), p); err == nil {
		t.Fatal("interruption not surfaced")
	}
	partial, err := s.Read()
	if err != nil || partial.Progress == nil || partial.Progress.Plan == nil {
		t.Fatal("missing resumable plan", err)
	}
	if len(partial.Progress.Completed) != 3 {
		t.Fatal(partial.Progress.Completed)
	}
	d.fail = ""
	// A completed download can disappear before the operation resumes.
	delete(d.done, p.Operations[0].ID)
	if err := e.Apply(context.Background(), *partial.Progress.Plan); err != nil {
		t.Fatal(err)
	}
	if d.count[p.Operations[0].ID] != 2 || d.count[p.Operations[1].ID] != 1 || d.count[p.Operations[3].ID] != 2 {
		t.Fatal("lost artifacts were not restored, intact work repeated or failed work skipped")
	}
	state, _ := s.Read()
	if state.Progress != nil || state.Revision != 1 || len(state.Placements) != len(p.Placements) {
		t.Fatal(state)
	}
	next := build(t, i, r, state, inv, false)
	for _, op := range next.Operations {
		d.done[op.ID] = true
	}
	if err := e.Apply(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	final, _ := s.Read()
	if Digest(final.Placements) != Digest(state.Placements) {
		t.Fatal("repeat apply moved replicas")
	}
}
func TestApplyRejectsMateriallyStaleAndTamperedPlans(t *testing.T) {
	i, r, inv := fixture(1)
	s, _, _ := testStore(t)
	state, _ := s.Read()
	p := build(t, i, r, state, inv, false)
	d := fake(inv)
	e := Engine{Store: s, Driver: d}
	d.inv = snapshot(inv)
	status := d.inv.Hosts["a"]
	status.Capacity.MemoryMiB--
	d.inv.Hosts["a"] = status
	if err := e.Apply(context.Background(), p); err == nil {
		t.Fatal("changed inventory accepted")
	}
	d.inv = inv
	state.Revision++
	if err := s.Write(state); err != nil {
		t.Fatal(err)
	}
	if err := e.Apply(context.Background(), p); err == nil {
		t.Fatal("changed state accepted")
	}
	p.Placements[0].Host = "invented"
	if err := e.Apply(context.Background(), p); err == nil {
		t.Fatal("tampered plan accepted")
	}
}

func TestReturningHostRetiresEveryReplacedInstanceAndReleasesItsBudget(t *testing.T) {
	i, r, inv := fixture(3)
	state := applied(build(t, i, r, State{}, inv, false))
	store, _, _ := testStore(t)
	if err := store.Write(state); err != nil {
		t.Fatal(err)
	}
	status := inv.Hosts["a"]
	status.Online = false
	inv.Hosts["a"] = status
	driver := fake(inv)
	engine := Engine{Store: store, Driver: driver}
	plan := build(t, i, r, state, inv, true)
	if err := engine.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	state, err := store.Read()
	if err != nil || len(state.Retained) != 4 {
		t.Fatal("unavailable core instances lost their lifecycle records", state.Retained, err)
	}
	status.Online = true
	inv.Hosts["a"] = status
	driver.inv = inv
	plan = build(t, i, r, state, inv, true)
	expected := i.Hosts[0].Reserve
	for _, pl := range append(append([]Placement{}, plan.Placements...), state.Retained...) {
		if pl.Host == "a" {
			expected = expected.Add(i.Components[pl.Role].Resources)
		}
	}
	if plan.Reservations["a"] != expected {
		t.Fatal("returning instances consumed unreserved resources")
	}
	retirements := 0
	for _, op := range plan.Operations {
		if op.Kind == "retire" && op.Host == "a" {
			retirements++
		}
	}
	if retirements != 4 {
		t.Fatal("returning units did not receive native retirement", retirements)
	}
	if err := engine.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	state, err = store.Read()
	if err != nil || len(state.Retained) != 0 {
		t.Fatal("retired instances still retained", err)
	}
	next := build(t, i, r, state, inv, true)
	if next.Reservations["a"] != expected.Sub(i.Components[ControlPlane].Resources).Sub(i.Components[Console].Resources).Sub(i.Components[Envoy].Resources).Sub(i.Components[Registry].Resources) {
		t.Fatal("retired resources were not released")
	}
}
func TestUnknownPurchaseRequiresDiscoveryAndNeverDuplicates(t *testing.T) {
	i, r, inv := fixture(1)
	i.Hosts[0].Binding.ServerID = ""
	i.Hosts[0].Purchase = &Purchase{ServerType: "cx32", Image: "ubuntu-24.04", Location: "hel1", SSHKeys: []string{"deployment"}}
	inv.Hosts["a"] = HostStatus{}
	inv.Database = DatabaseStatus{}
	s, _, _ := testStore(t)
	state, _ := s.Read()
	p := build(t, i, r, state, inv, false)
	requireComplete(t, p)
	d := fake(inv)
	d.unknownPurchase = true
	e := Engine{Store: s, Driver: d}
	if err := e.Apply(context.Background(), p); err == nil {
		t.Fatal("unknown outcome ignored")
	}
	if err := e.Apply(context.Background(), p); err == nil {
		t.Fatal("unknown purchase repeated")
	}
	if d.count[p.Operations[0].ID] != 1 {
		t.Fatal("duplicate purchase")
	}
	d.purchaseDiscovered = true
	d.unknownPurchase = false
	status := d.inv.Hosts["a"]
	status.ServerID = "42"
	d.inv.Hosts["a"] = status
	if err := e.Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	state, _ = s.Read()
	if state.Bindings["a"].ServerID != "42" || d.count[p.Operations[0].ID] != 1 {
		t.Fatal("discovered purchase binding not retained")
	}
}
func TestReleaseCutoverOrderAndBackupCutoff(t *testing.T) {
	i, r, inv := fixture(3)
	p := build(t, i, r, State{}, inv, false)
	state := applied(p)
	r.ID = "r43"
	r.Schema = 44
	i.Release = r.ID
	r.Conversions = map[string]Hook{"r42": {Command: []string{"/opt/convert"}, Verify: []string{"/opt/verify-conversion"}}}
	p = build(t, i, r, state, inv, false)
	requireComplete(t, p)
	quiesce, backup, convert, firstInstall, lastStop, lastDatabase, protect, schedule := -1, -1, -1, -1, -1, -1, -1, -1
	for n, op := range p.Operations {
		if op.Hook == "recovery-protect" {
			protect = n
		}
		if op.Hook == "backup-schedule" {
			schedule = n
		}
		if op.Hook == "quiesce" {
			quiesce = n
		}
		if op.Hook == "backup" {
			backup = n
		}
		if op.Kind == "convert" {
			convert = n
		}
		if op.Kind == "stop" {
			lastStop = n
		}
		if (op.Kind == "install" || op.Kind == "database-join") && op.Placement.Role == Database {
			lastDatabase = n
		}
		if firstInstall < 0 && op.Kind == "install" && op.Placement.Role != Database {
			firstInstall = n
		}
	}
	if !(quiesce < backup && backup < lastStop && lastStop < lastDatabase && lastDatabase < convert && convert < firstInstall) {
		t.Fatalf("unsafe cutover: q=%d b=%d stop=%d database=%d convert=%d install=%d", quiesce, backup, lastStop, lastDatabase, convert, firstInstall)
	}
	if !(protect < quiesce && convert < schedule && schedule < firstInstall) {
		t.Fatal("activation/cutover bypassed recovery protection or native schedules")
	}
	s, _, _ := testStore(t)
	if err := s.Write(state); err != nil {
		t.Fatal(err)
	}
	d := fake(inv)
	if err := (Engine{Store: s, Driver: d}).Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	final, _ := s.Read()
	if final.LastBackup.DataLossCutoff != testNow || final.LastBackup.Backup == "" {
		t.Fatal("backup cutoff missing")
	}
	r.Conversions = nil
	if _, err := BuildPlan(i, r, state, inv, false, testNow); err == nil {
		t.Fatal("unimplemented conversion accepted")
	}
}

func TestFailedUpgradeVerificationAndInterruptedResumeRemainPaused(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprint(interrupted), func(t *testing.T) {
			i, r, inv := fixture(3)
			state := applied(build(t, i, r, State{}, inv, false))
			r.ID = "r43"
			i.Release = r.ID
			p := build(t, i, r, state, inv, false)
			store, _, _ := testStore(t)
			if err := store.Write(state); err != nil {
				t.Fatal(err)
			}
			d := fake(inv)
			e := Engine{Store: store, Driver: d}
			if interrupted {
				d.failHook = "resume"
			} else {
				d.failHook = "production-verify"
			}
			if err := e.Apply(context.Background(), p); err == nil {
				t.Fatal("failure ignored")
			}
			for _, op := range p.Operations {
				if op.Hook == "resume" && !interrupted && d.count[op.ID] != 0 {
					t.Fatal("resumed failed upgrade")
				}
				if op.Hook == "production-verify" {
					delete(d.done, op.ID)
				}
			}
			if !d.paused {
				t.Fatal("failed upgrade or partial resume did not preserve the pause")
			}
			d.failHook = "production-verify"
			if err := e.Apply(context.Background(), p); err == nil {
				t.Fatal("failed retry resumed")
			}
			if !d.paused {
				t.Fatal("failed retry left automation active")
			}
			partial, _ := store.Read()
			if partial.Progress == nil || partial.LastBackup.Backup == "" || partial.Bundle.ID != "r42" {
				t.Fatal("recovery options lost")
			}
			d.failHook = ""
			if err := e.Apply(context.Background(), p); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func completeEvidence(installation, location string, t time.Time) Evidence {
	until := t.Add(recovery.Retention)
	object := recovery.Object{Key: "objects/test", Version: "protected", Digest: recovery.Digest([]byte("test")), Size: 4, RetainUntil: until}
	point := recovery.Point{Version: 1, Installation: installation, Snapshot: recovery.Snapshot{Timestamp: t, Schema: 43, ConsoleSchema: 3, Identities: map[string][]string{"agent_registrations": {}, "ingress_nodes": {}, "platform_signing_keys": {}}}, Database: recovery.Database{Collection: "external://recovery", Subdirectory: "full", Layers: []recovery.Layer{{End: t}}, Objects: []recovery.Object{object}}, CompletedAt: t, ExpiresAt: until}
	for _, kind := range []string{"keyring", "console-key", "external-secret", "installation", "deployment-state", "release", "tool"} {
		point.Snapshot.Requirements = append(point.Snapshot.Requirements, recovery.Requirement{Kind: kind, ID: "required"})
		point.Dependencies = append(point.Dependencies, recovery.Dependency{Kind: kind, ID: "required", Objects: []recovery.Object{object}})
	}
	manifest := object
	u, _ := url.Parse(location)
	manifest.Key = strings.TrimPrefix(u.Path, "/")
	manifest.Version = u.Query().Get("versionId")
	manifest.Digest = point.ManifestDigest()
	return Evidence{Backup: location, DataLossCutoff: t, Point: &point, Object: &manifest}
}

func TestRecoveryEvidenceRejectsAssertionsAndUnprotectedSelection(t *testing.T) {
	location := "s3://backups/production/points/production/complete?versionId=protected"
	valid := func() Evidence { return completeEvidence("production", location, testNow) }
	if err := validateRecoveryEvidence(valid(), "production", "s3://backups/production", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, alter := range []func(*Evidence){
		func(e *Evidence) { e.Point = nil },
		func(e *Evidence) { e.Point.Dependencies = nil; e.Object.Digest = e.Point.ManifestDigest() },
		func(e *Evidence) { e.Backup = "s3://backups/production/points/production/complete" },
		func(e *Evidence) { e.Object.Version = "different" },
		func(e *Evidence) { e.Object.RetainUntil = testNow },
		func(e *Evidence) { e.DataLossCutoff = e.DataLossCutoff.Add(time.Second) },
	} {
		e := valid()
		alter(&e)
		if err := validateRecoveryEvidence(e, "production", "s3://backups/production", time.Now()); err == nil {
			t.Fatal("incomplete recovery evidence accepted", e)
		}
	}
}

func TestResumeObservationFailureRepausesBeforeRetry(t *testing.T) {
	i, r, inv := fixture(3)
	state := applied(build(t, i, r, State{}, inv, false))
	r.ID = "r43"
	i.Release = r.ID
	p := build(t, i, r, state, inv, false)
	store, _, _ := testStore(t)
	if err := store.Write(state); err != nil {
		t.Fatal(err)
	}
	driver := fake(inv)
	driver.failObserveHook = "resume"
	engine := Engine{Store: store, Driver: driver}
	if err := engine.Apply(context.Background(), p); err == nil {
		t.Fatal("unverified resume accepted")
	}
	if !driver.paused {
		t.Fatal("failed live resume observation left automation running")
	}
	actual, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if actual.Progress == nil || actual.LastBackup.Backup == "" || actual.Bundle.ID != "r42" {
		t.Fatal("failed activation lost recovery options")
	}
	driver.failObserveHook = ""
	if err := engine.Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if driver.paused {
		t.Fatal("verified retry did not activate")
	}
}
