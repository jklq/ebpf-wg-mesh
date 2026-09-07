package controlplane

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

var (
	errAgentNotEnrolled       = errors.New("agent is not enrolled by an operator")
	errAgentCredentialRevoked = errors.New("agent credential is revoked")
	errAgentHasAllocations    = errors.New("agent still has allocations or attachments")
	errInvalidAgentTransition = errors.New("invalid agent lifecycle transition")
	fleetLabelPattern         = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62})$`)
)

const agentSelectSQL = `SELECT id, name, lifecycle_state, state_before_unavailable,
		region, zone, failure_domain, reserved_cpu_millis, reserved_memory_mebibytes,
		advertise_addr, workload_ipv4_subnet, workload_ipv6_subnet, wireguard_public_key, wireguard_listen_port,
		wireguard_ipv6, cpu_millis_capacity, memory_mebibytes_capacity,
		runtime_capabilities, software_version, maintenance_message,
		credential_revoked_at, last_seen_at
	FROM agents`

func scanAgentRecord(scanner interface{ Scan(...any) error }) (agentRecord, error) {
	var rec agentRecord
	var capabilities jsonStringSlice
	if err := scanner.Scan(
		&rec.ID, &rec.Name, &rec.LifecycleState, &rec.StateBeforeUnavailable,
		&rec.Region, &rec.Zone, &rec.FailureDomain, &rec.ReservedCPUMillis, &rec.ReservedMemoryMebibytes,
		&rec.AdvertiseAddr, &rec.WorkloadIPv4Subnet, &rec.WorkloadIPv6Subnet, &rec.WireGuardPublicKey, &rec.WireGuardListenPort,
		&rec.WireGuardIPv6, &rec.CPUMillisCapacity, &rec.MemoryMebibytesCapcity,
		&capabilities, &rec.SoftwareVersion, &rec.MaintenanceMessage,
		&rec.CredentialRevokedAt, &rec.LastSeenAt,
	); err != nil {
		return agentRecord{}, err
	}
	rec.RuntimeCapabilities = append([]string(nil), capabilities...)
	return rec, nil
}

func agentByIDQuerier(ctx context.Context, q serviceQueryer, agentID string, forUpdate bool) (agentRecord, error) {
	query := agentSelectSQL + ` WHERE id = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanAgentRecord(q.QueryRowContext(ctx, query, strings.TrimSpace(agentID)))
}

func canonicalCapabilities(values []string) []string {
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

func validateFleetAgentInput(id, name, region, zone, failureDomain string, reservedCPU, reservedMemory int64) error {
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
		if !fleetLabelPattern.MatchString(value) {
			return fmt.Errorf("%s must be a lowercase operator label", field)
		}
	}
	if reservedCPU < 0 || reservedMemory < 0 {
		return errors.New("resource reservations must not be negative")
	}
	return nil
}

func (s *Store) authorizeOperator(ctx context.Context, userID string) error {
	var one int
	return s.db.QueryRowContext(ctx, `SELECT 1 FROM platform_operators WHERE user_id = $1`, strings.TrimSpace(userID)).Scan(&one)
}

func (s *Store) authorizeAgentCredential(ctx context.Context, agentID string) error {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM agents
		WHERE id = $1 AND lifecycle_state <> 'retired' AND credential_revoked_at IS NULL`, strings.TrimSpace(agentID)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return errAgentCredentialRevoked
	}
	return err
}

func validateAgentTransition(current, target agentLifecycleState) error {
	if current == target {
		return nil
	}
	if current == agentStateRetired {
		return fmt.Errorf("%w: %s to %s", errInvalidAgentTransition, current, target)
	}
	if target == agentStateRetired && (current == agentStateEnrolling || current == agentStateCordoned || current == agentStateDraining || current == agentStateUnavailable) {
		return nil
	}
	if current == agentStateEnrolling || current == agentStateUnavailable {
		return fmt.Errorf("%w: %s to %s", errInvalidAgentTransition, current, target)
	}
	switch target {
	case agentStateActive:
		if current == agentStateCordoned || current == agentStateDraining {
			return nil
		}
	case agentStateCordoned:
		if current == agentStateActive || current == agentStateDraining {
			return nil
		}
	case agentStateDraining:
		if current == agentStateActive || current == agentStateCordoned {
			return nil
		}
	case agentStateRetired:
		if current == agentStateCordoned || current == agentStateDraining {
			return nil
		}
	}
	return fmt.Errorf("%w: %s to %s", errInvalidAgentTransition, current, target)
}

func (s *Store) activeAgentIDs(ctx context.Context) ([]string, error) {
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
	hash := bootstrapTokenHash(token)
	_, err := tx.ExecContext(ctx, `INSERT INTO agent_bootstrap_tokens(token_hash, agent_id, origin, created_at, consumed_at)
		VALUES ($1, $2, $3, $4, NULL)`, hash[:], strings.TrimSpace(agentID), origin, now)
	return err
}

func lifecycleStateProto(state agentLifecycleState) platformv1.AgentLifecycleState {
	switch state {
	case agentStateEnrolling:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_ENROLLING
	case agentStateActive:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_ACTIVE
	case agentStateCordoned:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_CORDONED
	case agentStateDraining:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_DRAINING
	case agentStateUnavailable:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_UNAVAILABLE
	case agentStateRetired:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_RETIRED
	default:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_UNSPECIFIED
	}
}

func lifecycleStateRecord(state platformv1.AgentLifecycleState) agentLifecycleState {
	switch state {
	case platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_ACTIVE:
		return agentStateActive
	case platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_CORDONED:
		return agentStateCordoned
	case platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_DRAINING:
		return agentStateDraining
	case platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_RETIRED:
		return agentStateRetired
	default:
		return ""
	}
}

func encodeCapabilities(values []string) ([]byte, error) {
	return json.Marshal(canonicalCapabilities(values))
}

func (s *Store) recordAgentCertificate(ctx context.Context, agentID, serial string) error {
	agentID = strings.TrimSpace(agentID)
	serial = strings.ToLower(strings.TrimSpace(serial))
	if agentID == "" || serial == "" {
		return errors.New("agent certificate serial is required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_certificates(serial, agent_id, issued_at)
		VALUES ($1, $2, $3) ON CONFLICT(serial) DO NOTHING`, serial, agentID, time.Now().UTC())
	return err
}

