package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

type RolloutReconciler struct {
	store    *Store
	notifier *Notifier
	ingress  platformIngress
	events   *PlatformEvents
	interval time.Duration
	now      func() time.Time
}

func NewRolloutReconciler(store *Store, notifier *Notifier, ingress platformIngress, events *PlatformEvents, interval time.Duration) *RolloutReconciler {
	return &RolloutReconciler{store: store, notifier: notifier, ingress: ingress, events: events, interval: interval, now: time.Now}
}

func (r *RolloutReconciler) Reconcile(ctx context.Context) error {
	rows, err := r.store.db.QueryContext(ctx,
		`SELECT DISTINCT sr.service_id
		   FROM service_rollouts sr
		   JOIN services s ON s.id = sr.service_id AND s.current_rollout_generation = sr.rollout_generation
		  WHERE sr.state = $1
		     OR EXISTS(SELECT 1 FROM allocations a WHERE a.service_id = sr.service_id AND a.rollout_state IN ($2, $3))
		  ORDER BY sr.service_id`,
		rolloutStateInProgress, allocationRolloutDraining, allocationRolloutWithdrawing)
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
	if err := rows.Close(); err != nil {
		return err
	}
	changed, ingressChanged := false, false
	environments := map[string]struct{}{}
	var waitingForIngress []string
	for _, serviceID := range serviceIDs {
		result, err := r.store.advanceRollout(ctx, serviceID, r.now().UTC())
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
		if r.ingress != nil {
			if err := r.ingress.Sync(ctx); err != nil {
				return fmt.Errorf("converge ingress before drain: %w", err)
			}
		}
		for _, serviceID := range waitingForIngress {
			confirmed, err := r.store.confirmRolloutIngressConverged(ctx, serviceID, r.now().UTC())
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
		agents, err := r.store.listAgents(ctx)
		if err != nil {
			return err
		}
		cutoff := r.now().UTC().Add(-agentHealthyTTL)
		for _, agent := range agents {
			switch agent.LifecycleState {
			case agentStateDraining:
				if _, err := r.store.reconcileDrainingAgent(ctx, agent.ID); err != nil {
					return fmt.Errorf("refresh draining agent %s: %w", agent.ID, err)
				}
			case agentStateUnavailable:
				if _, _, err := r.store.failoverServicesFromAgent(ctx, agent.ID, cutoff); err != nil {
					return fmt.Errorf("retry node-loss replacement for agent %s: %w", agent.ID, err)
				}
			}
		}
	}
	if changed && r.notifier != nil {
		ids, err := r.store.agentIDs(ctx)
		if err != nil {
			return err
		}
		r.notifier.NotifyAll(ids)
	}
	if ingressChanged && len(waitingForIngress) == 0 && r.ingress != nil {
		r.ingress.RequestSync()
	}
	for environmentID := range environments {
		if r.events != nil {
			r.events.Publish(environmentID)
		}
	}
	return nil
}

func (r *RolloutReconciler) Run(ctx context.Context) error {
	if r.interval <= 0 {
		return fmt.Errorf("rollout reconcile interval must be positive")
	}
	reconcile := func() {
		if err := r.Reconcile(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("rollout reconcile failed", "error", err)
		}
	}
	reconcile()
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			reconcile()
		}
	}
}
