package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"ebof-wg-mesh/internal/controlplane/durablework"
	"ebof-wg-mesh/internal/sqlretry"

	"github.com/google/uuid"
)

// WorkKindInvoice is the durable-work kind that settles one frozen
// statement with Stripe. Statement IDs are the resource IDs.
const WorkKindInvoice = "billing_invoice"

// errProviderMismatch blocks collection: Stripe's view of an invoice does
// not match the frozen statement, and retrying cannot fix that.
var errProviderMismatch = errors.New("billing: stripe invoice does not match statement")

// CollectionPaused reports the persisted kill switch state. Pausing stops
// invoice finalization and collection; measurement, rating, statements,
// and reconciliation keep running.
func (s *Service) CollectionPaused(ctx context.Context) (bool, string, error) {
	var paused bool
	var reason string
	err := s.db.QueryRowContext(ctx, `SELECT collection_paused, pause_reason FROM billing_control WHERE singleton`).Scan(&paused, &reason)
	return paused, reason, err
}

// SetCollectionPaused flips the kill switch.
func (s *Service) SetCollectionPaused(ctx context.Context, paused bool, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE billing_control SET collection_paused = $1, pause_reason = $2, updated_at = $3 WHERE singleton`,
		paused, reason, s.now().UTC())
	return err
}

func (s *Service) requireStripe() error {
	if s.stripe == nil {
		return ErrStripeNotConfigured
	}
	return nil
}

// StartPaymentSetup creates (or reuses) the Stripe customer and returns a
// hosted Checkout URL in setup mode for saving an off-session payment
// method. A saved card alone never activates paid service.
func (s *Service) StartPaymentSetup(ctx context.Context, workspaceID string) (string, error) {
	if err := s.requireStripe(); err != nil {
		return "", err
	}
	if s.cfg.CheckoutSuccessURL == "" || s.cfg.CheckoutCancelURL == "" {
		return "", fmt.Errorf("%w: billing checkout URLs are not configured", ErrStripeNotConfigured)
	}
	account, _, err := s.AccountForWorkspace(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	customerID, err := s.ensureCustomer(ctx, account)
	if err != nil {
		return "", err
	}
	_, url, err := s.stripe.CreateCheckoutSetup(ctx, customerID, account.ID,
		s.cfg.CheckoutSuccessURL, s.cfg.CheckoutCancelURL, "checkout:"+account.ID+":"+uuid.NewString())
	return url, err
}

func (s *Service) ensureCustomer(ctx context.Context, account Account) (string, error) {
	if account.StripeCustomerID != "" {
		return account.StripeCustomerID, nil
	}
	customerID, err := s.stripe.CreateCustomer(ctx, account.ID, account.WorkspaceID, "customer:"+account.ID)
	if err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE billing_accounts SET stripe_customer_id = $2, updated_at = $3
		WHERE id = $1 AND stripe_customer_id = ''`, account.ID, customerID, s.now().UTC()); err != nil {
		return "", err
	}
	fresh, err := accountByID(ctx, s.db, account.ID)
	if err != nil {
		return "", err
	}
	return fresh.StripeCustomerID, nil
}

