package productionops

import (
	"testing"

	"ebof-wg-mesh/internal/deploy"
)

func TestDatabaseRestartRequiresSurvivingRangeQuorumsAndFullHealth(t *testing.T) {
	healthy := deploy.DatabaseStatus{Members: []string{"a", "b", "c"}, Live: []string{"a", "b", "c"}, Replicated: true, Ranges: []deploy.RangeStatus{{ID: "1", Voters: []string{"a", "b", "c"}}}}
	if err := databaseRestartHealth(healthy, "a", false); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name              string
		alter             func(*deploy.DatabaseStatus)
		recovery, allowed bool
	}{
		{"underreplicated", func(s *deploy.DatabaseStatus) { s.Replicated = false }, false, false},
		{"other node lost", func(s *deploy.DatabaseStatus) { s.Live = []string{"a", "c"} }, false, false},
		{"range cannot survive", func(s *deploy.DatabaseStatus) { s.Ranges[0].Voters = []string{"a", "b"} }, false, false},
		{"current stopped node can recover", func(s *deploy.DatabaseStatus) { s.Live = []string{"b", "c"}; s.Replicated = false }, true, true},
		{"two stopped nodes cannot advance", func(s *deploy.DatabaseStatus) { s.Live = []string{"c"}; s.Replicated = false }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := healthy
			s.Ranges = append([]deploy.RangeStatus{}, healthy.Ranges...)
			tc.alter(&s)
			if err := databaseRestartHealth(s, "a", tc.recovery); (err == nil) != tc.allowed {
				t.Fatal(err)
			}
		})
	}
}

func TestDatabaseUpgradeRetryAfterFinalizationOnlyAdmitsNewBinaries(t *testing.T) {
	status := deploy.DatabaseStatus{Members: []string{"a", "b", "c"}, Versions: map[string]string{"a": "v26.1.0", "b": "v26.1.0", "c": "v26.1.0"}}
	if err := validateUpgradeClusterVersion("26.1", status, "v25.4.0", "v26.1.0"); err != nil {
		t.Fatal("finalized desired-version node cannot recover", err)
	}
	status.Versions["a"] = "v25.4.0"
	if err := validateUpgradeClusterVersion("26.1", status, "v25.4.0", "v26.1.0"); err == nil {
		t.Fatal("old binary entered a finalized cluster")
	}
	if err := validateUpgradeClusterVersion("25.4", status, "v25.4.0", "v26.1.0"); err != nil {
		t.Fatal("mixed binaries cannot complete the rolling upgrade", err)
	}
	if err := validateUpgradeClusterVersion("25.3", status, "v25.4.0", "v26.1.0"); err == nil {
		t.Fatal("unfinalized previous cluster admitted a major upgrade")
	}
}
