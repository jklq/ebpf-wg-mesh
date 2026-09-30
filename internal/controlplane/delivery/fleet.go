package delivery

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"sort"
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
		_, err := journal.AgentRow(req.GetAgentId()).Exec(ctx, tx, `INSERT INTO agent_registrations(
			id, name, region, zone, failure_domain,
			reserved_cpu_millis, reserved_memory_mebibytes, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`,
			strings.TrimSpace(req.GetAgentId()), strings.TrimSpace(req.GetName()),
			strings.TrimSpace(req.GetRegion()), strings.TrimSpace(req.GetZone()), strings.TrimSpace(req.GetFailureDomain()),
			req.GetReservedCpuMillis(), req.GetReservedMemoryMebibytes(), now)
		if err != nil {
			return err
		}

		if _, err := journal.AdministrationRow(req.GetAgentId()).Exec(ctx, tx, `INSERT INTO agent_administration(agent_id, lifecycle_state, updated_at)
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
		_, err = journal.AgentRow(req.GetAgentId()).Exec(ctx, tx, `UPDATE agent_registrations SET name = $1, region = $2, zone = $3,
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

func (d *Delivery) SetAgentLifecycle(ctx context.Context, user authz.User, agentID string, target AgentLifecycleState) (AgentRecord, []string, error) {
	s := d.store
	if _, err := s.authz.AuthorizeOperator(ctx, user); err != nil {
		return AgentRecord{}, nil, err
	}
	if target != AgentStateActive && target != AgentStateCordoned && target != AgentStateDraining && target != AgentStateRetired {
		return AgentRecord{}, nil, fmt.Errorf("%w: operators may set active, cordoned, draining, or retired", ErrInvalidAgentTransition)
	}
	var rec AgentRecord
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := agentByIDQuerier(ctx, tx, agentID, false)
		if err != nil {
			return err
		}
		var administrationState AgentLifecycleState
		if err := tx.QueryRowContext(ctx, `SELECT lifecycle_state FROM agent_administration WHERE agent_id = $1 FOR UPDATE`, agentID).Scan(&administrationState); err != nil {
			return err
		}
		if err := validateAgentTransition(administrationState, target); err != nil {
			return err
		}
		now := time.Now().UTC()
		if target == AgentStateRetired {
			var allocations int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM allocations WHERE agent_id = $1 AND rollout_state <> $2`, agentID, AllocationRolloutLost).Scan(&allocations); err != nil {
				return err
			}
			if allocations != 0 {
				return fmt.Errorf("%w: %d allocations remain", ErrAgentHasAllocations, allocations)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE agent_bootstrap_tokens SET consumed_at = $1 WHERE agent_id = $2 AND consumed_at IS NULL`, now, agentID); err != nil {
				return err
			}
			if err := s.setAgentAdministrationTx(ctx, tx, AgentAdministration{
				AgentID: agentID, LifecycleState: AgentStateRetired, OperatorIntent: "retire",
				MaintenanceMessage:  "credentials revoked; mesh identity removed",
				CredentialRevokedAt: sql.NullTime{Time: now, Valid: true}, UpdatedAt: now,
			}); err != nil {
				return err
			}
			if _, err := journal.AgentRow(agentID).Exec(ctx, tx, `UPDATE agent_registrations SET advertise_addr = '',
				workload_ipv4_subnet = '', workload_ipv6_subnet = '', wireguard_public_key = '',
				wireguard_listen_port = 0, wireguard_endpoint = '', wireguard_ipv6 = '', updated_at = $1 WHERE id = $2`, now, agentID); err != nil {
				return err
			}
		} else {
			message := ""
			if target == AgentStateCordoned {
				message = "cordoned; existing allocations continue, new placement is disabled"
			} else if target == AgentStateDraining {
				message = "draining stateless allocations"
			}
			if err := s.setAgentAdministrationTx(ctx, tx, AgentAdministration{
				AgentID: agentID, LifecycleState: target, OperatorIntent: string(target),
				MaintenanceMessage: message, CredentialRevokedAt: current.CredentialRevokedAt, UpdatedAt: now,
			}); err != nil {
				return err
			}
		}
		rec, err = agentByIDQuerier(ctx, tx, agentID, false)
		return err
	})
	if err != nil {
		return AgentRecord{}, nil, err
	}
	var notify []string
	if target == AgentStateDraining {
		notify, err = d.reconcileDrainingAgent(ctx, agentID)
		if err != nil {
			return AgentRecord{}, nil, err
		}
		rec, err = s.agentByID(ctx, agentID)
	} else if target == AgentStateActive {
		if err = d.ReconcileFleetCapacity(ctx); err != nil {
			return AgentRecord{}, nil, err
		}
		notify, err = s.agentIDs(ctx)
	} else if target == AgentStateRetired {
		notify, err = s.activeAgentIDs(ctx)
	}
	return rec, notify, err
}

