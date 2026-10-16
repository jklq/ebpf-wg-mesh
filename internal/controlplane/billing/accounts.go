package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"ebof-wg-mesh/internal/sqlretry"

	"github.com/google/uuid"
)

// Account states. Conditions beyond state (budgets, unpaid debt,
// cancellation) live on restrictions and statements, never folded into a
// single flag.
const (
	AccountTrial             = "trial"
	AccountPendingActivation = "pending_activation"
	AccountActive            = "active"
	AccountPastDue           = "past_due"
	AccountCancelled         = "cancelled"
)

// Account is one workspace's billing account.
type Account struct {
	ID                string
	WorkspaceID       string
	StripeCustomerID  string
	Currency          string
	PlanID            string
	PendingPlanID     string
	State             string
	AnchorAt          time.Time
	TrialEndsAt       sql.NullTime
	CancelEffectiveAt sql.NullTime
	BudgetStopCents   sql.NullInt64
	PaymentReady      bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Period kinds.
const (
	PeriodStandard    = "standard"
	PeriodStorageOnly = "storage_only"
)

// Period is one account billing period with its minimum, allowance, and
// rate version pinned at creation.
type Period struct {
	ID                  string
	AccountID           string
	Index               int64
	Kind                string
	PlanID              string
	StartsAt            time.Time
	EndsAt              time.Time
	MonthlyMinimumCents int64
	IncludedUsageCents  int64
	Currency            string
	RateVersion         int64
	PrepaidCents        int64
	State               string
}

const accountColumns = `id, workspace_id, stripe_customer_id, currency, plan_id, pending_plan_id,
	state, anchor_at, trial_ends_at, cancel_effective_at, budget_stop_cents, payment_ready, created_at, updated_at`

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.WorkspaceID, &a.StripeCustomerID, &a.Currency, &a.PlanID, &a.PendingPlanID,
		&a.State, &a.AnchorAt, &a.TrialEndsAt, &a.CancelEffectiveAt, &a.BudgetStopCents, &a.PaymentReady, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

const periodColumns = `id, account_id, period_index, kind, plan_id, starts_at, ends_at,
	monthly_minimum_cents, included_usage_cents, currency, rate_version, prepaid_cents, state`

func scanPeriod(row interface{ Scan(...any) error }) (Period, error) {
	var p Period
	err := row.Scan(&p.ID, &p.AccountID, &p.Index, &p.Kind, &p.PlanID, &p.StartsAt, &p.EndsAt,
		&p.MonthlyMinimumCents, &p.IncludedUsageCents, &p.Currency, &p.RateVersion, &p.PrepaidCents, &p.State)
	return p, err
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func accountByWorkspace(ctx context.Context, q querier, workspaceID string) (Account, error) {
	account, err := scanAccount(q.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM billing_accounts WHERE workspace_id = $1`, workspaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNoAccount
	}
	return account, err
}

func accountByID(ctx context.Context, q querier, accountID string) (Account, error) {
	account, err := scanAccount(q.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM billing_accounts WHERE id = $1`, accountID))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNoAccount
	}
	return account, err
}

// EnsureAccounts creates a trial billing account (with its first period)
// for every workspace that lacks one. Trial credit goes to a user once
// ever: the first workspace (by creation) whose owner has no recorded
// trial grant receives the credit and a budget stop at the credit amount;
// later workspaces of the same user start with no credit and stay
// restricted until paid activation (see EvaluateRestrictions). Managed
// platform projects have no workspace and stay exempt by construction.
func (s *Service) EnsureAccounts(ctx context.Context) (int, error) {
	if s.cfg.Mode == ModeDisabled {
		return 0, nil
	}
	created := 0
	err := sqlretry.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		created = 0
		rows, err := tx.QueryContext(ctx, `SELECT w.id, w.created_at, w.personal_user_id FROM workspaces w
			LEFT JOIN billing_accounts a ON a.workspace_id = w.id WHERE a.id IS NULL ORDER BY w.created_at, w.id`)
		if err != nil {
			return err
		}
		type pending struct {
			id             string
			createdAt      time.Time
			personalUserID sql.NullString
		}
		var missing []pending
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.id, &p.createdAt, &p.personalUserID); err != nil {
				rows.Close()
				return err
			}
			missing = append(missing, p)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		now := s.now().UTC()
		for _, ws := range missing {
			anchor := ws.createdAt.UTC()
			account := Account{
				ID: uuid.NewString(), WorkspaceID: ws.id, Currency: Currency,
				PlanID: PlanTrial, State: AccountTrial, AnchorAt: anchor,
				TrialEndsAt: sql.NullTime{Time: anchor.Add(TrialDuration), Valid: true},
				CreatedAt:   now, UpdatedAt: now,
			}
			result, err := tx.ExecContext(ctx, `INSERT INTO billing_accounts(`+accountColumns+`)
				VALUES ($1,$2,'',$3,$4,'',$5,$6,$7,NULL,NULL,FALSE,$8,$8)
				ON CONFLICT (workspace_id) DO NOTHING`,
				account.ID, account.WorkspaceID, account.Currency, account.PlanID, account.State,
				account.AnchorAt, account.TrialEndsAt, now)
			if err != nil {
				return fmt.Errorf("create billing account for workspace %s: %w", ws.id, err)
			}
			if affected, err := result.RowsAffected(); err != nil {
				return err
			} else if affected == 0 {
				continue // a concurrent pass created this account
			}
			owner, err := workspaceOwnerTx(ctx, tx, ws.id, ws.personalUserID)
			if err != nil {
				return err
			}
			if owner != "" {
				granted, err := claimTrialGrantTx(ctx, tx, owner, account, now)
				if err != nil {
					return err
				}
				if granted {
					// The budget stop at the credit amount is the
					// backstop that bounds trial spend through the
					// normal restriction path.
					if _, err := tx.ExecContext(ctx, `UPDATE billing_accounts SET budget_stop_cents = $2 WHERE id = $1`,
						account.ID, TrialCreditCents); err != nil {
						return err
					}
					if err := grantCreditTx(ctx, tx, account.ID, "grant", TrialCreditCents, account.TrialEndsAt.Time,
						"trial-grant:"+account.ID, "initial trial credit", now); err != nil {
						return err
					}
				}
			}
			if _, err := ensurePeriodTx(ctx, tx, account, 0, now); err != nil {
				return err
			}
			created++
		}
		return nil
	})
	return created, err
}

