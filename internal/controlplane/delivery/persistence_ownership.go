package delivery

import (
	"context"
	"database/sql"
	"time"
)

const (
	allocationIntentRun   = "run"
	allocationIntentDrain = "drain"
)

func (s *persistence) setAgentAdministrationTx(ctx context.Context, tx *sql.Tx, administration AgentAdministration) error {
	_, err := tx.ExecContext(ctx, `UPDATE agent_administration
		SET lifecycle_state = $2, operator_intent = $3, maintenance_message = $4,
		    credential_revoked_at = $5, updated_at = $6
		WHERE agent_id = $1`, administration.AgentID, administration.LifecycleState, administration.OperatorIntent,
		administration.MaintenanceMessage, administration.CredentialRevokedAt, administration.UpdatedAt)
	return err
}

func (s *persistence) setAgentMaintenanceMessageTx(ctx context.Context, tx *sql.Tx, agentID, message string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE agent_administration SET maintenance_message = $2, updated_at = $3
		WHERE agent_id = $1 AND lifecycle_state = 'draining'`, agentID, message, now)
	return err
}

type allocationAssignmentState struct {
	AllocationID  string
	RolloutState  string
	Intent        string
	IntentMessage string
	DrainStarted  sql.NullTime
	DrainDeadline sql.NullTime
	UpdatedAt     time.Time
}

func (s *persistence) setAllocationStateTx(ctx context.Context, tx *sql.Tx, state allocationAssignmentState) error {
	_, err := tx.ExecContext(ctx, `UPDATE allocation_assignments
		SET rollout_state = $2, intent = $3, intent_message = $4,
		    drain_started_at = $5, drain_deadline = $6, updated_at = $7
		WHERE id = $1`, state.AllocationID, state.RolloutState, state.Intent, state.IntentMessage,
		state.DrainStarted, state.DrainDeadline, state.UpdatedAt)
	return err
}

func (s *persistence) insertAllocationAssignmentTx(ctx context.Context, tx *sql.Tx, assignment AllocationAssignment) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO allocation_assignments(
		id, service_id, deployment_id, agent_id, desired_spec_revision,
		desired_rollout_generation, allocation_ipv4, allocation_ipv6,
		operator_restart_nonce, rollout_state, intent, intent_message,
		drain_started_at, drain_deadline, created_at, updated_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		assignment.ID, assignment.ServiceID, assignment.DeploymentID, assignment.AgentID,
		assignment.SpecRevision, assignment.RolloutGeneration, assignment.IPv4, assignment.IPv6,
		assignment.OperatorRestartNonce, assignment.RolloutState, assignment.Intent, assignment.IntentMessage,
		assignment.DrainStartedAt, assignment.DrainDeadline, assignment.CreatedAt, assignment.UpdatedAt)
	return err
}

func (s *persistence) deleteAllocationAssignmentTx(ctx context.Context, tx *sql.Tx, allocationID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM allocation_assignments WHERE id = $1`, allocationID)
	return err
}

func (s *persistence) markAssignmentLostTx(ctx context.Context, tx *sql.Tx, allocationID, message string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE allocation_assignments
		SET rollout_state = $2, intent = $3, intent_message = $4,
		    allocation_ipv4 = '', allocation_ipv6 = '', updated_at = $5
		WHERE id = $1`, allocationID, AllocationRolloutLost, allocationIntentRun, message, now)
	return err
}

func (s *persistence) setAssignmentMessageTx(ctx context.Context, tx *sql.Tx, allocationID, message string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE allocation_assignments SET intent_message = $2, updated_at = $3 WHERE id = $1`, allocationID, message, now)
	return err
}