var (
	ErrAgentNotEnrolled       = errors.New("agent is not enrolled by an operator")
	ErrAgentCredentialRevoked = errors.New("agent credential is revoked")
	ErrAgentHasAllocations    = errors.New("agent still has allocations or attachments")
	ErrInvalidAgentTransition = errors.New("invalid agent lifecycle transition")
	ErrInvalidFleetAgentInput = errors.New("invalid fleet agent input")
	ErrStaleAgentSession      = errors.New("stale agent session")
	ErrStaleObservation       = errors.New("stale allocation observation")
	ErrAllocationOwnership    = errors.New("allocation is not assigned to authenticated agent")
	ErrNotLiveOwner           = errors.New("replica is not the live owner")
	fleetLabelPattern         = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62})$`)
)

const agentSelectSQL = `SELECT id, name, lifecycle_state, state_before_unavailable,
		region, zone, failure_domain, reserved_cpu_millis, reserved_memory_mebibytes,
		advertise_addr, workload_ipv4_subnet, workload_ipv6_subnet, wireguard_public_key, wireguard_listen_port,
		wireguard_endpoint, wireguard_ipv6, cpu_millis_capacity, memory_mebibytes_capacity,
		runtime_capabilities, software_version, maintenance_message,
		credential_revoked_at, last_seen_at
	FROM agents`

func scanAgentRecord(scanner interface{ Scan(...any) error }) (AgentRecord, error) {
	var rec AgentRecord
	var capabilities jsonStringSlice
	if err := scanner.Scan(
		&rec.ID, &rec.Name, &rec.LifecycleState, &rec.StateBeforeUnavailable,
		&rec.Region, &rec.Zone, &rec.FailureDomain, &rec.ReservedCPUMillis, &rec.ReservedMemoryMebibytes,
		&rec.AdvertiseAddr, &rec.WorkloadIPv4Subnet, &rec.WorkloadIPv6Subnet, &rec.WireGuardPublicKey, &rec.WireGuardListenPort,
		&rec.WireGuardEndpoint, &rec.WireGuardIPv6, &rec.CPUMillisCapacity, &rec.MemoryMebibytesCapcity,
		&capabilities, &rec.SoftwareVersion, &rec.MaintenanceMessage,
		&rec.CredentialRevokedAt, &rec.LastSeenAt,
	); err != nil {
		return AgentRecord{}, err
	}
	rec.RuntimeCapabilities = append([]string(nil), capabilities...)
	return rec, nil
}

func (s *persistence) overlayAgent(rec AgentRecord) AgentRecord {
	if s == nil || s.live == nil {
		return overlayAgentAbsent(rec)
	}
	return s.live.OverlayAgent(rec)
}

func (s *persistence) overlayAllocation(rec AllocationRecord) AllocationRecord {
	if s == nil || s.live == nil {
		return overlayAllocation(rec, AgentSession{}, false, AllocationObservation{}, false, time.Time{}, AgentHealthyTTL)
	}
	return s.live.OverlayAllocation(rec)
}

func agentByIDQuerier(ctx context.Context, q ServiceQueryer, agentID string, forUpdate bool) (AgentRecord, error) {
	query := agentSelectSQL + ` WHERE id = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanAgentRecord(q.QueryRowContext(ctx, query, strings.TrimSpace(agentID)))
}