// StartPaidActivation begins leaving trial for a paid plan (enabled mode).
// With a saved payment method it immediately issues the activation
// statement for the first minimum; the account activates only when that
// invoice is paid. Without one, the checkout webhook continues the flow.
func (s *Service) StartPaidActivation(ctx context.Context, workspaceID, planID string) error {
	if s.cfg.Mode != ModeEnabled {
		return fmt.Errorf("%w: paid activation requires enabled billing mode", ErrInvalidInput)
	}
	if err := s.requireStripe(); err != nil {
		return err
	}
	if planID == PlanTrial || planID == PlanStorage {
		return fmt.Errorf("%w: plan %q cannot be selected", ErrInvalidInput, planID)
	}
	if _, ok := Plans[planID]; !ok {
		return fmt.Errorf("%w: unknown plan %q", ErrInvalidInput, planID)
	}
	account, period, err := s.AccountForWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	if account.State != AccountTrial && account.State != AccountPendingActivation {
		// Already paid: a plan change just schedules for the next period.
		return s.ChangePlan(ctx, workspaceID, planID)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE billing_accounts
		SET state = 'pending_activation', pending_plan_id = $2, updated_at = $3 WHERE id = $1`,
		account.ID, planID, s.now().UTC()); err != nil {
		return err
	}
	if !account.PaymentReady {
		return nil
	}
	return s.createActivationStatement(ctx, account.ID, period.ID, planID)
}

// createActivationStatement converts the trial period to the paid plan and
// freezes an activation statement for its minimum. Payment of that
// statement marks the minimum prepaid and activates the account; unused
// trial credit keeps its original expiry.
func (s *Service) createActivationStatement(ctx context.Context, accountID, periodID, planID string) error {
	plan := Plans[planID]
	now := s.now().UTC()
	return sqlretry.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		if _, err := scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM billing_accounts WHERE id = $1 FOR UPDATE`, accountID)); err != nil {
			return err
		}
		period, err := scanPeriod(tx.QueryRowContext(ctx, `SELECT `+periodColumns+` FROM billing_periods WHERE id = $1 FOR UPDATE`, periodID))
		if err != nil {
			return err
		}
		if period.State != "open" {
			return fmt.Errorf("%w: activation period is not open", ErrInvalidInput)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_periods
			SET plan_id = $2, monthly_minimum_cents = $3, included_usage_cents = $4 WHERE id = $1`,
			period.ID, planID, plan.MonthlyMinimumCents, plan.IncludedUsageCents); err != nil {
			return err
		}
		if _, err := statementByPeriodKindTx(ctx, tx, period.ID, StatementActivation); err == nil {
			return nil // already issued
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		statementID := uuid.NewString()
		if _, err := tx.ExecContext(ctx, `INSERT INTO billing_statements(`+statementColumns+`)
			VALUES ($1,$2,$3,'activation',$4,FALSE,'finalized',0,$5,0,0,$6,0,0,0,$6,$6,'{}','','',NULL,$7,$7,$7)`,
			statementID, accountID, period.ID, Currency, plan.IncludedUsageCents, plan.MonthlyMinimumCents, now); err != nil {
			return fmt.Errorf("insert activation statement: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO billing_statement_lines
			(id, statement_id, line_index, meter, description, amount_cents, period_start, period_end)
			VALUES ($1,$2,0,'minimum','Monthly minimum (first period)',$3,$4,$5)`,
			uuid.NewString(), statementID, plan.MonthlyMinimumCents, period.StartsAt, period.EndsAt); err != nil {
			return err
		}
		if s.work != nil {
			_, err = s.work.EnqueueTx(ctx, tx, durablework.EnqueueParams{
				Kind: WorkKindInvoice, DedupKey: "billing-invoice:" + statementID,
				ResourceType: "billing_statement", ResourceID: statementID,
			})
			return err
		}
		return nil
	})
}

