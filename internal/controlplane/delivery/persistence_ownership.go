package delivery

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type allocationObservationTarget struct {
	desiredSpecRevision, desiredGeneration               int64
	assignedIPv4, assignedIPv6, environmentID, serviceID string
	hasDomain                                            bool
	previousAppliedSpec, previousAppliedGeneration       sql.NullInt64
	previousPhase, previousMessage                       sql.NullString
	previousHealthy                                      sql.NullBool
	previousIPv4Ports, previousIPv6Ports                 []int32
	previousRestartRaw                                   []byte
}

const (
	allocationIntentRun   = "run"
	allocationIntentDrain = "drain"
)

// beginAgentSessionTx is the only persistence operation that lets a live
// agent replace presence. Administrative state is touched only to complete
// the operator-created enrollment.
func (s *persistence) beginAgentSessionTx(ctx context.Context, tx *sql.Tx, presence AgentPresence) error {
	presence.AgentID = strings.TrimSpace(presence.AgentID)
	presence.SessionID = strings.TrimSpace(presence.SessionID)
	if presence.SessionID == "" {
		return fmt.Errorf("session_id is required")
	}
	administration, err := tx.ExecContext(ctx, `UPDATE agent_administration
		SET lifecycle_state = CASE WHEN lifecycle_state = 'enrolling' THEN 'active' ELSE lifecycle_state END,
		    updated_at = CASE WHEN lifecycle_state = 'enrolling' THEN $2 ELSE updated_at END
		WHERE agent_id = $1 AND lifecycle_state <> 'retired' AND credential_revoked_at IS NULL`, presence.AgentID, presence.UpdatedAt)
	if err != nil {
		return err
	}
	rows, err := administration.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrAgentCredentialRevoked
	}
	_, err = tx.ExecContext(ctx, `UPSERT INTO agent_presence(
		agent_id, session_id, last_observation_sequence, last_contact_at, ready, reachable, updated_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7)`, presence.AgentID, presence.SessionID,
		int64(presence.LastObservationSequence), presence.LastContactAt, presence.Ready, presence.Reachable, presence.UpdatedAt)
	return err
}

func (s *persistence) recordAgentContactTx(ctx context.Context, tx *sql.Tx, agentID, sessionID string, now time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE agent_presence
		SET last_contact_at = $3, ready = TRUE, reachable = TRUE, updated_at = $3
		WHERE agent_id = $1 AND session_id = $2`, agentID, strings.TrimSpace(sessionID), now)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrStaleAgentSession
	}
	return nil
}

func (s *persistence) endAgentSessionTx(ctx context.Context, tx *sql.Tx, agentID, sessionID string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE agent_presence
		SET ready = FALSE, reachable = FALSE, updated_at = $3
		WHERE agent_id = $1 AND session_id = $2`, agentID, strings.TrimSpace(sessionID), now)
	return err
}

func (s *persistence) acceptAgentReportTx(ctx context.Context, tx *sql.Tx, agentID, sessionID string, sequence uint64, now time.Time) error {
	var current string
	var previousSequence int64
	if err := tx.QueryRowContext(ctx, `SELECT session_id, last_observation_sequence FROM agent_presence WHERE agent_id = $1 FOR UPDATE`, agentID).Scan(&current, &previousSequence); err != nil {
		return err
	}
	if strings.TrimSpace(sessionID) == "" || current != strings.TrimSpace(sessionID) {
		return ErrStaleAgentSession
	}
	if sequence <= uint64(previousSequence) {
		return fmt.Errorf("%w: report sequence %d follows %d", ErrStaleObservation, sequence, previousSequence)
	}
	_, err := tx.ExecContext(ctx, `UPDATE agent_presence
		SET last_observation_sequence = $3, last_contact_at = $4, ready = TRUE, reachable = TRUE, updated_at = $4
		WHERE agent_id = $1 AND session_id = $2`, agentID, sessionID, int64(sequence), now)
	return err
}

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

func (s *persistence) deleteStartingAssignmentsTx(ctx context.Context, tx *sql.Tx, serviceID string, generation *int64) error {
	query := `DELETE FROM allocation_assignments WHERE service_id = $1 AND rollout_state = $2`
	args := []any{serviceID, AllocationRolloutStarting}
	if generation != nil {
		query += ` AND desired_rollout_generation = $3`
		args = append(args, *generation)
	}
	_, err := tx.ExecContext(ctx, query, args...)
	return err
}