func (s *Store) listAgentCertificateSerials(ctx context.Context, agentID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT serial FROM agent_certificates WHERE agent_id = $1 ORDER BY issued_at`, strings.TrimSpace(agentID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var serials []string
	for rows.Next() {
		var serial string
		if err := rows.Scan(&serial); err != nil {
			return nil, err
		}
		serials = append(serials, serial)
	}
	return serials, rows.Err()
}

func (s *Store) fleetView(ctx context.Context, userID string) (*platformv1.Fleet, error) {
	if err := s.authorizeOperator(ctx, userID); err != nil {
		return nil, err
	}
	agents, err := s.listAgents(ctx)
	if err != nil {
		return nil, err
	}
	type usage struct {
		allocations int32
		cpu         int64
		memory      int64
	}
	usageByAgent := make(map[string]usage, len(agents))
	rows, err := s.db.QueryContext(ctx, `SELECT a.agent_id, count(*),
		COALESCE(SUM(COALESCE((r.spec_json->'runtime'->>'cpuMillis')::INT8, 0)), 0),
		COALESCE(SUM(COALESCE((r.spec_json->'runtime'->>'memoryMebibytes')::INT8, 0)), 0)
		FROM allocations a
		JOIN services s ON s.id = a.service_id
		JOIN service_revisions r ON r.service_id = s.id AND r.spec_revision = s.current_spec_revision
		GROUP BY a.agent_id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var item usage
		if err := rows.Scan(&id, &item.allocations, &item.cpu, &item.memory); err != nil {
			rows.Close()
			return nil, err
		}
		usageByAgent[id] = item
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	versions := make(map[string]int)
	for _, agent := range agents {
		if agent.LifecycleState != agentStateRetired && agent.SoftwareVersion != "" {
			versions[agent.SoftwareVersion]++
		}
	}
	recommendedVersion := ""
	for version, count := range versions {
		if count > versions[recommendedVersion] || (count == versions[recommendedVersion] && version > recommendedVersion) {
			recommendedVersion = version
		}
	}
	fleet := &platformv1.Fleet{Capacity: &platformv1.FleetCapacity{}}
	now := time.Now().UTC()
	for _, rec := range agents {
		item := toProtoAgent(rec)
		used := usageByAgent[rec.ID]
		item.AllocationCount = used.allocations
		item.AllocatedCpuMillis = used.cpu
		item.AllocatedMemoryMebibytes = used.memory
		item.HeadroomCpuMillis = max(item.SchedulableCpuMillis-used.cpu, 0)
		item.HeadroomMemoryMebibytes = max(item.SchedulableMemoryMebibytes-used.memory, 0)
		if recommendedVersion != "" && item.SoftwareVersion != "" && item.SoftwareVersion != recommendedVersion && rec.LifecycleState != agentStateRetired {
			item.VersionSkewWarning = fmt.Sprintf("reports %s while the fleet majority reports %s", item.SoftwareVersion, recommendedVersion)
		}
		fleet.Agents = append(fleet.Agents, item)
		if rec.LifecycleState != agentStateRetired {
			fleet.Capacity.NodeCount++
		}
		if rec.LifecycleState == agentStateActive && rec.healthy(now) {
			fleet.Capacity.SchedulableNodeCount++
			fleet.Capacity.SchedulableCpuMillis += item.SchedulableCpuMillis
			fleet.Capacity.SchedulableMemoryMebibytes += item.SchedulableMemoryMebibytes
			fleet.Capacity.AllocatedCpuMillis += used.cpu
			fleet.Capacity.AllocatedMemoryMebibytes += used.memory
			fleet.Capacity.HeadroomCpuMillis += item.HeadroomCpuMillis
			fleet.Capacity.HeadroomMemoryMebibytes += item.HeadroomMemoryMebibytes
		}
	}
	if len(versions) > 1 {
		fleet.VersionWarning = fmt.Sprintf("fleet software version skew detected; converge nodes on %s", recommendedVersion)
	}
	return fleet, nil
}
