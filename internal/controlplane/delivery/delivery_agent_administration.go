package delivery

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/journal"
)

func (d *Delivery) CreateFleetAgent(ctx context.Context, user authz.User, req *platformv1.CreateAgentRequest) (AgentRecord, string, error) {
	scope, err := d.store.authz.AuthorizeOperator(ctx, user)
	if err != nil {
		return AgentRecord{}, "", err
	}
	return d.createFleetAgent(ctx, scope, req)
}

func (d *Delivery) createFleetAgent(ctx context.Context, _ authz.Operator, req *platformv1.CreateAgentRequest) (AgentRecord, string, error) {
	s := d.store
	if err := ValidateFleetAgentInput(req.GetAgentId(), req.GetName(), req.GetRegion(), req.GetZone(), req.GetFailureDomain(), req.GetReservedCpuMillis(), req.GetReservedMemoryMebibytes()); err != nil {
		return AgentRecord{}, "", err
	}
	token, err := newAgentBootstrapToken()
	if err != nil {
		return AgentRecord{}, "", err
	}
	var rec AgentRecord
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
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
		journal.RecordAgent(ctx, req.GetAgentId())
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_administration(agent_id, lifecycle_state, updated_at)
			VALUES ($1, 'enrolling', $2)`, req.GetAgentId(), now); err != nil {
			return err
		}
		journal.RecordAdministration(ctx, req.GetAgentId())
		if err := insertAgentBootstrapTokenTx(ctx, tx, req.GetAgentId(), token, "operator", now); err != nil {
			return err
		}
		rec, err = agentByIDQuerier(ctx, tx, req.GetAgentId(), false)
		return err
	})
	return rec, token, err
}

func (d *Delivery) UpdateFleetAgent(ctx context.Context, user authz.User, req *platformv1.UpdateAgentRequest) (AgentRecord, error) {
	scope, err := d.store.authz.AuthorizeOperator(ctx, user)
	if err != nil {
		return AgentRecord{}, err
	}
	return d.updateFleetAgent(ctx, scope, req)
}

func (d *Delivery) updateFleetAgent(ctx context.Context, _ authz.Operator, req *platformv1.UpdateAgentRequest) (AgentRecord, error) {
	s := d.store
	if err := ValidateFleetAgentInput(req.GetAgentId(), req.GetName(), req.GetRegion(), req.GetZone(), req.GetFailureDomain(), req.GetReservedCpuMillis(), req.GetReservedMemoryMebibytes()); err != nil {
		return AgentRecord{}, err
	}
	var rec AgentRecord
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
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
			return fmt.Errorf("%w: reserved CPU exceeds observed node capacity", ErrInvalidFleetAgentInput)
		}
		if req.GetReservedMemoryMebibytes() > current.MemoryMebibytesCapcity && current.MemoryMebibytesCapcity > 0 {
			return fmt.Errorf("%w: reserved memory exceeds observed node capacity", ErrInvalidFleetAgentInput)
		}
		_, err = tx.ExecContext(ctx, `UPDATE agent_registrations SET name = $1, region = $2, zone = $3,
			failure_domain = $4, reserved_cpu_millis = $5, reserved_memory_mebibytes = $6,
			updated_at = $7 WHERE id = $8`, strings.TrimSpace(req.GetName()), strings.TrimSpace(req.GetRegion()),
			strings.TrimSpace(req.GetZone()), strings.TrimSpace(req.GetFailureDomain()), req.GetReservedCpuMillis(),
			req.GetReservedMemoryMebibytes(), time.Now().UTC(), req.GetAgentId())
		if err != nil {
			return err
		}
		journal.RecordAgent(ctx, req.GetAgentId())
		rec, err = agentByIDQuerier(ctx, tx, req.GetAgentId(), false)
		return err
	})
	return rec, err
}
