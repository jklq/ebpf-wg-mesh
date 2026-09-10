package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"errors"
	"fmt"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

type fleetLiveReader interface {
	AgentUsage() map[string]deliverycore.AgentUsage
	Position() deliverycore.LivePosition
}

func (s *fleetPersistence) AuthorizeAgentCredential(ctx context.Context, agentID string) error {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM agents
		WHERE id = $1 AND lifecycle_state <> 'retired' AND credential_revoked_at IS NULL`, strings.TrimSpace(agentID)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return deliverycore.ErrAgentCredentialRevoked
	}
	return err
}

func lifecycleStateProto(state deliverycore.AgentLifecycleState) platformv1.AgentLifecycleState {
	switch state {
	case deliverycore.AgentStateEnrolling:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_ENROLLING
	case deliverycore.AgentStateActive:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_ACTIVE
	case deliverycore.AgentStateCordoned:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_CORDONED
	case deliverycore.AgentStateDraining:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_DRAINING
	case deliverycore.AgentStateUnavailable:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_UNAVAILABLE
	case deliverycore.AgentStateRetired:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_RETIRED
	default:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_UNSPECIFIED
	}
}

func lifecycleStateRecord(state platformv1.AgentLifecycleState) deliverycore.AgentLifecycleState {
	switch state {
	case platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_ACTIVE:
		return deliverycore.AgentStateActive
	case platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_CORDONED:
		return deliverycore.AgentStateCordoned
	case platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_DRAINING:
		return deliverycore.AgentStateDraining
	case platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_RETIRED:
		return deliverycore.AgentStateRetired
	default:
		return ""
	}
}

func (s *fleetPersistence) RecordAgentCertificate(ctx context.Context, agentID, serial string) error {
	agentID = strings.TrimSpace(agentID)
	serial = strings.ToLower(strings.TrimSpace(serial))
	if agentID == "" || serial == "" {
		return errors.New("agent certificate serial is required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_certificates(serial, agent_id, issued_at)
		VALUES ($1, $2, $3) ON CONFLICT(serial) DO NOTHING`, serial, agentID, time.Now().UTC())
	return err
}

func (s *fleetPersistence) listAgentCertificateSerials(ctx context.Context, agentID string) ([]string, error) {
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

func (s *fleetPersistence) fleetView(ctx context.Context, userID string) (*platformv1.Fleet, error) {
	if err := s.reads.AuthorizeOperator(ctx, userID); err != nil {
		return nil, err
	}
	agents, err := s.reads.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	usageByAgent := s.live.AgentUsage()
	versions := make(map[string]int)
	for _, agent := range agents {
		if agent.LifecycleState != deliverycore.AgentStateRetired && agent.SoftwareVersion != "" {
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
		item.AllocationCount = used.Allocations
		item.AllocatedCpuMillis = used.CPUMillis
		item.AllocatedMemoryMebibytes = used.MemoryMebibytes
		item.HeadroomCpuMillis = max(item.SchedulableCpuMillis-used.CPUMillis, 0)
		item.HeadroomMemoryMebibytes = max(item.SchedulableMemoryMebibytes-used.MemoryMebibytes, 0)
		if recommendedVersion != "" && item.SoftwareVersion != "" && item.SoftwareVersion != recommendedVersion && rec.LifecycleState != deliverycore.AgentStateRetired {
			item.VersionSkewWarning = fmt.Sprintf("reports %s while the fleet majority reports %s", item.SoftwareVersion, recommendedVersion)
		}
		fleet.Agents = append(fleet.Agents, item)
		if rec.LifecycleState != deliverycore.AgentStateRetired {
			fleet.Capacity.NodeCount++
		}
		if rec.LifecycleState == deliverycore.AgentStateActive && rec.Healthy(now) {
			fleet.Capacity.SchedulableNodeCount++
			fleet.Capacity.SchedulableCpuMillis += item.SchedulableCpuMillis
			fleet.Capacity.SchedulableMemoryMebibytes += item.SchedulableMemoryMebibytes
			fleet.Capacity.AllocatedCpuMillis += used.CPUMillis
			fleet.Capacity.AllocatedMemoryMebibytes += used.MemoryMebibytes
			fleet.Capacity.HeadroomCpuMillis += item.HeadroomCpuMillis
			fleet.Capacity.HeadroomMemoryMebibytes += item.HeadroomMemoryMebibytes
		}
	}
	if len(versions) > 1 {
		fleet.VersionWarning = fmt.Sprintf("fleet software version skew detected; converge nodes on %s", recommendedVersion)
	}
	if s.live != nil {
		fleet.Live = toProtoLiveRead(s.live.Position())
	}
	return fleet, nil
}
