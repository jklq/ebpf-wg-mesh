package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"ebof-wg-mesh/internal/alarm"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/billing"
)

// newBillingService constructs the billing module from operator config.
// Enabled mode fails loudly without Stripe configuration or a completed
// shadow period; shadow never charges and needs no Stripe keys.
func newBillingService(ctx context.Context, cfg config.ControlPlaneConfig, store *persistence, alarms *alarm.Sender) (*billing.Service, error) {
	mode, err := billing.ParseMode(cfg.Billing.Mode)
	if err != nil {
		return nil, err
	}
	var adapter billing.StripeAdapter
	if key := strings.TrimSpace(cfg.Billing.StripeSecretKey); key != "" {
		adapter = billing.NewStripeAdapter(key, strings.TrimSpace(cfg.Billing.StripeAPIBaseURL))
	}
	if mode == billing.ModeEnabled {
		if adapter == nil || strings.TrimSpace(cfg.Billing.StripeWebhookSecret) == "" {
			return nil, fmt.Errorf("billing: enabled mode requires a Stripe secret key and webhook secret")
		}
	}
	if hourly := time.Duration(cfg.Usage.HourlyRetentionDays) * 24 * time.Hour; mode != billing.ModeDisabled && hourly < 33*24*time.Hour {
		slog.Warn("usage hourly retention is shorter than a billing period plus its close window; rating may lose evidence",
			"hourly_retention_days", cfg.Usage.HourlyRetentionDays)
	}
	service := billing.New(store.db, store.source.Work(), store.withProductTx, adapter, alarms, billing.Config{
		Mode:               mode,
		WebhookSecret:      strings.TrimSpace(cfg.Billing.StripeWebhookSecret),
		CheckoutSuccessURL: strings.TrimSpace(cfg.Billing.CheckoutSuccessURL),
		CheckoutCancelURL:  strings.TrimSpace(cfg.Billing.CheckoutCancelURL),
		HourlyRetention:    time.Duration(cfg.Usage.HourlyRetentionDays) * 24 * time.Hour,
	})
	if err := service.ValidateLaunchGate(ctx); err != nil {
		return nil, err
	}
	if paused, reason, err := service.CollectionPaused(ctx); err == nil && paused {
		slog.Warn("billing collection is paused by the kill switch", "reason", reason)
	}
	return service, nil
}

// billingMaintenanceLoop provisions accounts, closes periods, refreshes
// restrictions, and settles provider work on the live owner.
func (s *Server) billingMaintenanceLoop(ctx context.Context) {
	if s.billing == nil || s.billing.Mode() == billing.ModeDisabled {
		return
	}
	interval := time.Duration(s.cfg.Billing.MaintenanceSeconds) * time.Second
	if interval <= 0 {
		interval = time.Minute
	}
	maintain := func() {
		result, err := s.billing.Maintain(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("billing maintenance failed", "error", err)
			}
			return
		}
		if result.PeriodsClosed > 0 || result.InvoicesSettled > 0 || result.AccountsCreated > 0 {
			slog.Info("billing maintenance applied",
				"accounts_created", result.AccountsCreated,
				"periods_closed", result.PeriodsClosed,
				"invoices_settled", result.InvoicesSettled,
				"webhooks_applied", result.WebhooksApplied)
		}
	}
	maintain()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			maintain()
		}
	}
}
