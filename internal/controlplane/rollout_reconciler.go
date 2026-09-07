package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

type rolloutDelivery interface {
	ReconcileRollouts(context.Context) error
}

type RolloutReconciler struct {
	delivery rolloutDelivery
	interval time.Duration
}

func NewRolloutReconciler(delivery rolloutDelivery, interval time.Duration) *RolloutReconciler {
	return &RolloutReconciler{delivery: delivery, interval: interval}
}

func (r *RolloutReconciler) Reconcile(ctx context.Context) error {
	return r.delivery.ReconcileRollouts(ctx)
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
