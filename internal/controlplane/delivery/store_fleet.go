package delivery

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

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
	FleetLabelPattern         = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62})$`)
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
		if !FleetLabelPattern.MatchString(value) {
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
