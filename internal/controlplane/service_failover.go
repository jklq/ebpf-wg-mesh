package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"fmt"
	"log/slog"
	"time"
)

type failoverDelivery interface {
	ReconcileFailover(context.Context, time.Duration) (deliverycore.ServiceFailoverResult, error)
}

type ServiceFailoverReconciler struct {
	delivery           failoverDelivery
	interval           time.Duration
	unhealthyThreshold time.Duration
}

func NewServiceFailoverReconciler(delivery failoverDelivery, interval, unhealthyThreshold time.Duration) *ServiceFailoverReconciler {
	return &ServiceFailoverReconciler{
		delivery: delivery, interval: interval, unhealthyThreshold: unhealthyThreshold,
	}
}

func (r *ServiceFailoverReconciler) Reconcile(ctx context.Context) (deliverycore.ServiceFailoverResult, error) {
	if r == nil || r.delivery == nil {
		return deliverycore.ServiceFailoverResult{}, nil
	}
	return r.delivery.ReconcileFailover(ctx, r.unhealthyThreshold)
}

func (r *ServiceFailoverReconciler) Run(ctx context.Context) error {
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
