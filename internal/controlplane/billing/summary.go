package billing

import (
	"context"
	"database/sql"
	"math/big"
	"time"

	"ebof-wg-mesh/internal/sqlretry"
)

// ProjectCost is one project's share of a rated window.
type ProjectCost struct {
	ProjectID   string
	ProjectName string
	Meter       Meter
	Quantity    string
	AmountCents int64
}

// Summary is the current-period account view. ForecastGrossCents assumes the
// trailing 24 hours of hourly burn continue for the rest of the period; it
// is an estimate only and never feeds credits, restrictions, or invoices.
type Summary struct {
	Account             Account
	Period              Period
	Restriction         Restriction
	GrossCents          int64
	AfterAllowanceCents int64
	CreditBalanceCents  int64
	EstimatedTotalCents int64
	ForecastGrossCents  int64
	UnpaidCents         int64
	Coverage            Coverage
	Watermark           sql.NullTime
	ProjectCosts        []ProjectCost
	Rates               RateVersion
}

// AccountSummary rates the current open period to date for one workspace.
func (s *Service) AccountSummary(ctx context.Context, workspaceID string) (Summary, error) {
	if s.cfg.Mode == ModeDisabled {
		return Summary{}, ErrDisabled
	}
	account, _, err := s.AccountForWorkspace(ctx, workspaceID)
	if err != nil {
		return Summary{}, err
	}
	now := s.now().UTC()
	var summary Summary
	summary.Account = account
	err = sqlretry.ExecuteTx(ctx, s.db, &sql.TxOptions{ReadOnly: true}, func(tx *sql.Tx) error {
		period, err := scanPeriod(tx.QueryRowContext(ctx, `SELECT `+periodColumns+` FROM billing_periods
			WHERE account_id = $1 AND state = 'open' ORDER BY period_index DESC LIMIT 1`, account.ID))
		if err != nil {
			return err
		}
		summary.Period = period
		rates, err := loadRatesTx(ctx, tx, period.RateVersion, period.Currency)
		if err != nil {
			return err
		}
		summary.Rates = rates
		storageOnly := period.Kind == PeriodStorageOnly
		rated, err := s.rateWindowTx(ctx, tx, workspaceID, rates, period.StartsAt, minTime(now, period.EndsAt), storageOnly)
		if err != nil {
			return err
		}
		creditBalance, err := s.creditBalanceTx(ctx, tx, account.ID, now)
		if err != nil {
			return err
		}
		totals := ComputeTotals(rated.GrossExact, period.MonthlyMinimumCents, period.IncludedUsageCents, creditBalance)
		summary.GrossCents = totals.GrossCents
		summary.AfterAllowanceCents = totals.AfterAllowanceCents
		summary.CreditBalanceCents = creditBalance
		summary.EstimatedTotalCents = totals.PeriodTotalCents
		summary.Coverage = rated.Coverage

		exact := make([]*big.Rat, len(rated.Lines))
		for i, line := range rated.Lines {
			exact[i] = line.ExactCents
		}
		allocated := AllocateCents(exact, totals.GrossCents)
		for i, line := range rated.Lines {
			summary.ProjectCosts = append(summary.ProjectCosts, ProjectCost{
				ProjectID: line.ProjectID, ProjectName: line.ProjectName, Meter: line.Meter,
				Quantity: line.Quantity.String(), AmountCents: allocated[i],
			})
		}

		summary.ForecastGrossCents = totals.GrossCents
		if remaining := period.EndsAt.Sub(now); remaining > 0 {
			burnFrom := now.Add(-24 * time.Hour)
			if burnFrom.Before(period.StartsAt) {
				burnFrom = period.StartsAt
			}
			if window := now.Sub(burnFrom); window >= time.Hour {
				recent, err := s.rateWindowTx(ctx, tx, workspaceID, rates, burnFrom, now, storageOnly)
				if err != nil {
					return err
				}
				burnPerHour := new(big.Rat).Quo(recent.GrossExact, big.NewRat(int64(window/time.Second), 3600))
				projected := new(big.Rat).Mul(burnPerHour, big.NewRat(int64(remaining/time.Second), 3600))
				summary.ForecastGrossCents = totals.GrossCents + RoundHalfUpCents(projected)
			}
		}

		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum(amount_due_cents), 0) FROM billing_statements
			WHERE account_id = $1 AND state IN ('finalized', 'open', 'payment_failed')`, account.ID).Scan(&summary.UnpaidCents); err != nil {
			return err
		}
		summary.Watermark, err = watermarkTx(ctx, tx, workspaceID)
		return err
	})
	if err != nil {
		return Summary{}, err
	}
	restriction, _, err := s.RestrictionForWorkspace(ctx, workspaceID)
	if err != nil {
		return Summary{}, err
	}
	summary.Restriction = restriction
	return summary, nil
}

func (s *Service) creditBalanceTx(ctx context.Context, tx *sql.Tx, accountID string, now time.Time) (int64, error) {
	grants, err := grantBalancesTx(ctx, tx, accountID, now)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, g := range grants {
		total += g.remaining
	}
	return total, nil
}
