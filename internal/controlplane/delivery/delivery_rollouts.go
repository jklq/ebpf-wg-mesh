package delivery

import (
	"context"
	"fmt"
	"time"
)

// ReconcileRollouts advances every active rollout through placement, ingress
// withdrawal, draining, and failover, then performs the wake and publication
// effects required by the committed changes.
func (d *Delivery) ReconcileRollouts(ctx context.Context) error {
	var now time.Time
	if d.rolloutNow == nil {
		var err error
		now, err = DatabaseTime(ctx, d.store.db)
		if err != nil {
			return fmt.Errorf("read database time: %w", err)
		}
	} else {
		now = d.rolloutNow().UTC()
	}

	rows, err := d.store.db.QueryContext(ctx,
		`SELECT DISTINCT sr.service_id
		   FROM service_rollouts sr
		   JOIN services s ON s.id = sr.service_id AND s.current_rollout_generation = sr.rollout_generation
		  WHERE sr.state = $1
		     OR EXISTS(SELECT 1 FROM allocations a WHERE a.service_id = sr.service_id AND a.rollout_state IN ($2, $3))
		  ORDER BY sr.service_id`,
		rolloutStateInProgress, AllocationRolloutDraining, AllocationRolloutWithdrawing)
	if err != nil {
		return err
	}
	var serviceIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		serviceIDs = append(serviceIDs, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	changed, ingressChanged := false, false
	environments := map[string]struct{}{}
	var waitingForIngress []string
	for _, serviceID := range serviceIDs {
		result, err := d.advanceRollout(ctx, serviceID, now)
		if err != nil {
			return fmt.Errorf("advance service %s: %w", serviceID, err)
		}
		changed = changed || result.Changed
		ingressChanged = ingressChanged || result.IngressChanged
		if result.Changed && result.EnvironmentID != "" {
			environments[result.EnvironmentID] = struct{}{}
		}
		if result.NeedsIngressConvergence {
			waitingForIngress = append(waitingForIngress, serviceID)
		}
	}

	if len(waitingForIngress) > 0 {
		if d.ingress != nil {
			if err := d.ingress.Sync(ctx); err != nil {
				return fmt.Errorf("converge ingress before drain: %w", err)
			}
		}
		for _, serviceID := range waitingForIngress {
			confirmed, err := d.confirmRolloutIngressConverged(ctx, serviceID, now)
			if err != nil {
				return fmt.Errorf("confirm ingress convergence for service %s: %w", serviceID, err)
			}
			changed = changed || confirmed.Changed
			if confirmed.Changed && confirmed.EnvironmentID != "" {
				environments[confirmed.EnvironmentID] = struct{}{}
			}
		}
	}

	if changed {
		agents, err := d.store.listAgents(ctx)
		if err != nil {
			return err
		}
		cutoff := now.Add(-AgentHealthyTTL)
		for _, agent := range agents {
			switch agent.LifecycleState {
			case AgentStateDraining:
				if _, err := d.reconcileDrainingAgent(ctx, agent.ID); err != nil {
					return fmt.Errorf("refresh draining agent %s: %w", agent.ID, err)
				}
			case AgentStateUnavailable:
				if _, _, err := d.failoverServicesFromAgent(ctx, agent.ID, cutoff); err != nil {
					return fmt.Errorf("retry node-loss replacement for agent %s: %w", agent.ID, err)
				}
			}
		}
	}

	if changed && d.notifier != nil {
		ids, err := d.store.agentIDs(ctx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			d.notifier.Notify(id)
		}
	}
	if ingressChanged && len(waitingForIngress) == 0 && d.ingress != nil {
		d.ingress.RequestSync()
	}
	for environmentID := range environments {
		if d.events != nil {
			if _, err := d.events.Publish(ctx, environmentID); err != nil {
				return fmt.Errorf("publish rollout event: %w", err)
			}
		}
	}
	return nil
}
