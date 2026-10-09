package deploy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ebof-wg-mesh/internal/recovery"
)

func TestLoadGeneratedRecoveryJSON(t *testing.T) {
	i, release, inv := fixture(3)
	plan := build(t, i, release, State{}, inv, false)
	now := time.Now().UTC()
	progress := RecoveryProgress{
		Generation: "recovery", StartedAt: now, ElapsedSeconds: 1.25,
		Release: release.ID, Fenced: true, Verified: true,
		ApprovedDigest: "approved", ReservedDigest: "reserved", ReportDigest: "report",
		NewRecoveryPoint: &Evidence{Backup: "protected-point", DataLossCutoff: now},
		Report:           &recovery.FleetReport{CapturedAt: now, ElapsedSeconds: 1.25, AdmittedAgents: []string{"agent-a"}, Reservations: []recovery.NetworkReservation{{EnvironmentID: "unknown-environment", Identity: 999999}}},
		Acknowledgements: map[string]CheckpointAcknowledgement{"agent-a": {Generation: "recovery", AuthorityEpoch: 2, Cursor: 3, Complete: true}},
		CompletedAt:      now,
	}
	dir := t.TempDir()
	write := func(name string, value any) string {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	loadedProgress, err := Load[RecoveryProgress](write("progress.json", progress))
	if err != nil || Digest(loadedProgress) != Digest(progress) {
		t.Fatal("generated recovery progress did not round trip", err)
	}
	loadedPlan, err := Load[Plan](write("plan.json", plan))
	if err != nil || Digest(loadedPlan) != Digest(plan) {
		t.Fatal("generated deployment plan did not round trip", err)
	}
	if err := loadedPlan.Validate(); err != nil {
		t.Fatal("loaded plan lost its declared dependencies", err)
	}
	for _, data := range []string{`{"generation":"recovery","unknown":true}`, `{"report":{"unknown":true}}`, "{\"generation\":\"recovery\"}\n{\"generation\":\"other\"}"} {
		path := filepath.Join(dir, "invalid.json")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load[RecoveryProgress](path); err == nil {
			t.Fatal("unknown fields or a second document were accepted")
		}
	}
}