// workspaceOwnerTx resolves the user a workspace's trial eligibility is
// charged to: the personal workspace's user, or a shared workspace's
// earliest admin (its creator). Empty when neither exists.
func workspaceOwnerTx(ctx context.Context, tx *sql.Tx, workspaceID string, personalUserID sql.NullString) (string, error) {
	if personalUserID.Valid && personalUserID.String != "" {
		return personalUserID.String, nil
	}
	var owner sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT user_id FROM workspace_memberships
		WHERE workspace_id = $1 AND role = 'admin' ORDER BY created_at, user_id LIMIT 1`, workspaceID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return owner.String, err
}

// claimTrialGrantTx records the user's one trial grant for this account,
// reporting whether this workspace won it. The primary key makes the claim
// atomic across concurrent account provisioning.
func claimTrialGrantTx(ctx context.Context, tx *sql.Tx, userID string, account Account, now time.Time) (bool, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO billing_trial_grants(user_id, workspace_id, account_id, granted_at)
		VALUES ($1, $2, $3, $4) ON CONFLICT (user_id) DO NOTHING`,
		userID, account.WorkspaceID, account.ID, now)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// ensurePeriodTx returns the account's period with the given index, creating
// it pinned to the account's effective plan for that index. The first period
// uses the trial plan; a pending plan change applies from the next period.
func ensurePeriodTx(ctx context.Context, tx *sql.Tx, account Account, index int64, now time.Time) (Period, error) {
	period, err := scanPeriod(tx.QueryRowContext(ctx, `SELECT `+periodColumns+` FROM billing_periods WHERE account_id = $1 AND period_index = $2`, account.ID, index))
	if err == nil {
		return period, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Period{}, err
	}
	planID := account.PlanID
	kind := PeriodStandard
	if account.State == AccountCancelled {
		planID, kind = PlanStorage, PeriodStorageOnly
	} else if index > 0 && account.PendingPlanID != "" {
		planID = account.PendingPlanID
	}
	plan, ok := Plans[planID]
	if !ok {
		return Period{}, fmt.Errorf("%w: unknown plan %q", ErrInvalidInput, planID)
	}
	period = Period{
		ID: uuid.NewString(), AccountID: account.ID, Index: index, Kind: kind, PlanID: planID,
		StartsAt: AddAnchorMonths(account.AnchorAt, int(index)), EndsAt: AddAnchorMonths(account.AnchorAt, int(index)+1),
		MonthlyMinimumCents: plan.MonthlyMinimumCents, IncludedUsageCents: plan.IncludedUsageCents,
		Currency: account.Currency, RateVersion: 1, State: "open",
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO billing_periods(`+periodColumns+`, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'open',$13)
		ON CONFLICT (account_id, period_index) DO NOTHING`,
		period.ID, period.AccountID, period.Index, period.Kind, period.PlanID, period.StartsAt, period.EndsAt,
		period.MonthlyMinimumCents, period.IncludedUsageCents, period.Currency, period.RateVersion, period.PrepaidCents, now); err != nil {
		return Period{}, fmt.Errorf("create billing period: %w", err)
	}
	// A concurrent creation wins; return the stored row.
	return scanPeriod(tx.QueryRowContext(ctx, `SELECT `+periodColumns+` FROM billing_periods WHERE account_id = $1 AND period_index = $2`, account.ID, index))
}

// currentPeriodTx returns the open period containing now, creating it (and
// applying any pending plan change) when the previous period has closed.
func currentPeriodTx(ctx context.Context, tx *sql.Tx, account Account, now time.Time) (Period, error) {
	var index int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(period_index), -1) FROM billing_periods WHERE account_id = $1`, account.ID).Scan(&index)
	if err != nil {
		return Period{}, err
	}
	if index < 0 {
		return ensurePeriodTx(ctx, tx, account, 0, now)
	}
	period, err := scanPeriod(tx.QueryRowContext(ctx, `SELECT `+periodColumns+` FROM billing_periods WHERE account_id = $1 AND period_index = $2`, account.ID, index))
	if err != nil {
		return Period{}, err
	}
	if period.State == "open" {
		return period, nil
	}
	next, err := ensurePeriodTx(ctx, tx, account, index+1, now)
	if err != nil {
		return Period{}, err
	}
	// The pending plan becomes current once its period exists.
	if account.PendingPlanID != "" && next.PlanID == account.PendingPlanID {
		if _, err := tx.ExecContext(ctx, `UPDATE billing_accounts SET plan_id = $2, pending_plan_id = '', updated_at = $3 WHERE id = $1`,
			account.ID, account.PendingPlanID, now); err != nil {
			return Period{}, err
		}
	}
	return next, nil
}