func (s *persistence) withdrawServiceAssignmentsTx(ctx context.Context, tx *sql.Tx, serviceID, message string, now time.Time) (int64, error) {
	result, err := tx.ExecContext(ctx, `UPDATE allocation_assignments
		SET rollout_state = $2, intent = $3, intent_message = $4, updated_at = $5
		WHERE service_id = $1 AND rollout_state NOT IN ($2, $6, $7)`,
		serviceID, AllocationRolloutWithdrawing, allocationIntentRun, message, now, AllocationRolloutDraining, AllocationRolloutLost)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *persistence) retargetAssignmentsTx(ctx context.Context, tx *sql.Tx, serviceID, agentID, deploymentID string, specRevision, generation int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE allocation_assignments
		SET deployment_id = $1, desired_spec_revision = $2, desired_rollout_generation = $3,
		    intent = $4, intent_message = '', updated_at = $5
		WHERE service_id = $6 AND agent_id = $7 AND rollout_state IN ($8, $9)`,
		deploymentID, specRevision, generation, allocationIntentRun, now, serviceID, agentID,
		AllocationRolloutStarting, AllocationRolloutServing)
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

func (s *persistence) allocationObservationTargetTx(ctx context.Context, tx *sql.Tx, allocationID, agentID string, generation int64) (allocationObservationTarget, error) {
	var target allocationObservationTarget
	err := tx.QueryRowContext(ctx, `SELECT a.desired_spec_revision, a.desired_rollout_generation,
		a.allocation_ipv4, a.allocation_ipv6,
		EXISTS(SELECT 1 FROM domain_bindings d WHERE d.service_id = a.service_id),
		s.environment_id, a.service_id,
		o.applied_spec_revision, o.applied_rollout_generation, o.phase, o.message, o.healthy,
		o.healthy_ipv4_ports, o.healthy_ipv6_ports, o.restart_observation_json
	FROM allocation_assignments a
	JOIN services s ON s.id = a.service_id
	LEFT JOIN allocation_observations o ON o.allocation_id = a.id AND o.rollout_generation = $3
	WHERE a.id = $1 AND a.agent_id = $2`, allocationID, agentID, generation).Scan(
		&target.desiredSpecRevision, &target.desiredGeneration, &target.assignedIPv4, &target.assignedIPv6,
		&target.hasDomain, &target.environmentID, &target.serviceID,
		&target.previousAppliedSpec, &target.previousAppliedGeneration, &target.previousPhase, &target.previousMessage, &target.previousHealthy,
		(*jsonInt32Slice)(&target.previousIPv4Ports), (*jsonInt32Slice)(&target.previousIPv6Ports), &target.previousRestartRaw,
	)
	return target, err
}

func (s *persistence) recordAllocationObservationTx(ctx context.Context, tx *sql.Tx, observation AllocationObservation) error {
	healthyIPv4Ports, err := encodeHealthyPorts(observation.HealthyIPv4Ports)
	if err != nil {
		return fmt.Errorf("encode healthy IPv4 ports: %w", err)
	}
	healthyIPv6Ports, err := encodeHealthyPorts(observation.HealthyIPv6Ports)
	if err != nil {
		return fmt.Errorf("encode healthy IPv6 ports: %w", err)
	}
	restartRaw, err := encodeRestartObservation(observation.Restart)
	if err != nil {
		return fmt.Errorf("encode restart observation: %w", err)
	}
	_, err = tx.ExecContext(ctx, `UPSERT INTO allocation_observations(
		allocation_id, rollout_generation, applied_spec_revision, applied_rollout_generation,
		phase, message, healthy_ipv4_ports, healthy_ipv6_ports, healthy,
		restart_observation_json, agent_id, session_id, observation_sequence, observed_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		observation.AllocationID, observation.RolloutGeneration, observation.AppliedSpecRevision, observation.AppliedGeneration,
		observation.Phase, observation.Message, healthyIPv4Ports, healthyIPv6Ports, observation.Healthy, restartRaw,
		observation.AgentID, observation.SessionID, int64(observation.Sequence), observation.ObservedAt)
	return err
}
