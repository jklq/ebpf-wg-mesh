//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (s *persistence) markAllocationHealthyForTest(ctx context.Context, serviceID, allocationIP string, healthyPorts ...int32) error {
	encodedPorts, err := json.Marshal(healthyPorts)
	if err != nil {
		return err
	}
	addressColumn, portsColumn := testAllocationFamilyColumns(allocationIP)
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE allocations
			    SET healthy = TRUE,
			        %s = $1,
			        %s = $2,
			        applied_spec_revision = GREATEST(applied_spec_revision, desired_spec_revision),
			        applied_rollout_generation = GREATEST(applied_rollout_generation, desired_rollout_generation),
			        rollout_state = $5,
			        updated_at = $3
			  WHERE service_id = $4`, addressColumn, portsColumn),
			allocationIP, encodedPorts, time.Now().UTC(), serviceID, deliverycore.AllocationRolloutServing,
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
		return seedActiveDeploymentTx(ctx, tx, serviceID)
	})
}
func (s *persistence) markAllocationIDHealthyForTest(ctx context.Context, allocationID, allocationIP string, healthyPorts ...int32) error {
	encodedPorts, err := json.Marshal(healthyPorts)
	if err != nil {
		return err
	}
	addressColumn, portsColumn := testAllocationFamilyColumns(allocationIP)
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var serviceID string
		var desiredRollout int64
		if err := tx.QueryRowContext(ctx,
			fmt.Sprintf(`UPDATE allocations
			    SET healthy = TRUE,
			        %s = $1,
			        %s = $2,
			        applied_spec_revision = GREATEST(applied_spec_revision, desired_spec_revision),
			        applied_rollout_generation = GREATEST(applied_rollout_generation, desired_rollout_generation),
			        rollout_state = $5,
			        updated_at = $3
			  WHERE id = $4
			  RETURNING service_id, desired_rollout_generation`, addressColumn, portsColumn),
			allocationIP, encodedPorts, time.Now().UTC(), allocationID, deliverycore.AllocationRolloutServing,
		).Scan(&serviceID, &desiredRollout); err != nil {
			return err
		}
		return seedActiveDeploymentTx(ctx, tx, serviceID)
	})
}
func testAllocationFamilyColumns(allocationIP string) (addressColumn, portsColumn string) {
	if addr, err := netip.ParseAddr(strings.TrimSpace(allocationIP)); err == nil && addr.Is4() {
		return "allocation_ipv4", "healthy_ipv4_ports"
	}
	return "allocation_ipv6", "healthy_ipv6_ports"
}
func seedActiveDeploymentTx(ctx context.Context, tx *sql.Tx, serviceID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO deployment_transitions(id,deployment_id,from_state,to_state,cause_kind,cause_id,reason_code,detail,spec_revision,image_digest,rollout_generation,occurred_at)
 SELECT $1,id,state,'active','system','','DEPLOYMENT_ACTIVE','Marked healthy for test',spec_revision,image_digest,rollout_generation,statement_timestamp() FROM deployments WHERE service_id=$2 AND is_current AND state<>'active'`, uuid.NewString(), serviceID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE deployments SET state='active',cause_kind='system',reason_code='DEPLOYMENT_ACTIVE',detail='Marked healthy for test',updated_at=statement_timestamp() WHERE service_id=$1 AND is_current`, serviceID)
	return err
}
func (s *persistence) currentDesiredRevisionForAgent(ctx context.Context, id string) (int64, error) {
	var rev int64
	err := s.db.QueryRowContext(ctx, `SELECT desired_revision FROM agents WHERE id=$1`, id).Scan(&rev)
	return rev, err
}