func CanonicalCapabilities(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func ValidateFleetAgentInput(id, name, region, zone, failureDomain string, reservedCPU, reservedMemory int64) error {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if id == "" || len(id) > 128 {
		return fmt.Errorf("%w: agent_id is required and must not exceed 128 characters", ErrInvalidFleetAgentInput)
	}
	if name == "" || len(name) > 128 {
		return fmt.Errorf("%w: agent name is required and must not exceed 128 characters", ErrInvalidFleetAgentInput)
	}
	for field, value := range map[string]string{
		"region": region, "zone": zone, "failure_domain": failureDomain,
	} {
		value = strings.TrimSpace(value)
		if field == "zone" && value == "" {
			continue
		}
		if !fleetLabelPattern.MatchString(value) {
			return fmt.Errorf("%w: %s must be a lowercase operator label", ErrInvalidFleetAgentInput, field)
		}
	}
	if reservedCPU < 0 || reservedMemory < 0 {
		return fmt.Errorf("%w: resource reservations must not be negative", ErrInvalidFleetAgentInput)
	}
	return nil
}

func validateAgentTransition(current, target AgentLifecycleState) error {
	if current == target {
		return nil
	}
	if current == AgentStateRetired {
		return fmt.Errorf("%w: %s to %s", ErrInvalidAgentTransition, current, target)
	}
	if target == AgentStateRetired && (current == AgentStateEnrolling || current == AgentStateCordoned || current == AgentStateDraining || current == AgentStateUnavailable) {
		return nil
	}
	if current == AgentStateEnrolling || current == AgentStateUnavailable {
		return fmt.Errorf("%w: %s to %s", ErrInvalidAgentTransition, current, target)
	}
	switch target {
	case AgentStateActive:
		if current == AgentStateCordoned || current == AgentStateDraining {
			return nil
		}
	case AgentStateCordoned:
		if current == AgentStateActive || current == AgentStateDraining {
			return nil
		}
	case AgentStateDraining:
		if current == AgentStateActive || current == AgentStateCordoned {
			return nil
		}
	case AgentStateRetired:
		if current == AgentStateCordoned || current == AgentStateDraining {
			return nil
		}
	}
	return fmt.Errorf("%w: %s to %s", ErrInvalidAgentTransition, current, target)
}

func (s *persistence) activeAgentIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM agents WHERE lifecycle_state <> 'retired' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func newAgentBootstrapToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func insertAgentBootstrapTokenTx(ctx context.Context, tx *sql.Tx, agentID, token, origin string, now time.Time) error {
	if origin == "" {
		origin = "operator"
	}
	hash := BootstrapTokenHash(token)
	_, err := tx.ExecContext(ctx, `INSERT INTO agent_bootstrap_tokens(token_hash, agent_id, origin, created_at, consumed_at)
		VALUES ($1, $2, $3, $4, NULL)`, hash[:], strings.TrimSpace(agentID), origin, now)
	return err
}

func BootstrapTokenHash(token string) [sha256.Size]byte {
	return sha256.Sum256([]byte(strings.TrimSpace(token)))
}

func (s *persistence) setAgentAdministrationTx(ctx context.Context, tx *sql.Tx, administration AgentAdministration) error {
	if _, err := journal.AdministrationRow(administration.AgentID).Exec(ctx, tx, `UPDATE agent_administration
		SET lifecycle_state = $2, operator_intent = $3, maintenance_message = $4,
		    credential_revoked_at = $5, updated_at = $6
		WHERE agent_id = $1`, administration.AgentID, administration.LifecycleState, administration.OperatorIntent,
		administration.MaintenanceMessage, administration.CredentialRevokedAt, administration.UpdatedAt); err != nil {
		return err
	}
	return nil
}

func (s *persistence) setAgentMaintenanceMessageTx(ctx context.Context, tx *sql.Tx, agentID, message string, now time.Time) error {
	if _, err := journal.AdministrationRow(agentID).Exec(ctx, tx, `UPDATE agent_administration SET maintenance_message = $2, updated_at = $3
		WHERE agent_id = $1 AND lifecycle_state = 'draining'`, agentID, message, now); err != nil {
		return err
	}
	return nil
}