// ProcessInvoice settles one finalized statement with Stripe. Every step
// re-reads the statement state first, because the work queue may retry a
// completed operation; queue deduplication alone never prevents a charge.
func (s *Service) ProcessInvoice(ctx context.Context, statementID string) error {
	if err := s.requireStripe(); err != nil {
		return err
	}
	if s.cfg.Mode != ModeEnabled {
		return ErrCollectionPaused
	}
	if paused, _, err := s.CollectionPaused(ctx); err != nil {
		return err
	} else if paused {
		return ErrCollectionPaused
	}
	statement, account, err := s.statementAndAccount(ctx, statementID)
	if err != nil {
		return err
	}
	switch statement.State {
	case StatementPaid, StatementWaived, StatementVoid, StatementCarried, StatementShadow, StatementAccumulating:
		return nil
	case StatementFinalized, StatementOpen, StatementPaymentFailed:
	default:
		return fmt.Errorf("billing: statement %s in unexpected state %q", statementID, statement.State)
	}
	if statement.StripeInvoiceID != "" && statement.State != StatementFinalized {
		// Already issued; webhooks and reconciliation own its state now.
		return s.syncInvoice(ctx, statement)
	}

	customerID, err := s.ensureCustomer(ctx, account)
	if err != nil {
		return err
	}
	if !account.PaymentReady {
		return fmt.Errorf("billing: account %s has no saved payment method", account.ID)
	}

	invoiceID := statement.StripeInvoiceID
	if invoiceID == "" {
		if statement.ProviderAttemptAt.Valid {
			found, ok, err := s.stripe.FindInvoiceByStatement(ctx, customerID, statement.ID)
			if err != nil {
				return err
			}
			if ok {
				invoiceID = found.ID
				if err := s.persistInvoiceID(ctx, statement.ID, found); err != nil {
					return err
				}
			}
		}
		if invoiceID == "" {
			if !statement.ProviderAttemptAt.Valid {
				if _, err := s.db.ExecContext(ctx, `UPDATE billing_statements SET provider_attempt_at = $2, updated_at = $2 WHERE id = $1`,
					statement.ID, s.now().UTC()); err != nil {
					return err
				}
			}
			created, err := s.stripe.CreateDraftInvoice(ctx, customerID, statement.ID, "statement:"+statement.ID+":invoice")
			if err != nil {
				return err
			}
			invoiceID = created.ID
			if err := s.persistInvoiceID(ctx, statement.ID, created); err != nil {
				return err
			}
		}
	}

	for _, line := range statement.Lines {
		if line.AmountCents == 0 || line.StripeInvoiceItemID != "" {
			continue
		}
		req := InvoiceItemRequest{
			LineID: line.ID, StatementID: statement.ID, CustomerID: customerID, InvoiceID: invoiceID,
			Description:    lineDescription(line),
			AmountCents:    line.AmountCents,
			IdempotencyKey: fmt.Sprintf("statement:%s:line:%d", statement.ID, line.Index),
		}
		if line.PeriodStart.Valid {
			req.PeriodStart = line.PeriodStart.Time
			req.PeriodEnd = line.PeriodEnd.Time
		}
		itemID, err := s.stripe.AddInvoiceItem(ctx, req)
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE billing_statement_lines SET stripe_invoice_item_id = $2 WHERE id = $1`, line.ID, itemID); err != nil {
			return err
		}
	}

	draft, err := s.stripe.RetrieveInvoice(ctx, invoiceID)
	if err != nil {
		return err
	}
	if err := verifyDraft(statement, customerID, draft); err != nil {
		s.alarm(ctx, "billing_provider_mismatch", statement.ID, err.Error(), "inspect the statement and Stripe invoice before retrying")
		return err
	}
	finalized, err := s.stripe.FinalizeInvoice(ctx, invoiceID, "statement:"+statement.ID+":finalize")
	if err != nil {
		return err
	}
	return s.applyInvoiceSnapshot(ctx, statement.ID, finalized)
}

func verifyDraft(statement Statement, customerID string, draft InvoiceSnapshot) error {
	switch {
	case draft.CustomerID != "" && draft.CustomerID != customerID:
		return fmt.Errorf("%w: customer %s, want %s", errProviderMismatch, draft.CustomerID, customerID)
	case draft.Currency != "" && draft.Currency != StripeCurrency:
		return fmt.Errorf("%w: currency %s", errProviderMismatch, draft.Currency)
	case draft.StatementID != "" && draft.StatementID != statement.ID:
		return fmt.Errorf("%w: statement metadata %s", errProviderMismatch, draft.StatementID)
	case draft.Status == "draft" && draft.AmountDueCents != statement.AmountDueCents:
		return fmt.Errorf("%w: draft amount %d cents, statement due %d cents", errProviderMismatch, draft.AmountDueCents, statement.AmountDueCents)
	}
	return nil
}

func lineDescription(line StatementLine) string {
	if line.ProjectName != "" {
		return line.ProjectName + " — " + line.Description
	}
	return line.Description
}

func (s *Service) statementAndAccount(ctx context.Context, statementID string) (Statement, Account, error) {
	var statement Statement
	var account Account
	err := sqlretry.ExecuteTx(ctx, s.db, &sql.TxOptions{ReadOnly: true}, func(tx *sql.Tx) error {
		var err error
		statement, err = scanStatement(tx.QueryRowContext(ctx, `SELECT `+statementColumns+` FROM billing_statements WHERE id = $1`, statementID))
		if err != nil {
			return err
		}
		statement.Lines, err = loadLinesTx(ctx, tx, statement.ID)
		if err != nil {
			return err
		}
		account, err = accountByID(ctx, tx, statement.AccountID)
		return err
	})
	return statement, account, err
}

func (s *Service) persistInvoiceID(ctx context.Context, statementID string, snap InvoiceSnapshot) error {
	_, err := s.db.ExecContext(ctx, `UPDATE billing_statements SET stripe_invoice_id = $2, stripe_hosted_invoice_url = $3, updated_at = $4
		WHERE id = $1 AND stripe_invoice_id = ''`, statementID, snap.ID, snap.HostedURL, s.now().UTC())
	return err
}

// applyInvoiceSnapshot folds a provider invoice state into the statement.
// Paid is terminal: an older failure event never regresses it.
func (s *Service) applyInvoiceSnapshot(ctx context.Context, statementID string, snap InvoiceSnapshot) error {
	return sqlretry.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		return s.applyInvoiceSnapshotTx(ctx, tx, statementID, snap)
	})
}

func (s *Service) applyInvoiceSnapshotTx(ctx context.Context, tx *sql.Tx, statementID string, snap InvoiceSnapshot) error {
	statement, err := scanStatement(tx.QueryRowContext(ctx, `SELECT `+statementColumns+` FROM billing_statements WHERE id = $1 FOR UPDATE`, statementID))
	if err != nil {
		return err
	}
	if statement.State == StatementPaid || statement.State == StatementWaived || statement.State == StatementVoid {
		return nil
	}
	next := statement.State
	switch snap.Status {
	case "paid":
		next = StatementPaid
	case "open":
		next = StatementOpen
	case "void", "uncollectible":
		next = StatementVoid
	case "draft":
		next = StatementFinalized
	}
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE billing_statements SET state = $2, stripe_hosted_invoice_url = $3, updated_at = $4 WHERE id = $1`,
		statement.ID, next, snap.HostedURL, now); err != nil {
		return err
	}
	if next == StatementPaid {
		return s.onStatementPaidTx(ctx, tx, statement, now)
	}
	return nil
}

