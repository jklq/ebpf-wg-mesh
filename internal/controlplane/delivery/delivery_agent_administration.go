package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func (d *Delivery) CreateFleetAgent(ctx context.Context, req *platformv1.CreateAgentRequest) (AgentRecord, string, error) {
	identity, err := d.userFromContext(ctx)
	if err != nil {
		return AgentRecord{}, "", err
	}
	return d.createFleetAgent(ctx, identity.UserID, req)
}

func (d *Delivery) createFleetAgent(ctx context.Context, userID string, req *platformv1.CreateAgentRequest) (AgentRecord, string, error) {
	s := d.store
	if err := s.authorizeOperator(ctx, userID); err != nil {
		return AgentRecord{}, "", err
	}
	if err := ValidateFleetAgentInput(req.GetAgentId(), req.GetName(), req.GetRegion(), req.GetZone(), req.GetFailureDomain(), req.GetReservedCpuMillis(), req.GetReservedMemoryMebibytes()); err != nil {
		return AgentRecord{}, "", err
	}
	token, err := newAgentBootstrapToken()
	if err != nil {
		return AgentRecord{}, "", err
	}
	var rec AgentRecord
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		_, err := tx.ExecContext(ctx, `INSERT INTO agent_registrations(
			id, name, region, zone, failure_domain,
			reserved_cpu_millis, reserved_memory_mebibytes, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`,
			strings.TrimSpace(req.GetAgentId()), strings.TrimSpace(req.GetName()),
			strings.TrimSpace(req.GetRegion()), strings.TrimSpace(req.GetZone()), strings.TrimSpace(req.GetFailureDomain()),
			req.GetReservedCpuMillis(), req.GetReservedMemoryMebibytes(), now)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_administration(agent_id, lifecycle_state, updated_at)
			VALUES ($1, 'enrolling', $2)`, req.GetAgentId(), now); err != nil {
			return err
		}
		if err := insertAgentBootstrapTokenTx(ctx, tx, req.GetAgentId(), token, "operator", now); err != nil {
			return err
		}
		rec, err = agentByIDQuerier(ctx, tx, req.GetAgentId(), false)
		return err
	})
	return rec, token, err
}

func (d *Delivery) UpdateFleetAgent(ctx context.Context, req *platformv1.UpdateAgentRequest) (AgentRecord, error) {
	identity, err := d.userFromContext(ctx)
	if err != nil {
		return AgentRecord{}, err
	}
	return d.updateFleetAgent(ctx, identity.UserID, req)
}

func (d *Delivery) updateFleetAgent(ctx context.Context, userID string, req *platformv1.UpdateAgentRequest) (AgentRecord, error) {
	s := d.store
	if err := s.authorizeOperator(ctx, userID); err != nil {
		return AgentRecord{}, err
	}
	if err := ValidateFleetAgentInput(req.GetAgentId(), req.GetName(), req.GetRegion(), req.GetZone(), req.GetFailureDomain(), req.GetReservedCpuMillis(), req.GetReservedMemoryMebibytes()); err != nil {
		return AgentRecord{}, err
	}
	var rec AgentRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var locked string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM agent_registrations WHERE id = $1 FOR UPDATE`, req.GetAgentId()).Scan(&locked); err != nil {
			return err
		}
		current, err := agentByIDQuerier(ctx, tx, locked, false)
		if err != nil {
			return err
		}
		if current.LifecycleState == AgentStateRetired {
			return fmt.Errorf("%w: retired agents are immutable", ErrInvalidAgentTransition)
		}
		if req.GetReservedCpuMillis() > current.CPUMillisCapacity && current.CPUMillisCapacity > 0 {
			return errors.New("reserved CPU exceeds observed node capacity")
		}
		if req.GetReservedMemoryMebibytes() > current.MemoryMebibytesCapcity && current.MemoryMebibytesCapcity > 0 {
			return errors.New("reserved memory exceeds observed node capacity")
		}
		_, err = tx.ExecContext(ctx, `UPDATE agent_registrations SET name = $1, region = $2, zone = $3,
			failure_domain = $4, reserved_cpu_millis = $5, reserved_memory_mebibytes = $6,
			updated_at = $7 WHERE id = $8`, strings.TrimSpace(req.GetName()), strings.TrimSpace(req.GetRegion()),
			strings.TrimSpace(req.GetZone()), strings.TrimSpace(req.GetFailureDomain()), req.GetReservedCpuMillis(),
			req.GetReservedMemoryMebibytes(), time.Now().UTC(), req.GetAgentId())
		if err != nil {
			return err
		}
		rec, err = agentByIDQuerier(ctx, tx, req.GetAgentId(), false)
		return err
	})
	return rec, err
}
