package controlplane

import (
	"strings"
	"testing"
)

func TestDeploymentActionsUseSchemaVersionAfterRollingReplacement(t *testing.T) {
	if currentSchemaVersion != 5 {
		t.Fatalf("current schema version = %d, want 5", currentSchemaVersion)
	}
	rolling := strings.Join(schemaUpgrades[4], "\n")
	actions := strings.Join(schemaUpgrades[5], "\n")
	for _, marker := range []string{"rollout_state", "drain_deadline", "strategy_json"} {
		if !strings.Contains(rolling, marker) {
			t.Fatalf("schema v4 lost rolling replacement marker %q", marker)
		}
	}
	for _, marker := range []string{"target_allocation_id", "resolved_spec_json", "variable_versions_json", "deployment_actions"} {
		if !strings.Contains(actions, marker) {
			t.Fatalf("schema v5 lost deployment action marker %q", marker)
		}
	}
	if strings.Contains(rolling, "deployment_actions") {
		t.Fatal("schema v4 reuses the rolling replacement version for deployment actions")
	}
}
