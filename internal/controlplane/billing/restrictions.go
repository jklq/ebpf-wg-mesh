package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ebof-wg-mesh/internal/sqlretry"

	"github.com/google/uuid"
)

// Restriction is a workspace's current spend restriction. Restricted stops
// compute; GrowthBlocked only stops new resource consumption. Dry-run rows
// are shadow observations and are never enforced.
type Restriction struct {
	WorkspaceID   string
	AccountID     string
	Restricted    bool
	GrowthBlocked bool
	DryRun        bool
	Reason        string
	Since         sql.NullTime
	UpdatedAt     time.Time
}

// Enforced reports whether delivery must act on this restriction.
func (r Restriction) Enforced() bool { return r.Restricted && !r.DryRun }

// EnforcedGrowth reports whether new resource consumption must be refused.
func (r Restriction) EnforcedGrowth() bool {
	return (r.Restricted || r.GrowthBlocked) && !r.DryRun
}

func scanRestriction(row interface{ Scan(...any) error }) (Restriction, error) {
	var r Restriction
	err := row.Scan(&r.WorkspaceID, &r.AccountID, &r.Restricted, &r.GrowthBlocked, &r.DryRun, &r.Reason, &r.Since, &r.UpdatedAt)
	return r, err
}

const restrictionColumns = `workspace_id, account_id, restricted, growth_blocked, dry_run, reason, since, updated_at`

