package delivery

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/restartpolicy"
)

func (d *Delivery) ObserveAgentStatus(ctx context.Context, agentID string, report *agentv1.StatusReport) error {
	ingressChanged, environmentIDs, err := d.recordStatusReport(ctx, agentID, report)
	if err != nil {
		return err
	}
	for _, environmentID := range environmentIDs {
		if d.events != nil {
			if _, err := d.events.Publish(ctx, environmentID); err != nil {
				return fmt.Errorf("publish status event: %w", err)
			}
		}
	}
	if ingressChanged && d.ingress != nil {
		d.ingress.RequestSync()
	}
	return nil
}

func (d *Delivery) recordStatusReport(ctx context.Context, agentID string, report *agentv1.StatusReport) (bool, []string, error) {
	s := d.store
	var ingressChanged bool
	changedEnvironments := make(map[string]struct{})
	rolloutServiceIDs := make(map[string]struct{})
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		ingressChanged = false
		changedEnvironments = make(map[string]struct{})
		rolloutServiceIDs = make(map[string]struct{})
		now := time.Now().UTC()
		for _, cond := range report.Services {
			var (
				prevAppliedSpecRevision      int64
				prevAppliedRolloutGeneration int64
				desiredSpecRevision          int64
				prevPhase                    string
				prevMessage                  string
				prevHealthy                  bool
				prevAllocationIPv4           string
				prevAllocationIPv6           string
				prevHealthyIPv4Ports         []int32
				prevHealthyIPv6Ports         []int32
				hasDomain                    bool
				environmentID                string
				serviceID                    string
				prevRestartRaw               []byte
			)
			err := tx.QueryRowContext(ctx,
				`SELECT a.applied_spec_revision,
				        a.applied_rollout_generation,
				        a.desired_spec_revision,
				        a.phase,
				        a.message,
				        a.healthy,
				        a.allocation_ipv4,
				        a.allocation_ipv6,
				        a.healthy_ipv4_ports,
				        a.healthy_ipv6_ports,
				        a.restart_observation_json,
				        EXISTS(SELECT 1 FROM domain_bindings d WHERE d.service_id = a.service_id),
				        s.environment_id,
				        a.service_id
				   FROM allocations a
				   JOIN services s ON s.id = a.service_id
				   JOIN agents ag ON ag.id = a.agent_id
				  WHERE a.id = $1 AND a.agent_id = $2`,
				cond.AllocationId, agentID,
			).Scan(
				&prevAppliedSpecRevision,
				&prevAppliedRolloutGeneration,
				&desiredSpecRevision,
				&prevPhase,
				&prevMessage,
				&prevHealthy,
				&prevAllocationIPv4,
				&prevAllocationIPv6,
				(*jsonInt32Slice)(&prevHealthyIPv4Ports),
				(*jsonInt32Slice)(&prevHealthyIPv6Ports),
				&prevRestartRaw,
				&hasDomain,
				&environmentID,
				&serviceID,
			)
			if err != nil {
				if err == sql.ErrNoRows {
					continue
				}
				return fmt.Errorf("load allocation status: %w", err)
			}
			healthyIPv4Ports, err := encodeHealthyPorts(cond.GetHealthyIpv4Ports())
			if err != nil {
				return fmt.Errorf("encode healthy IPv4 ports: %w", err)
			}
			healthyIPv6Ports, err := encodeHealthyPorts(cond.GetHealthyIpv6Ports())
			if err != nil {
				return fmt.Errorf("encode healthy IPv6 ports: %w", err)
			}
			allocationIPv4 := strings.TrimSpace(cond.GetAllocationIpv4())
			allocationIPv6 := strings.TrimSpace(cond.GetAllocationIpv6())
			if s.useReportedAllocationIP {
				if allocationIPv4 == "" {
					allocationIPv4 = prevAllocationIPv4
				}
				if allocationIPv6 == "" {
					allocationIPv6 = prevAllocationIPv6
				}
				if allocationIPv4 != "" && (net.ParseIP(allocationIPv4) == nil || net.ParseIP(allocationIPv4).To4() == nil) {
					return fmt.Errorf("reported allocation IPv4 %q is invalid", allocationIPv4)
				}
				if allocationIPv6 != "" && (net.ParseIP(allocationIPv6) == nil || net.ParseIP(allocationIPv6).To4() != nil) {
					return fmt.Errorf("reported allocation IPv6 %q is invalid", allocationIPv6)
				}
			} else {
				if allocationIPv4 != prevAllocationIPv4 || allocationIPv6 != prevAllocationIPv6 {
					return fmt.Errorf("allocation %s reported addresses %q/%q, want %q/%q", cond.GetAllocationId(), allocationIPv4, allocationIPv6, prevAllocationIPv4, prevAllocationIPv6)
				}
			}
			restartRaw, err := encodeRestartObservation(cond.GetRestart())
			if err != nil {
				return fmt.Errorf("encode restart observation: %w", err)
			}
			statusChanged := prevAppliedSpecRevision != cond.GetAppliedSpecRevision() ||
				prevAppliedRolloutGeneration != cond.GetAppliedRolloutGeneration() ||
				prevPhase != cond.GetPhase() ||
				prevMessage != cond.GetMessage() ||
				prevAllocationIPv4 != allocationIPv4 || prevAllocationIPv6 != allocationIPv6 ||
				!equalInt32Slices(prevHealthyIPv4Ports, cond.GetHealthyIpv4Ports()) ||
				!equalInt32Slices(prevHealthyIPv6Ports, cond.GetHealthyIpv6Ports()) ||
				prevHealthy != cond.GetHealthy() ||
				string(prevRestartRaw) != string(restartRaw)
			if !statusChanged {
				continue
			}
			phase := cond.Phase
			healthy := cond.Healthy
			if cond.GetRestart().GetCrashLoop() || phase == restartpolicy.PhaseCrashLoop {
				phase = restartpolicy.PhaseCrashLoop
				healthy = false
			}
			appliedSpec := cond.GetAppliedSpecRevision()
			if appliedSpec == 0 && healthy && cond.GetAppliedRolloutGeneration() >= cond.GetDesiredRolloutGeneration() {
				appliedSpec = desiredSpecRevision
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE allocations
				    SET applied_spec_revision = $1,
				        applied_rollout_generation = $2,
				        phase = $3,
				        message = $4,
				        allocation_ipv4 = $5,
				        allocation_ipv6 = $6,
				        healthy_ipv4_ports = $7,
				        healthy_ipv6_ports = $8,
				        healthy = $9,
				        restart_observation_json = $10,
				        updated_at = $11
				  WHERE id = $12 AND agent_id = $13`,
				appliedSpec, cond.AppliedRolloutGeneration, phase, cond.Message, allocationIPv4, allocationIPv6, healthyIPv4Ports, healthyIPv6Ports, healthy, restartRaw, now, cond.AllocationId, agentID,
			); err != nil {
				return fmt.Errorf("update allocation status: %w", err)
			}
			changedEnvironments[environmentID] = struct{}{}
			var rolloutState string
			err = tx.QueryRowContext(ctx,
				`SELECT state FROM service_rollouts WHERE service_id = $1 AND rollout_generation = $2`,
				serviceID, cond.GetDesiredRolloutGeneration(),
			).Scan(&rolloutState)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if rolloutState == rolloutStateInProgress {
				rolloutServiceIDs[serviceID] = struct{}{}
			} else if err := s.applyAgentDeploymentObservationTx(ctx, tx, serviceID, cond.GetDesiredRolloutGeneration(), phase, cond.GetMessage(), healthy, cond.GetAppliedRolloutGeneration(), agentID); err != nil {
				return fmt.Errorf("apply deployment observation: %w", err)
			}
			if hasDomain && (prevHealthy != healthy || prevAllocationIPv4 != allocationIPv4 || prevAllocationIPv6 != allocationIPv6 || !equalInt32Slices(prevHealthyIPv4Ports, cond.GetHealthyIpv4Ports()) || !equalInt32Slices(prevHealthyIPv6Ports, cond.GetHealthyIpv6Ports()) || prevPhase != phase) {
				ingressChanged = true
			}
		}
		return nil
	})
	if err != nil {
		return false, nil, err
	}
	now := time.Now().UTC()
	for serviceID := range rolloutServiceIDs {
		advanced, advanceErr := d.advanceRollout(ctx, serviceID, now)
		if advanceErr != nil {
			return false, nil, fmt.Errorf("advance rollout after status: %w", advanceErr)
		}
		ingressChanged = ingressChanged || advanced.IngressChanged
		if advanced.EnvironmentID != "" {
			changedEnvironments[advanced.EnvironmentID] = struct{}{}
		}
	}
	environmentIDs := make([]string, 0, len(changedEnvironments))
	for environmentID := range changedEnvironments {
		environmentIDs = append(environmentIDs, environmentID)
	}
	return ingressChanged, environmentIDs, nil
}