// onStatementPaidTx runs payment side effects: an activation statement
// activates the account and marks its period minimum prepaid; any payment
// clears past-due once no failed statements remain.
func (s *Service) onStatementPaidTx(ctx context.Context, tx *sql.Tx, statement Statement, now time.Time) error {
	if statement.Kind == StatementActivation {
		if _, err := tx.ExecContext(ctx, `UPDATE billing_periods SET prepaid_cents = $2 WHERE id = $1`,
			statement.PeriodID, statement.MinimumCents); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_accounts
			SET state = 'active', plan_id = pending_plan_id, pending_plan_id = '', updated_at = $2
			WHERE id = $1 AND state = 'pending_activation' AND pending_plan_id <> ''`, statement.AccountID, now); err != nil {
			return err
		}
	}
	var failed int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM billing_statements WHERE account_id = $1 AND state = 'payment_failed'`,
		statement.AccountID).Scan(&failed); err != nil {
		return err
	}
	if failed == 0 {
		_, err := tx.ExecContext(ctx, `UPDATE billing_accounts SET state = 'active', updated_at = $2
			WHERE id = $1 AND state = 'past_due'`, statement.AccountID, now)
		return err
	}
	return nil
}

func (s *Service) syncInvoice(ctx context.Context, statement Statement) error {
	snap, err := s.stripe.RetrieveInvoice(ctx, statement.StripeInvoiceID)
	if err != nil {
		return err
	}
	if snap.AmountDueCents != 0 && statement.State != StatementPaid && snap.AmountDueCents != statement.AmountDueCents && snap.AmountPaidCents == 0 {
		s.alarm(ctx, "billing_provider_mismatch", statement.ID,
			fmt.Sprintf("stripe invoice %s amount due %d does not match statement %d", snap.ID, snap.AmountDueCents, statement.AmountDueCents),
			"inspect the statement and Stripe invoice")
	}
	return s.applyInvoiceSnapshot(ctx, statement.ID, snap)
}

// Reconcile syncs every unsettled statement that has a provider invoice.
// It covers missing webhooks; mismatches alarm instead of mutating amounts.
func (s *Service) Reconcile(ctx context.Context) error {
	if s.cfg.Mode != ModeEnabled || s.stripe == nil {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, stripe_invoice_id FROM billing_statements
		WHERE stripe_invoice_id <> '' AND state IN ('finalized', 'open', 'payment_failed') ORDER BY created_at LIMIT 100`)
	if err != nil {
		return err
	}
	type pending struct{ id, invoiceID string }
	var list []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.invoiceID); err != nil {
			rows.Close()
			return err
		}
		list = append(list, p)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, p := range list {
		snap, err := s.stripe.RetrieveInvoice(ctx, p.invoiceID)
		if err != nil {
			return err
		}
		if err := s.applyInvoiceSnapshot(ctx, p.id, snap); err != nil {
			return err
		}
	}
	return nil
}
