package deploy

import (
	"bytes"
	"context"
	"errors"
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
	if contract, ok := ContractForHook(op.Hook); ok {
		if contract.Kind == DatabaseVerification {
			db := DatabaseStatus{Replicated: true}
			for _, placement := range p.Placements {
				if placement.Role == Database {
					db.Members = append(db.Members, placement.Host)
					db.Live = append(db.Live, placement.Host)
				}
			}
			e.Database = &db
		}
		if contract.Kind == StorageVerification {
			e.Storage = snapshot(d.inv.Storage)
			for _, name := range p.Installation.Components[Database].Storage {
				e.Storage[name] = StorageStatus{Hosts: p.Installation.Storage[name].Hosts, Verified: true}
			}
		}
		e.Recovery = &RecoveryReceipt{Installation: p.Installation.ID, Generation: p.Generation, Checks: map[string]bool{}}
		for _, name := range contract.Checks {
			e.Recovery.Checks[name] = true
		}
	}
	if restorationPlan(p) {
		e.Recovery = &RecoveryReceipt{Installation: p.Installation.ID, Generation: p.Generation, Checks: map[string]bool{}, Acknowledgements: map[string]CheckpointAcknowledgement{}}
		contract, _ := ContractForHook(op.Hook)
		for _, name := range contract.Checks {
			e.Recovery.Checks[name] = true
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
func completeEvidence(installation, location string, t time.Time) Evidence {
	until := t.Add(recovery.Retention)
	object := recovery.Object{Key: "objects/test", Version: "protected", Digest: recovery.Digest([]byte("test")), Size: 4, RetainUntil: until}
	point := recovery.Point{Version: 1, Installation: installation, Snapshot: recovery.Snapshot{Timestamp: t, Schema: 43, ConsoleSchema: 3, Identities: map[string][]string{"agent_registrations": {}, "ingress_nodes": {}, "platform_signing_keys": {}}}, Database: recovery.Database{Collection: "external://recovery", Subdirectory: "full", Layers: []recovery.Layer{{End: t}}, Objects: []recovery.Object{object}}, CompletedAt: t, ExpiresAt: until}
	for _, kind := range []string{"keyring", "console-key", "external-secret", "installation", "deployment-state", "release", "tool"} {
		point.Snapshot.Requirements = append(point.Snapshot.Requirements, recovery.Requirement{Kind: kind, ID: "required"})
		point.Dependencies = append(point.Dependencies, recovery.Dependency{Kind: kind, ID: "required", Objects: []recovery.Object{object}})
	}
	point.Installer = append([]recovery.Requirement{}, point.Snapshot.Requirements...)
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

type unprovenPauseDriver struct{ Driver }

func (d unprovenPauseDriver) Observe(ctx context.Context, p Plan, state State, op Operation) (bool, Evidence, error) {
	done, evidence, err := d.Driver.Observe(ctx, p, state, op)
	if op.Hook == "quiesce" {
		evidence.Recovery = nil
	}
	return done, evidence, err
}

func TestInterruptedResumeRequiresDeclaredPauseProof(t *testing.T) {
	i, r, inv := fixture(3)
	p := build(t, i, r, State{}, inv, false)
	d := fake(inv)
	engine := Engine{Driver: unprovenPauseDriver{Driver: d}}
	if err := engine.pauseBeforeRetry(context.Background(), p, State{}); err == nil || !strings.Contains(err.Error(), "verification contract") {
		t.Fatalf("pause attempt accepted without its required proof: %v", err)
	}
	if !d.paused {
		t.Fatal("pause did not execute before its proof was rejected")
	}
}
