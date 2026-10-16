package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ebof-wg-mesh/internal/controlplane/durablework"
	"ebof-wg-mesh/internal/telemetry"

	"github.com/google/uuid"
)

// MaintainResult reports one maintenance pass for logging.
type MaintainResult struct {
	AccountsCreated int
	PeriodsClosed   int
	InvoicesSettled int
	WebhooksApplied int
}

// ValidateLaunchGate enforces the plan's shipping gate: enabled mode may
// only start after at least one full shadow period (28 days or longer) has
// been rated into a statement, so the first real charge is preceded by a
// complete shadow-billing period the operator can reconcile against
// independent measurements.
func (s *Service) ValidateLaunchGate(ctx context.Context) error {
	if s.cfg.Mode != ModeEnabled {
		return nil
	}
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM billing_statements st JOIN billing_periods p ON p.id = st.period_id
		 WHERE st.shadow AND st.kind = 'period_close' AND p.ends_at - p.starts_at >= interval '28 days')`).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("billing: enabled mode requires a completed full shadow period first (run in shadow mode, reconcile its statements, then enable)")
	}
	return nil
}

// Maintain runs one billing pass: provision accounts, close due periods,
// refresh restrictions, settle provider work, apply webhooks, reconcile.
// Every step is transactional and idempotent, so concurrent passes are safe.
func (s *Service) Maintain(ctx context.Context) (MaintainResult, error) {
	var result MaintainResult
	if s.cfg.Mode == ModeDisabled {
		return result, nil
	}
	created, err := s.EnsureAccounts(ctx)
	if err != nil {
		return result, err
	}
	result.AccountsCreated = created

	closed, err := s.closeDuePeriods(ctx)
	if err != nil {
		return result, err
	}
	result.PeriodsClosed = closed

	if err := s.EvaluateRestrictions(ctx); err != nil {
		return result, err
	}

	if s.cfg.Mode == ModeEnabled {
		applied, err := s.ProcessWebhooks(ctx)
		if err != nil {
			return result, err
		}
		result.WebhooksApplied = applied

		settled, err := s.processInvoiceWork(ctx)
		if err != nil {
			return result, err
		}
		result.InvoicesSettled = settled

		if err := s.Reconcile(ctx); err != nil {
			return result, err
		}
	}
	return result, nil
}

// closeDuePeriods closes every open period whose lateness window has
// passed. Usage arriving after finalization stays as retained evidence and
// becomes an explicit credit decision, never a silent statement edit.
func (s *Service) closeDuePeriods(ctx context.Context) (int, error) {
	cutoff := s.now().UTC().Add(-PeriodCloseLateness)
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM billing_periods WHERE state = 'open' AND ends_at <= $1 ORDER BY ends_at LIMIT 100`, cutoff)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		statement, err := s.ClosePeriod(ctx, id)
		if err != nil {
			return 0, fmt.Errorf("close billing period %s: %w", id, err)
		}
		telemetry.Set(ctx, telemetry.String("billing.statement.id", statement.ID),
			telemetry.String("billing.period.id", id))
		telemetry.RecordBillingPeriodClosed(ctx, string(s.cfg.Mode), statement.State, statement.Currency, statement.AmountDueCents)
	}
	return len(ids), nil
}

// processInvoiceWork claims and settles queued invoice work. The worker
// checks statement state inside ProcessInvoice before any provider call.
func (s *Service) processInvoiceWork(ctx context.Context) (int, error) {
	if s.work == nil {
		return 0, nil
	}
	owner := "billing-" + uuid.NewString()
	settled := 0
	for {
		record, err := s.work.Claim(ctx, owner, 2*time.Minute, WorkKindInvoice)
		if err != nil {
			return settled, err
		}
		if record.ID == "" {
			return settled, nil
		}
		processErr := s.ProcessInvoice(ctx, record.ResourceID)
		switch {
		case processErr == nil:
			if err := s.work.Complete(ctx, record); err != nil {
				return settled, err
			}
			telemetry.RecordBillingInvoiceWork(ctx, "settled")
			settled++
		case errors.Is(processErr, ErrCollectionPaused):
			telemetry.RecordBillingInvoiceWork(ctx, "paused")
			if err := s.work.Fail(ctx, record, processErr, durablework.FailOptions{Retryable: true, RetryAfter: 5 * time.Minute}); err != nil {
				return settled, err
			}
			return settled, nil
		case errors.Is(processErr, errProviderMismatch):
			telemetry.RecordBillingInvoiceWork(ctx, "mismatch")
			if err := s.work.Fail(ctx, record, processErr, durablework.FailOptions{Retryable: false}); err != nil {
				return settled, err
			}
		default:
			telemetry.RecordBillingInvoiceWork(ctx, "failed")
			if err := s.work.Fail(ctx, record, processErr, durablework.FailOptions{Retryable: true}); err != nil {
				return settled, err
			}
		}
	}
}
