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
	FleetLabelPattern         = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62})$`)
)

const agentSelectSQL = `SELECT id, name, lifecycle_state, state_before_unavailable,
		region, zone, failure_domain, reserved_cpu_millis, reserved_memory_mebibytes,
		advertise_addr, workload_ipv4_subnet, workload_ipv6_subnet, wireguard_public_key, wireguard_listen_port,
		wireguard_ipv6, cpu_millis_capacity, memory_mebibytes_capacity,
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
		&rec.WireGuardIPv6, &rec.CPUMillisCapacity, &rec.MemoryMebibytesCapcity,
		&capabilities, &rec.SoftwareVersion, &rec.MaintenanceMessage,
		&rec.CredentialRevokedAt, &rec.LastSeenAt,
	); err != nil {
		return AgentRecord{}, err
	}
	rec.RuntimeCapabilities = append([]string(nil), capabilities...)
	return rec, nil
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
		return errors.New("agent_id is required and must not exceed 128 characters")
	}
	if name == "" || len(name) > 128 {
		return errors.New("agent name is required and must not exceed 128 characters")
	}
	for field, value := range map[string]string{
		"region": region, "zone": zone, "failure_domain": failureDomain,
	} {
		value = strings.TrimSpace(value)
		if field == "zone" && value == "" {
			continue
		}
		if !FleetLabelPattern.MatchString(value) {
			return fmt.Errorf("%s must be a lowercase operator label", field)
		}
	}
	if reservedCPU < 0 || reservedMemory < 0 {
		return errors.New("resource reservations must not be negative")
	}
	return nil
}

func (s *persistence) authorizeOperator(ctx context.Context, userID string) error {
	var one int
	return s.db.QueryRowContext(ctx, `SELECT 1 FROM platform_operators WHERE user_id = $1`, strings.TrimSpace(userID)).Scan(&one)
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
