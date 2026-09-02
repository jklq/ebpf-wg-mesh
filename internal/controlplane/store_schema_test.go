package controlplane

import (
	"strings"
	"testing"
)

func TestSchemaVersionsKeepRollingActionsFleetAndIsolation(t *testing.T) {
	if currentSchemaVersion != 8 {
		t.Fatalf("current schema version = %d, want 8", currentSchemaVersion)
	}
	rolling := strings.Join(schemaUpgrades[4], "\n")
	actions := strings.Join(schemaUpgrades[5], "\n")
	fleet := strings.Join(schemaUpgrades[6], "\n")
	isolation := strings.Join(schemaUpgrades[7], "\n")
	railwayDefaults := strings.Join(schemaUpgrades[8], "\n")
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
	for _, marker := range []string{"lifecycle_state", "platform_operators", "agent_certificates", "failure_domain"} {
		if !strings.Contains(fleet, marker) {
			t.Fatalf("schema v6 lost fleet marker %q", marker)
		}
	}
	for _, marker := range []string{"sandbox_profile_audit_events", "sandboxProfile"} {
		if !strings.Contains(isolation, marker) {
			t.Fatalf("schema v7 lost isolation marker %q", marker)
		}
	}
	for _, marker := range []string{"service_revisions", "deployments", "sandboxProfile", "DROP TABLE sandbox_profile_audit_events"} {
		if !strings.Contains(railwayDefaults, marker) {
			t.Fatalf("schema v8 lost Railway-default marker %q", marker)
		}
	}
	if strings.Contains(rolling, "deployment_actions") {
		t.Fatal("schema v4 reuses the rolling replacement version for deployment actions")
	}
	if strings.Contains(actions, "lifecycle_state") {
		t.Fatal("schema v5 reuses the deployment-actions version for fleet lifecycle")
	}
	if strings.Contains(fleet, "sandbox_profile_audit_events") {
		t.Fatal("schema v6 reuses the fleet version for workload isolation")
	}
	if strings.Contains(rolling, "sandbox_profile_audit_events") || strings.Contains(actions, "sandbox_profile_audit_events") {
		t.Fatal("schema v4/v5 reuses an earlier version for workload isolation")
	}
	if strings.Contains(isolation, "DROP TABLE sandbox_profile_audit_events") {
		t.Fatal("schema v7 reuses the workload-isolation version for the Railway-default cutover")
	}
}
