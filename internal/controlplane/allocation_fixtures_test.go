//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (s *persistence) markAllocationHealthyForTest(ctx context.Context, serviceID, allocationIP string, healthyPorts ...int32) error {
	addressColumn, _ := testAllocationFamilyColumns(allocationIP)
	if err := s.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE allocation_assignments SET %s = $1, rollout_state = $4, updated_at = $2 WHERE service_id = $3`, addressColumn),
			allocationIP, time.Now().UTC(), serviceID, deliverycore.AllocationRolloutServing,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE service_rollouts
			    SET state = $1, failure_reason = '', completed_at = $2, progress_at = $2
			  WHERE service_id = $3
			    AND rollout_generation = (SELECT current_rollout_generation FROM services WHERE id = $3)`,
			"succeeded", time.Now().UTC(), serviceID,
		); err != nil {
			return err
		}
		if err := recordServiceAssignmentsAndRollout(ctx, tx, serviceID); err != nil {
			return err
		}
		return seedActiveDeploymentTx(ctx, tx, serviceID)
	}); err != nil {
		return err
	}
	allocs, err := s.reads.ListAllocationsByServiceID(ctx, serviceID)
	if err != nil {
		return err
	}
	for _, alloc := range allocs {
		if err := observeAllocationHealthyForTest(ctx, s, alloc, allocationIP, healthyPorts...); err != nil {
			return err
		}
	}
	return nil
}
func (s *persistence) markAllocationIDHealthyForTest(ctx context.Context, allocationID, allocationIP string, healthyPorts ...int32) error {
	addressColumn, _ := testAllocationFamilyColumns(allocationIP)
	if err := s.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := time.Now().UTC()
		if strings.TrimSpace(allocationIP) == "" {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE allocation_assignments SET %s = $1, updated_at = $2 WHERE id = $3`, addressColumn),
			allocationIP, now, allocationID,
		); err != nil {
			return err
		}
		journal.RecordAssignment(ctx, allocationID)
		return nil
	}); err != nil {
		return err
	}
	var rec deliverycore.AllocationRecord
	if err := s.db.QueryRowContext(ctx, `SELECT id, service_id, agent_id, desired_spec_revision, desired_rollout_generation, allocation_ipv4, allocation_ipv6
		FROM allocation_assignments WHERE id = $1`, allocationID).Scan(
		&rec.ID, &rec.ServiceID, &rec.AgentID, &rec.DesiredSpecRevision, &rec.DesiredRolloutGeneration, &rec.AllocationIPv4, &rec.AllocationIPv6); err != nil {
		return err
	}
	return observeAllocationHealthyForTest(ctx, s, rec, allocationIP, healthyPorts...)
}

func observeAllocationHealthyForTest(ctx context.Context, store *persistence, alloc deliverycore.AllocationRecord, reportedIP string, healthyPorts ...int32) error {
	session, ok := fixtureLive(store).Session(alloc.AgentID)
	if !ok {
		return fmt.Errorf("no live session for agent %s", alloc.AgentID)
	}
	ipv4Ports, ipv6Ports := []int32(nil), []int32(nil)
	if _, col := testAllocationFamilyColumns(reportedIP); col == "healthy_ipv4_ports" {
		ipv4Ports = healthyPorts
	} else {
		ipv6Ports = healthyPorts
	}
	inventory, err := agentAllocationIDsForTest(ctx, store, alloc.AgentID)
	if err != nil {
		return err
	}
	if err := fixtureLive(store).AcceptReport(alloc.AgentID, session.SessionID, session.Sequence+1, inventory, true); err != nil {
		return err
	}
	_, err = fixtureLive(store).RecordObservation(deliverycore.AllocationObservation{
		AllocationID: alloc.ID, RolloutGeneration: alloc.DesiredRolloutGeneration,
		AppliedSpecRevision: alloc.DesiredSpecRevision, AppliedGeneration: alloc.DesiredRolloutGeneration,
		Phase: "Healthy", Healthy: true, HealthyIPv4Ports: ipv4Ports, HealthyIPv6Ports: ipv6Ports,
		AgentID: alloc.AgentID, SessionID: session.SessionID, Sequence: session.Sequence + 1, ObservedAt: time.Now().UTC(),
	})
	return err
}
func testAllocationFamilyColumns(allocationIP string) (addressColumn, portsColumn string) {
	if addr, err := netip.ParseAddr(strings.TrimSpace(allocationIP)); err == nil && addr.Is4() {
		return "allocation_ipv4", "healthy_ipv4_ports"
	}
	return "allocation_ipv6", "healthy_ipv6_ports"
}

// agentAllocationIDsForTest returns the allocation IDs the control plane
// currently assigns to an agent, mirroring what the agent would report as its
// inventory so admission is re-evaluated correctly in fixtures.
func agentAllocationIDsForTest(ctx context.Context, store *persistence, agentID string) ([]string, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT id FROM allocation_assignments
		WHERE agent_id = $1 AND rollout_state <> 'lost' ORDER BY id`, agentID)
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
func seedActiveDeploymentTx(ctx context.Context, tx *sql.Tx, serviceID string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO deployment_transitions(id,deployment_id,from_state,to_state,cause_kind,cause_id,reason_code,detail,spec_revision,image_digest,rollout_generation,occurred_at)
 SELECT $1,id,state,'active','system','','DEPLOYMENT_ACTIVE','Marked healthy for test',spec_revision,image_digest,rollout_generation,statement_timestamp() FROM deployments WHERE service_id=$2 AND is_current AND state<>'active'`, uuid.NewString(), serviceID); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE deployments SET state='active',cause_kind='system',reason_code='DEPLOYMENT_ACTIVE',detail='Marked healthy for test',updated_at=statement_timestamp() WHERE service_id=$1 AND is_current RETURNING id`, serviceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		journal.RecordDeployment(ctx, id)
	}
	return rows.Err()
}

func recordServiceAssignmentsAndRollout(ctx context.Context, tx *sql.Tx, serviceID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id::STRING FROM allocation_assignments WHERE service_id = $1`, serviceID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		journal.RecordAssignment(ctx, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT current_rollout_generation FROM services WHERE id = $1`, serviceID).Scan(&generation); err != nil {
		return err
	}
	journal.RecordRollout(ctx, serviceID, generation)
	return nil
}
func (s *persistence) currentDesiredRevisionForAgent(ctx context.Context, id string) (int64, error) {
	_ = ctx
	rev, ok := fixtureLive(s).DesiredRevision(id)
	if !ok {
		return 0, sql.ErrNoRows
	}
	return rev, nil
}
