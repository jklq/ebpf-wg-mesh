//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestSchemaRejectsCrossServiceReferences(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	err := store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		statements := []string{
			`INSERT INTO projects(id, name, kind, owner_user_id, created_at) VALUES ('integrity-project', 'integrity', 'user', 'owner', $1)`,
			`INSERT INTO environments(id, project_id, name, kind, auto_deploy, network_identity, created_at, updated_at) VALUES ('integrity-environment', 'integrity-project', 'production', 'production', TRUE, 9001, $1, $1)`,
			`INSERT INTO services(id, environment_id, name, current_spec_revision, desired_replica_count, created_at, updated_at) VALUES ('service-a', 'integrity-environment', 'a', 1, 1, $1, $1), ('service-b', 'integrity-environment', 'b', 1, 1, $1, $1)`,
			`INSERT INTO service_delivery_status(service_id, updated_at) VALUES ('service-a', $1), ('service-b', $1)`,
			`INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at) VALUES ('service-a', 1, '{}', $1), ('service-b', 1, '{}', $1)`,
			`INSERT INTO deployments(id, service_id, spec_revision, state, cause_kind, reason_code, resolved_spec_json, variable_versions_json, sealed_versions_json, created_at, updated_at) VALUES ('deployment-a', 'service-a', 1, 'staged', 'system', 'TEST', '{}', '{}', '{}', $1, $1), ('deployment-b', 'service-b', 1, 'staged', 'system', 'TEST', '{}', '{}', '{}', $1, $1)`,
			`INSERT INTO agent_registrations(id, name, region, failure_domain, created_at, updated_at) VALUES ('integrity-agent', 'integrity-agent', 'test', 'integrity-agent', $1, $1)`,
			`INSERT INTO agent_administration(agent_id, lifecycle_state, updated_at) VALUES ('integrity-agent', 'ready', $1)`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.db.ExecContext(ctx, `INSERT INTO allocation_assignments(
		id, service_id, deployment_id, agent_id, desired_spec_revision, desired_rollout_generation,
		rollout_state, intent, created_at, updated_at
	) VALUES ('bad-allocation', 'service-a', 'deployment-b', 'integrity-agent', 1, 0, 'starting', 'run', $1, $1)`, now); err == nil {
		t.Fatal("cross-service allocation was accepted")
	}

	if _, err := store.db.ExecContext(ctx, `INSERT INTO deployment_actions(
		id, service_id, target_deployment_id, action, idempotency_key, requested_by_user_id, created_at
	) VALUES ('bad-action', 'service-a', 'deployment-b', 'restore', 'bad-action', 'owner', $1)`, now); err == nil {
		t.Fatal("cross-service deployment action was accepted")
	}
}
