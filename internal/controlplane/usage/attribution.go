package usage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Execer is the transaction surface attribution capture needs.
type Execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

const (
	KindAllocation = "allocation"
	KindVolume     = "volume"
)

// RecordAllocation captures the attribution of a newly assigned allocation.
// Call it in the transaction that inserts the assignment, so a producer can
// only report an allocation the control plane placed on its agent.
func RecordAllocation(ctx context.Context, tx Execer, allocationID, serviceID, deploymentID, agentID string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_attributions(
			resource_kind, resource_id, agent_id, project_id, environment_id, service_id,
			deployment_id, environment_name, resource_name, created_at)
		SELECT 'allocation', $1, $2, e.project_id, e.id, s.id, $3, e.name, s.name, $4
		  FROM services s JOIN environments e ON e.id = s.environment_id
		 WHERE s.id = $5
		ON CONFLICT (resource_kind, resource_id) DO NOTHING`,
		allocationID, agentID, deploymentID, at.UTC(), serviceID)
	if err != nil {
		return fmt.Errorf("record allocation usage attribution: %w", err)
	}
	return nil
}

// EndAllocation records when an allocation left desired state. The agent can
// still report its final intervals afterwards.
func EndAllocation(ctx context.Context, tx Execer, allocationID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE usage_attributions SET ended_at = statement_timestamp()
		WHERE resource_kind = 'allocation' AND resource_id = $1 AND ended_at IS NULL`, allocationID)
	return err
}

// RecordVolume captures the attribution of a volume when it is pinned to an
// agent. A volume never moves after pinning.
func RecordVolume(ctx context.Context, tx Execer, volumeID, agentID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_attributions(
			resource_kind, resource_id, agent_id, project_id, environment_id,
			environment_name, resource_name, created_at)
		SELECT 'volume', v.id, $2, e.project_id, e.id, e.name, v.name, statement_timestamp()
		  FROM volumes v JOIN environments e ON e.id = v.environment_id
		 WHERE v.id = $1
		ON CONFLICT (resource_kind, resource_id) DO NOTHING`, volumeID, agentID)
	if err != nil {
		return fmt.Errorf("record volume usage attribution: %w", err)
	}
	return nil
}

// CaptureVolumeDeletion keeps the logical deletion time of volumes whose
// catalog rows are about to be collected. Their data stays on the agent
// until destruction completes, and usage until then is retained storage.
func CaptureVolumeDeletion(ctx context.Context, tx Execer, volumeIDs []string) error {
	if len(volumeIDs) == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE usage_attributions a
		   SET deleted_at = COALESCE(a.deleted_at, d.deleted_at)
		  FROM (SELECT v.id, LEAST(v.deleted_at, e.deleted_at, p.deleted_at) AS deleted_at
		          FROM volumes v
		          JOIN environments e ON e.id = v.environment_id
		          JOIN projects p ON p.id = e.project_id
		         WHERE v.id = ANY($1)) d
		 WHERE a.resource_kind = 'volume' AND a.resource_id = d.id AND d.deleted_at IS NOT NULL`, volumeIDs)
	return err
}

// EndVolume records that an agent confirmed a volume's data destroyed.
func EndVolume(ctx context.Context, tx Execer, volumeID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE usage_attributions SET ended_at = statement_timestamp()
		WHERE resource_kind = 'volume' AND resource_id = $1 AND ended_at IS NULL`, volumeID)
	return err
}
