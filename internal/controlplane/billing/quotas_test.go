package billing

import "testing"

// The trial plan is the free tier offered at open signup. These values are
// the product's free caps; changing one is a pricing decision, so the test
// pins them exactly.
func TestTrialPlanIsTheFreeTier(t *testing.T) {
	quota := Plans[PlanTrial].Quota
	checks := []struct {
		name string
		got  int64
		want int64
	}{
		{"projects per workspace", quota.MaxProjects, 1},
		{"services per project", quota.MaxServicesPerProject, 3},
		{"services per environment", quota.MaxServicesPerEnvironment, 3},
		{"CPU millis per service", quota.MaxCPUMillisPerService, 1_000},
		{"memory MiB per service", quota.MaxMemoryMebibytesPerService, 512},
		{"replicas per service", quota.MaxReplicasPerService, 1},
		{"workspace CPU millis", quota.MaxCPUMillis, 3_000},
		{"workspace memory MiB", quota.MaxMemoryMebibytes, 1_536},
		{"volume bytes", quota.MaxVolumeBytes, 512 << 20},
		{"free workspaces per user", TrialWorkspacesPerUser, 1},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Errorf("trial %s = %d, want %d", check.name, check.got, check.want)
		}
	}
	if Plans[PlanTrial].MonthlyMinimumCents != 0 {
		t.Errorf("trial monthly minimum = %d, want 0 (free)", Plans[PlanTrial].MonthlyMinimumCents)
	}
}