// AccountForWorkspace ensures and returns the workspace's account and its
// current open period.
func (s *Service) AccountForWorkspace(ctx context.Context, workspaceID string) (Account, Period, error) {
	if s.cfg.Mode == ModeDisabled {
		return Account{}, Period{}, ErrDisabled
	}
	if _, err := s.EnsureAccounts(ctx); err != nil {
		return Account{}, Period{}, err
	}
	var account Account
	var period Period
	err := sqlretry.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		var err error
		account, err = accountByWorkspace(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		period, err = currentPeriodTx(ctx, tx, account, s.now().UTC())
		return err
	})
	return account, period, err
}

// ChangePlan schedules a plan change for the next period (version one has
// no mid-period plan changes), except leaving trial in enabled mode, which
// starts paid activation instead. In shadow mode a trial exit also just
// schedules, because shadow never activates paid service.
func (s *Service) ChangePlan(ctx context.Context, workspaceID, planID string) error {
	planID = strings.TrimSpace(planID)
	if planID == PlanTrial || planID == PlanStorage {
		return fmt.Errorf("%w: plan %q cannot be selected", ErrInvalidInput, planID)
	}
	if _, ok := Plans[planID]; !ok {
		return fmt.Errorf("%w: unknown plan %q", ErrInvalidInput, planID)
	}
	return sqlretry.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		account, err := accountByWorkspace(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		if account.State == AccountCancelled {
			return fmt.Errorf("%w: account is cancelled", ErrInvalidInput)
		}
		_, err = tx.ExecContext(ctx, `UPDATE billing_accounts SET pending_plan_id = $2, updated_at = $3 WHERE id = $1`,
			account.ID, planID, s.now().UTC())
		return err
	})
}

// SetBudget sets the gross-usage stop threshold in cents. Zero clears it.
func (s *Service) SetBudget(ctx context.Context, workspaceID string, stopCents int64) error {
	if stopCents < 0 {
		return fmt.Errorf("%w: budget must not be negative", ErrInvalidInput)
	}
	return sqlretry.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		account, err := accountByWorkspace(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		var budget sql.NullInt64
		if stopCents > 0 {
			budget = sql.NullInt64{Int64: stopCents, Valid: true}
		}
		_, err = tx.ExecContext(ctx, `UPDATE billing_accounts SET budget_stop_cents = $2, updated_at = $3 WHERE id = $1`,
			account.ID, budget, s.now().UTC())
		return err
	})
}

// Cancel schedules account cancellation at the current period's end.
// Allocations stop at the effective time; retained volumes continue on
// storage-only periods until deleted. Restore is a plan change before the
// effective time (ChangePlan clears nothing else; see docs).
func (s *Service) Cancel(ctx context.Context, workspaceID string) (time.Time, error) {
	var effective time.Time
	err := sqlretry.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		account, err := accountByWorkspace(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		period, err := currentPeriodTx(ctx, tx, account, s.now().UTC())
		if err != nil {
			return err
		}
		effective = period.EndsAt
		_, err = tx.ExecContext(ctx, `UPDATE billing_accounts
			SET state = 'cancelled', cancel_effective_at = $2, pending_plan_id = '', updated_at = $3 WHERE id = $1`,
			account.ID, effective, s.now().UTC())
		return err
	})
	return effective, err
}
