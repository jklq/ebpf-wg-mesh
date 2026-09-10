package delivery

import (
	"context"
	"fmt"
	"time"
)

// ReconcileRollouts advances every active rollout through placement, ingress
// withdrawal, draining, and failover, then performs the wake effects required
// by the committed changes.
func (d *Delivery) ReconcileRollouts(ctx context.Context) error {
	var now time.Time
	if d.rolloutNow == nil {
		now = d.live.currentTime()
	} else {
		now = d.rolloutNow().UTC()
	}

	serviceIDs := d.live.InProgressRolloutServiceIDs()

	changed, ingressChanged := false, false
	var waitingForIngress []string
	for _, serviceID := range serviceIDs {
		result, err := d.advanceRollout(ctx, serviceID, now)
		if err != nil {
			return fmt.Errorf("advance service %s: %w", serviceID, err)
		}
		changed = changed || result.Changed
		ingressChanged = ingressChanged || result.IngressChanged
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
	return nil
}
