package deploy

import (
	"context"
	"testing"
	"time"

	"ebof-wg-mesh/internal/recovery"
)

func TestRecoveryResumeCannotExecuteBeforeVerification(t *testing.T) {
	i, r, inv := fixture(1)
	p, state, err := newRestorePlan(i, r, State{}, inv, Evidence{Backup: "selected", DataLossCutoff: time.Now().Add(-time.Hour)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range p.Operations {
		if op.Hook == "recovery-resume" {
			p.Operations = []Operation{op}
			break
		}
	}
	p.ID = p.digest()
	state.Progress.Plan = &p
	state.Progress.PlanID = p.ID
	store, _, _ := testStore(t)
	if err := store.Write(state); err != nil {
		t.Fatal(err)
	}
	driver := fake(inv)
	if err := (Engine{Store: store, Driver: driver}).Apply(context.Background(), p); err == nil {
		t.Fatal("unverified recovery resumed")
	}
	if driver.count[p.Operations[0].ID] != 0 {
		t.Fatal("resume hook executed before verification gate")
	}
}

func TestRecoveryApprovalBindsReportAndRefreshInvalidatesApproval(t *testing.T) {
	i, r, inv := fixture(1)
	p, state, err := newRestorePlan(i, r, State{}, inv, Evidence{Backup: "selected", DataLossCutoff: time.Now().Add(-time.Hour)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	report, err := recovery.CompareFleet(recovery.FleetInput{CapturedAt: time.Now()}, i.ID, p.Generation, r.ID, state.LastRestore.DataLossCutoff, state.Recovery.StartedAt, time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	state.Recovery.Report = &report
	state.Recovery.ReservedDigest = report.ApprovalDigest()
	store, _, _ := testStore(t)
	if err := store.Write(state); err != nil {
		t.Fatal(err)
	}
	engine := Engine{Store: store}
	if err := engine.ApproveRecovery("different-report"); err == nil {
		t.Fatal("operator approved a different report")
	}
	if err := engine.ApproveRecovery(report.ApprovalDigest()); err != nil {
		t.Fatal(err)
	}
	if err := engine.RefreshRecoveryInventory(); err != nil {
		t.Fatal(err)
	}
	state, _ = store.Read()
	if state.Recovery.ApprovedDigest != "" || state.Recovery.Report != nil || state.Recovery.Generation != p.Generation {
		t.Fatal("refresh retained approval or changed generation")
	}
}

func TestResumeRequiresCheckpointsFromEveryAdmittedAgentAndANewCompletePoint(t *testing.T) {
	i, release, inv := fixture(1)
	now := time.Now().UTC()
	p, state, err := newRestorePlan(i, release, State{}, inv, Evidence{Backup: "selected", DataLossCutoff: now.Add(-time.Hour)}, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	report, err := recovery.CompareFleet(recovery.FleetInput{CapturedAt: now}, i.ID, p.Generation, release.ID, state.LastRestore.DataLossCutoff, state.Recovery.StartedAt, now, true)
	if err != nil {
		t.Fatal(err)
	}
	report.AdmittedAgents = []string{"surviving-agent"}
	r := state.Recovery
	r.Fenced, r.Verified, r.Report = true, true, &report
	r.ApprovedDigest, r.ReservedDigest = report.ApprovalDigest(), report.ApprovalDigest()
	point := completeEvidence(i.ID, "s3://backups/production/points/production/complete?versionId=protected", now)
	r.NewRecoveryPoint = &point
	r.Acknowledgements = map[string]CheckpointAcknowledgement{}
	for _, pl := range p.Placements {
		if pl.Role == Agent {
			r.Acknowledgements[pl.Instance] = CheckpointAcknowledgement{Generation: p.Generation, AuthorityEpoch: 1, Complete: true}
		}
	}
	if err := validateRecoveryResume(p, r, now); err == nil {
		t.Fatal("resume ignored a surviving agent outside target placements")
	}
	r.Acknowledgements["surviving-agent"] = CheckpointAcknowledgement{Generation: "before", AuthorityEpoch: 900, Complete: true}
	if err := validateRecoveryResume(p, r, now); err == nil {
		t.Fatal("old-generation checkpoint authorized resume")
	}
	r.Acknowledgements["surviving-agent"] = CheckpointAcknowledgement{Generation: p.Generation, AuthorityEpoch: 1, Complete: true}
	if err := validateRecoveryResume(p, r, now); err != nil {
		t.Fatal("fully verified operation could not resume", err)
	}
	r.NewRecoveryPoint = nil
	if err := validateRecoveryResume(p, r, now); err == nil {
		t.Fatal("resume accepted a SQL backup without a new complete point")
	}
}
