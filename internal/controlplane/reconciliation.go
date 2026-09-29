package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

type rolloutDelivery interface {
	ReconcileRollouts(context.Context) error
}

type rolloutReconciler struct {
	delivery rolloutDelivery
	interval time.Duration
}

func newRolloutReconciler(delivery rolloutDelivery, interval time.Duration) *rolloutReconciler {
	return &rolloutReconciler{delivery: delivery, interval: interval}
}

func (r *rolloutReconciler) Reconcile(ctx context.Context) error {
	return r.delivery.ReconcileRollouts(ctx)
}

func (r *rolloutReconciler) Run(ctx context.Context) error {
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

type failoverDelivery interface {
	ReconcileFailover(context.Context, time.Duration) (deliverycore.ServiceFailoverResult, error)
}

type serviceFailoverReconciler struct {
	delivery           failoverDelivery
	interval           time.Duration
	unhealthyThreshold time.Duration
}

func newServiceFailoverReconciler(delivery failoverDelivery, interval, unhealthyThreshold time.Duration) *serviceFailoverReconciler {
	return &serviceFailoverReconciler{
		delivery: delivery, interval: interval, unhealthyThreshold: unhealthyThreshold,
	}
}

func (r *serviceFailoverReconciler) Reconcile(ctx context.Context) (deliverycore.ServiceFailoverResult, error) {
	if r == nil || r.delivery == nil {
		return deliverycore.ServiceFailoverResult{}, nil
	}
	return r.delivery.ReconcileFailover(ctx, r.unhealthyThreshold)
}

func (r *serviceFailoverReconciler) Run(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if r.interval <= 0 {
		return fmt.Errorf("failover reconcile interval must be greater than zero")
	}
	reconcile := func() {
		result, err := r.Reconcile(ctx)
		if err != nil {
			slog.Warn("service failover reconcile failed", "error", err)
			return
		}
		if len(result.MovedServiceIDs) > 0 || len(result.BlockedServiceIDs) > 0 {
			slog.Info("service failover reconciled", "moved", len(result.MovedServiceIDs), "blocked", len(result.BlockedServiceIDs))
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