// RestrictionForWorkspace reads a workspace's restriction, if any.
func (s *Service) RestrictionForWorkspace(ctx context.Context, workspaceID string) (Restriction, bool, error) {
	r, err := scanRestriction(s.db.QueryRowContext(ctx,
		`SELECT `+restrictionColumns+` FROM billing_restrictions WHERE workspace_id = $1`, workspaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return Restriction{WorkspaceID: workspaceID, DryRun: s.cfg.Mode != ModeEnabled}, false, nil
	}
	return r, err == nil, err
}

// CheckGrowth refuses new resource consumption for an enforced restriction.
// Platform handlers call it before releases, restarts, scale increases, and
// volume growth; reads, drafts, payment, export, and deletion never call it.
func (s *Service) CheckGrowth(ctx context.Context, workspaceID string) error {
	if s == nil || s.cfg.Mode != ModeEnabled || workspaceID == "" {
		return nil
	}
	r, found, err := s.RestrictionForWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	if found && r.EnforcedGrowth() {
		return fmt.Errorf("%w: %s", ErrGrowthBlocked, r.Reason)
	}
	return nil
}

// ServiceEnforcedTx reports the enforced compute restriction for a service,
// resolved through its project's workspace inside the caller's transaction.
// Delivery calls it at the allocation-creation seam; managed platform
// services have no workspace and are never restricted.
func ServiceEnforcedTx(ctx context.Context, tx *sql.Tx, serviceID string) (bool, string, error) {
	var restricted bool
	var reason string
	err := tx.QueryRowContext(ctx, `SELECT r.restricted AND NOT r.dry_run, r.reason
		  FROM services s
		  JOIN environments e ON e.id = s.environment_id
		  JOIN projects p ON p.id = e.project_id
		  JOIN billing_restrictions r ON r.workspace_id = p.workspace_id
		 WHERE s.id = $1`, serviceID).Scan(&restricted, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	return restricted, reason, err
}

// ServiceGrowthBlockedTx reports whether new resource consumption (a build
// claim, a release, a scale increase) is refused for a service: an enforced
// restriction, or an enforced growth block such as a failed payment still
// inside its grace period.
func ServiceGrowthBlockedTx(ctx context.Context, tx *sql.Tx, serviceID string) (bool, string, error) {
	var blocked bool
	var reason string
	err := tx.QueryRowContext(ctx, `SELECT (r.restricted OR r.growth_blocked) AND NOT r.dry_run, r.reason
		  FROM services s
		  JOIN environments e ON e.id = s.environment_id
		  JOIN projects p ON p.id = e.project_id
		  JOIN billing_restrictions r ON r.workspace_id = p.workspace_id
		 WHERE s.id = $1`, serviceID).Scan(&blocked, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	return blocked, reason, err
}

// WorkspaceEnforcedTx is ServiceEnforcedTx for callers that already hold a
// workspace scope.
func WorkspaceEnforcedTx(ctx context.Context, tx *sql.Tx, workspaceID string) (bool, string, error) {
	var restricted bool
	var reason string
	err := tx.QueryRowContext(ctx, `SELECT restricted AND NOT dry_run, reason
		FROM billing_restrictions WHERE workspace_id = $1`, workspaceID).Scan(&restricted, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	return restricted, reason, err
}

// thresholdPercents are the budget notice points, evaluated against gross
// usage before allowance, credits, and tax.
var thresholdPercents = []int64{75, 90, 100}

// evaluation is one account's computed restriction inputs.
type evaluation struct {
	restricted    bool
	growthBlocked bool
	reason        string
	grossCents    int64
}

// EvaluateRestrictions recomputes every account's restriction from rated
// gross usage, unpaid statements, trial expiry, and cancellation, records
// threshold notices, and upserts restrictions as journal product state.
func (s *Service) EvaluateRestrictions(ctx context.Context) error {
	if s.cfg.Mode == ModeDisabled {
		return nil
	}
	if _, err := s.EnsureAccounts(ctx); err != nil {
		return err
	}
	accounts, err := s.listAccounts(ctx)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	for _, account := range accounts {
		ev, period, err := s.evaluateAccount(ctx, account, now)
		if err != nil {
			return fmt.Errorf("evaluate billing account %s: %w", account.ID, err)
		}
		if err := s.applyRestriction(ctx, account, period, ev, now); err != nil {
			return fmt.Errorf("apply billing restriction for account %s: %w", account.ID, err)
		}
	}
	return nil
}

func (s *Service) listAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountColumns+` FROM billing_accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, account)
	}
	return out, rows.Err()
}

func (s *Service) evaluateAccount(ctx context.Context, account Account, now time.Time) (evaluation, Period, error) {
	var ev evaluation
	var period Period
	err := sqlretry.ExecuteTx(ctx, s.db, &sql.TxOptions{ReadOnly: true}, func(tx *sql.Tx) error {
		var err error
		period, err = scanPeriod(tx.QueryRowContext(ctx, `SELECT `+periodColumns+` FROM billing_periods
			WHERE account_id = $1 AND state = 'open' ORDER BY period_index DESC LIMIT 1`, account.ID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		rates, err := loadRatesTx(ctx, tx, period.RateVersion, period.Currency)
		if err != nil {
			return err
		}
		rated, err := s.rateWindowTx(ctx, tx, account.WorkspaceID, rates, period.StartsAt, minTime(now, period.EndsAt), period.Kind == PeriodStorageOnly)
		if err != nil {
			return err
		}
		ev.grossCents = RoundHalfUpCents(rated.GrossExact)

		var failedSince sql.NullTime
		if err := tx.QueryRowContext(ctx, `SELECT min(finalized_at) FROM billing_statements
			WHERE account_id = $1 AND state = 'payment_failed'`, account.ID).Scan(&failedSince); err != nil {
			return err
		}

		// Trial gates: the one-trial-per-user grant and the owner's GitHub
		// account age. Accounts that never received the user's grant (a
		// later workspace) stay restricted until paid activation moves
		// them out of the trial states.
		trialAccount := account.State == AccountTrial || account.State == AccountPendingActivation
		trialUsedUp, githubTooNew := false, false
		if trialAccount {
			var grantUser string
			err := tx.QueryRowContext(ctx, `SELECT user_id FROM billing_trial_grants WHERE workspace_id = $1`, account.WorkspaceID).Scan(&grantUser)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				trialUsedUp = true
			case err != nil:
				return err
			default:
				tooNew, err := githubAccountTooNewTx(ctx, tx, grantUser, now)
				if err != nil {
					return err
				}
				githubTooNew = tooNew
			}
		}
		egressOver := account.PlanID == PlanTrial && ratedEgressBytes(rated) > TrialEgressCapBytes

		switch {
		case account.State == AccountCancelled && account.CancelEffectiveAt.Valid && !now.Before(account.CancelEffectiveAt.Time):
			ev.restricted, ev.reason = true, "account cancelled"
		case trialUsedUp:
			ev.restricted, ev.reason = true, "trial credit already used"
		case githubTooNew:
			ev.restricted, ev.reason = true, "github account is less than 30 days old"
		case account.State == AccountTrial && account.TrialEndsAt.Valid && !now.Before(account.TrialEndsAt.Time):
			ev.restricted, ev.reason = true, "trial expired"
		case failedSince.Valid && now.Sub(failedSince.Time) >= PaymentGrace:
			ev.restricted, ev.reason = true, "payment failed and grace period elapsed"
		case egressOver:
			ev.restricted, ev.reason = true, "trial egress limit reached"
		case account.BudgetStopCents.Valid && ev.grossCents >= account.BudgetStopCents.Int64:
			ev.restricted, ev.reason = true, "budget stop threshold reached"
		}
		if failedSince.Valid && !ev.restricted {
			ev.growthBlocked, ev.reason = true, "payment failed"
		}
		return nil
	})
	return ev, period, err
}

// ratedEgressBytes sums the rated public-egress quantities of a period.
func ratedEgressBytes(rated RatedPeriod) int64 {
	var total int64
	for _, line := range rated.Lines {
		if line.Meter != MeterEgress || line.Quantity == nil {
			continue
		}
		if !line.Quantity.IsInt64() {
			return TrialEgressCapBytes + 1
		}
		total += line.Quantity.Int64()
	}
	return total
}

// githubAccountTooNewTx reports whether a GitHub user's recorded account
// age blocks trial compute. Only users with a profile row are GitHub
// signups; a row whose github_created_at is unknown fails closed.
// Bootstrap and dev users have no row and are never gated.
func githubAccountTooNewTx(ctx context.Context, tx *sql.Tx, userID string, now time.Time) (bool, error) {
	var createdAt sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT github_created_at FROM user_profiles WHERE user_id = $1`, userID).Scan(&createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !createdAt.Valid {
		return true, nil
	}
	return now.Before(createdAt.Time.Add(GitHubMinAccountAge)), nil
}

func (s *Service) applyRestriction(ctx context.Context, account Account, period Period, ev evaluation, now time.Time) error {
	dryRun := s.cfg.Mode != ModeEnabled
	run := s.productTx
	if run == nil {
		run = func(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
			return sqlretry.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error { return fn(ctx, tx) })
		}
	}
	changedToRestricted := false
	err := run(ctx, func(ctx context.Context, tx *sql.Tx) error {
		previous, err := scanRestriction(tx.QueryRowContext(ctx,
			`SELECT `+restrictionColumns+` FROM billing_restrictions WHERE workspace_id = $1`, account.WorkspaceID))
		found := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		since := previous.Since
		if ev.restricted && !previous.Restricted {
			since = sql.NullTime{Time: now, Valid: true}
		}
		if !ev.restricted {
			since = sql.NullTime{}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO billing_restrictions(`+restrictionColumns+`)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (workspace_id) DO UPDATE SET account_id = EXCLUDED.account_id,
				restricted = EXCLUDED.restricted, growth_blocked = EXCLUDED.growth_blocked,
				dry_run = EXCLUDED.dry_run, reason = EXCLUDED.reason, since = EXCLUDED.since,
				updated_at = EXCLUDED.updated_at
			WHERE billing_restrictions.restricted <> EXCLUDED.restricted
			   OR billing_restrictions.growth_blocked <> EXCLUDED.growth_blocked
			   OR billing_restrictions.dry_run <> EXCLUDED.dry_run
			   OR billing_restrictions.reason <> EXCLUDED.reason`,
			account.WorkspaceID, account.ID, ev.restricted, ev.growthBlocked, dryRun, ev.reason, since, now); err != nil {
			return err
		}
		changed := !found || previous.Restricted != ev.restricted || previous.GrowthBlocked != ev.growthBlocked || previous.DryRun != dryRun
		changedToRestricted = changed && (ev.restricted || ev.growthBlocked)
		if changed {
			// Nudge delivery: fleet reconciliation revisits services with a
			// placement message, so a restriction change converges within
			// one reconciliation round.
			message := "billing restriction cleared"
			if ev.restricted || ev.growthBlocked {
				message = "billing restriction: " + ev.reason
			}
			if _, err := tx.ExecContext(ctx, `UPDATE service_delivery_status ds SET placement_message = $2, updated_at = $3
				  FROM services svc JOIN environments e ON e.id = svc.environment_id
				  JOIN projects p ON p.id = e.project_id
				 WHERE ds.service_id = svc.id AND p.workspace_id = $1`,
				account.WorkspaceID, message, now); err != nil {
				return err
			}
		}
		if account.BudgetStopCents.Valid && period.ID != "" {
			for _, percent := range thresholdPercents {
				if ev.grossCents*100 < account.BudgetStopCents.Int64*percent {
					continue
				}
				payload, _ := json.Marshal(map[string]any{
					"gross_cents": ev.grossCents, "budget_stop_cents": account.BudgetStopCents.Int64,
					"percent": percent, "dry_run": dryRun,
				})
				if _, err := tx.ExecContext(ctx, `INSERT INTO billing_notices
					(id, account_id, period_id, kind, threshold_percent, dedup_key, payload, created_at)
					VALUES ($1,$2,$3,'budget_threshold',$4,$5,$6,$7) ON CONFLICT (dedup_key) DO NOTHING`,
					uuid.NewString(), account.ID, period.ID, percent,
					fmt.Sprintf("budget:%s:%s:%d", account.ID, period.ID, percent), payload, now); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err == nil && changedToRestricted {
		s.alarm(ctx, "billing_restriction", account.WorkspaceID,
			fmt.Sprintf("workspace %s billing restriction: %s (dry_run=%v)", account.WorkspaceID, ev.reason, dryRun),
			"inspect the billing account")
	}
	return err
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
