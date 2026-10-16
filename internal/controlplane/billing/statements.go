package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/durablework"
	"ebof-wg-mesh/internal/sqlretry"

	"github.com/google/uuid"
)

// Statement kinds and states.
const (
	StatementActivation  = "activation"
	StatementPeriodClose = "period_close"

	StatementShadow        = "shadow"
	StatementFinalized     = "finalized"
	StatementOpen          = "open"
	StatementPaid          = "paid"
	StatementPaymentFailed = "payment_failed"
	StatementAccumulating  = "accumulating"
	StatementCarried       = "carried"
	StatementWaived        = "waived"
	StatementVoid          = "void"
)

// Statement is a frozen, rated bill for one period (or a paid activation).
// Finalized statements never change their quantities or amounts; late
// evidence becomes an explicit credit adjustment instead.
type Statement struct {
	ID                     string
	AccountID              string
	PeriodID               string
	Kind                   string
	Currency               string
	Shadow                 bool
	State                  string
	GrossCents             int64
	IncludedCents          int64
	AfterAllowanceCents    int64
	CreditCents            int64
	MinimumCents           int64
	PrepaidAppliedCents    int64
	NextMinimumCents       int64
	CarriedFromCents       int64
	PeriodTotalCents       int64
	AmountDueCents         int64
	Coverage               Coverage
	StripeInvoiceID        string
	StripeHostedInvoiceURL string
	ProviderAttemptAt      sql.NullTime
	FinalizedAt            sql.NullTime
	CreatedAt              time.Time
	UpdatedAt              time.Time
	Lines                  []StatementLine
}

// StatementLine is one frozen statement row. Usage lines carry exact
// quantities and rates; account lines (minimum, allowance, credit, prepaid,
// next minimum, carried balance) explain the period equation.
type StatementLine struct {
	ID                  string
	Index               int64
	ProjectID           string
	ProjectName         string
	Meter               string
	Description         string
	Quantity            string
	Unit                string
	RateNumerator       string
	RateDenominator     string
	AmountCents         int64
	PeriodStart         sql.NullTime
	PeriodEnd           sql.NullTime
	StripeInvoiceItemID string
}

const statementColumns = `id, account_id, period_id, kind, currency, shadow, state, gross_cents, included_cents,
	after_allowance_cents, credit_cents, minimum_cents, prepaid_applied_cents, next_minimum_cents,
	carried_from_cents, period_total_cents, amount_due_cents, coverage, stripe_invoice_id,
	stripe_hosted_invoice_url, provider_attempt_at, finalized_at, created_at, updated_at`

func scanStatement(row interface{ Scan(...any) error }) (Statement, error) {
	var st Statement
	var coverage []byte
	err := row.Scan(&st.ID, &st.AccountID, &st.PeriodID, &st.Kind, &st.Currency, &st.Shadow, &st.State,
		&st.GrossCents, &st.IncludedCents, &st.AfterAllowanceCents, &st.CreditCents, &st.MinimumCents,
		&st.PrepaidAppliedCents, &st.NextMinimumCents, &st.CarriedFromCents, &st.PeriodTotalCents,
		&st.AmountDueCents, &coverage, &st.StripeInvoiceID, &st.StripeHostedInvoiceURL,
		&st.ProviderAttemptAt, &st.FinalizedAt, &st.CreatedAt, &st.UpdatedAt)
	if err != nil {
		return Statement{}, err
	}
	if len(coverage) > 0 {
		_ = json.Unmarshal(coverage, &st.Coverage)
	}
	return st, nil
}

func loadLinesTx(ctx context.Context, tx *sql.Tx, statementID string) ([]StatementLine, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, line_index, project_id, project_name, meter, description,
			quantity::text, unit, rate_numerator::text, rate_denominator::text, amount_cents,
			period_start, period_end, stripe_invoice_item_id
		  FROM billing_statement_lines WHERE statement_id = $1 ORDER BY line_index`, statementID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines []StatementLine
	for rows.Next() {
		var line StatementLine
		if err := rows.Scan(&line.ID, &line.Index, &line.ProjectID, &line.ProjectName, &line.Meter, &line.Description,
			&line.Quantity, &line.Unit, &line.RateNumerator, &line.RateDenominator, &line.AmountCents,
			&line.PeriodStart, &line.PeriodEnd, &line.StripeInvoiceItemID); err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, rows.Err()
}

func statementByPeriodKindTx(ctx context.Context, tx *sql.Tx, periodID, kind string) (Statement, error) {
	st, err := scanStatement(tx.QueryRowContext(ctx, `SELECT `+statementColumns+` FROM billing_statements WHERE period_id = $1 AND kind = $2`, periodID, kind))
	if err != nil {
		return Statement{}, err
	}
	st.Lines, err = loadLinesTx(ctx, tx, st.ID)
	return st, err
}

// ClosePeriod rates and freezes one period exactly once. Concurrent closers
// serialize on the period row and observe the same statement. In enabled
// mode the frozen statement and its invoice work item commit atomically.
func (s *Service) ClosePeriod(ctx context.Context, periodID string) (Statement, error) {
	if s.cfg.Mode == ModeDisabled {
		return Statement{}, ErrDisabled
	}
	var result Statement
	err := sqlretry.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		var err error
		result, err = s.closePeriodTx(ctx, tx, periodID)
		return err
	})
	return result, err
}

func (s *Service) closePeriodTx(ctx context.Context, tx *sql.Tx, periodID string) (Statement, error) {
	now := s.now().UTC()
	var accountID string
	if err := tx.QueryRowContext(ctx, `SELECT account_id FROM billing_periods WHERE id = $1`, periodID).Scan(&accountID); err != nil {
		return Statement{}, err
	}
	// The account lock serializes credit consumption and period creation.
	account, err := scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM billing_accounts WHERE id = $1 FOR UPDATE`, accountID))
	if err != nil {
		return Statement{}, err
	}
	period, err := scanPeriod(tx.QueryRowContext(ctx, `SELECT `+periodColumns+` FROM billing_periods WHERE id = $1 FOR UPDATE`, periodID))
	if err != nil {
		return Statement{}, err
	}
	if period.State == "closed" {
		return statementByPeriodKindTx(ctx, tx, periodID, StatementPeriodClose)
	}

	rates, err := loadRatesTx(ctx, tx, period.RateVersion, period.Currency)
	if err != nil {
		return Statement{}, err
	}
	rated, err := s.rateWindowTx(ctx, tx, account.WorkspaceID, rates, period.StartsAt, period.EndsAt, period.Kind == PeriodStorageOnly)
	if err != nil {
		return Statement{}, err
	}
	grants, err := grantBalancesTx(ctx, tx, account.ID, now)
	if err != nil {
		return Statement{}, err
	}
	var creditAvailable int64
	for _, g := range grants {
		creditAvailable += g.remaining
	}
	totals := ComputeTotals(rated.GrossExact, period.MonthlyMinimumCents, period.IncludedUsageCents, creditAvailable)

	statementID := uuid.NewString()
	applied, err := consumeCreditTx(ctx, tx, account.ID, statementID, totals.CreditAppliedCents, now)
	if err != nil {
		return Statement{}, err
	}
	if applied != totals.CreditAppliedCents {
		totals = ComputeTotals(rated.GrossExact, period.MonthlyMinimumCents, period.IncludedUsageCents, applied)
	}

	// Small balances from earlier statements carry into this one exactly
	// once.
	var carriedFrom int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum(amount_due_cents), 0) FROM billing_statements
		WHERE account_id = $1 AND state = 'accumulating'`, account.ID).Scan(&carriedFrom); err != nil {
		return Statement{}, err
	}

	terminal, nextMinimum, nextPeriod, err := s.nextPeriodOnCloseTx(ctx, tx, account, period, now)
	if err != nil {
		return Statement{}, err
	}
	amountDue := totals.PeriodTotalCents - period.PrepaidCents + nextMinimum + carriedFrom

	shadow := s.cfg.Mode != ModeEnabled
	state := StatementFinalized
	switch {
	case shadow:
		state = StatementShadow
	case amountDue <= 0:
		state = StatementWaived
	case amountDue < StripeMinimumChargeCents && terminal:
		state = StatementWaived
	case amountDue < StripeMinimumChargeCents:
		state = StatementAccumulating
	}

	coverage, err := json.Marshal(rated.Coverage)
	if err != nil {
		return Statement{}, err
	}
	statement := Statement{
		ID: statementID, AccountID: account.ID, PeriodID: period.ID, Kind: StatementPeriodClose,
		Currency: period.Currency, Shadow: shadow, State: state,
		GrossCents: totals.GrossCents, IncludedCents: period.IncludedUsageCents,
		AfterAllowanceCents: totals.AfterAllowanceCents, CreditCents: totals.CreditAppliedCents,
		MinimumCents: period.MonthlyMinimumCents, PrepaidAppliedCents: period.PrepaidCents,
		NextMinimumCents: nextMinimum, CarriedFromCents: carriedFrom,
		PeriodTotalCents: totals.PeriodTotalCents, AmountDueCents: amountDue,
		Coverage: rated.Coverage, CreatedAt: now, UpdatedAt: now,
	}
	if !shadow {
		statement.FinalizedAt = sql.NullTime{Time: now, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO billing_statements(`+statementColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,'','',NULL,$19,$20,$20)`,
		statement.ID, statement.AccountID, statement.PeriodID, statement.Kind, statement.Currency,
		statement.Shadow, statement.State, statement.GrossCents, statement.IncludedCents,
		statement.AfterAllowanceCents, statement.CreditCents, statement.MinimumCents,
		statement.PrepaidAppliedCents, statement.NextMinimumCents, statement.CarriedFromCents,
		statement.PeriodTotalCents, statement.AmountDueCents, coverage, statement.FinalizedAt, now); err != nil {
		return Statement{}, fmt.Errorf("insert statement: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_statements SET state = 'carried', carried_into_statement_id = $2, updated_at = $3
		WHERE account_id = $1 AND state = 'accumulating' AND id <> $2`, account.ID, statement.ID, now); err != nil {
		return Statement{}, err
	}

	lines := buildStatementLines(rated, totals, period, nextPeriod, nextMinimum, carriedFrom)
	for _, line := range lines {
		line.ID = uuid.NewString()
		if _, err := tx.ExecContext(ctx, `INSERT INTO billing_statement_lines
			(id, statement_id, line_index, project_id, project_name, meter, description, quantity, unit,
			 rate_numerator, rate_denominator, amount_cents, period_start, period_end)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8::numeric,$9,$10::numeric,$11::numeric,$12,$13,$14)`,
			line.ID, statement.ID, line.Index, line.ProjectID, line.ProjectName, line.Meter, line.Description,
			line.Quantity, line.Unit, line.RateNumerator, line.RateDenominator, line.AmountCents,
			line.PeriodStart, line.PeriodEnd); err != nil {
			return Statement{}, fmt.Errorf("insert statement line: %w", err)
		}
		statement.Lines = append(statement.Lines, line)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE billing_periods SET state = 'closed' WHERE id = $1`, period.ID); err != nil {
		return Statement{}, err
	}
	if state == StatementFinalized && s.work != nil {
		if _, err := s.work.EnqueueTx(ctx, tx, durablework.EnqueueParams{
			Kind: WorkKindInvoice, DedupKey: "billing-invoice:" + statement.ID,
			ResourceType: "billing_statement", ResourceID: statement.ID,
		}); err != nil {
			return Statement{}, fmt.Errorf("enqueue invoice work: %w", err)
		}
	}
	return statement, nil
}

// nextPeriodOnCloseTx creates the following period unless the account is
// cancelled with no retained volumes left to bill. Its minimum is recorded
// as prepaid on the new period; unpaid statements remain visible as debt.
func (s *Service) nextPeriodOnCloseTx(ctx context.Context, tx *sql.Tx, account Account, period Period, now time.Time) (terminal bool, nextMinimum int64, next Period, err error) {
	if account.State == AccountCancelled {
		var retained bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM usage_attributions
			WHERE workspace_id = $1 AND resource_kind = 'volume' AND ended_at IS NULL AND deleted_at IS NULL)`,
			account.WorkspaceID).Scan(&retained); err != nil {
			return false, 0, Period{}, err
		}
		if !retained {
			return true, 0, Period{}, nil
		}
	}
	next, err = ensurePeriodTx(ctx, tx, account, period.Index+1, now)
	if err != nil {
		return false, 0, Period{}, err
	}
	nextMinimum = next.MonthlyMinimumCents
	if nextMinimum > 0 && next.PrepaidCents != nextMinimum {
		if _, err := tx.ExecContext(ctx, `UPDATE billing_periods SET prepaid_cents = $2 WHERE id = $1`, next.ID, nextMinimum); err != nil {
			return false, 0, Period{}, err
		}
		next.PrepaidCents = nextMinimum
	}
	if account.PendingPlanID != "" && next.PlanID == account.PendingPlanID {
		if _, err := tx.ExecContext(ctx, `UPDATE billing_accounts SET plan_id = $2, pending_plan_id = '', updated_at = $3 WHERE id = $1`,
			account.ID, account.PendingPlanID, now); err != nil {
			return false, 0, Period{}, err
		}
	}
	return false, nextMinimum, next, nil
}

// buildStatementLines lays out the frozen statement: rated usage lines with
// allocated cents, then the account lines that make the period equation and
// the cash due sum exactly.
func buildStatementLines(rated RatedPeriod, totals Totals, period, next Period, nextMinimum, carriedFrom int64) []StatementLine {
	var lines []StatementLine
	index := int64(0)
	exact := make([]*big.Rat, len(rated.Lines))
	for i, line := range rated.Lines {
		exact[i] = line.ExactCents
	}
	allocated := AllocateCents(exact, totals.GrossCents)
	for i, line := range rated.Lines {
		rate := rated.Rates.Rates[line.Meter]
		lines = append(lines, StatementLine{
			Index: index, ProjectID: line.ProjectID, ProjectName: line.ProjectName,
			Meter: string(line.Meter), Description: meterDescription(line.Meter),
			Quantity: line.Quantity.String(), Unit: line.Meter.Unit(),
			RateNumerator: rate.Num.String(), RateDenominator: rate.Den.String(),
			AmountCents: allocated[i],
			PeriodStart: sql.NullTime{Time: period.StartsAt, Valid: true},
			PeriodEnd:   sql.NullTime{Time: period.EndsAt, Valid: true},
		})
		index++
	}
	accountLine := func(meter, description string, amount int64, start, end time.Time) {
		if amount == 0 {
			return
		}
		line := StatementLine{Index: index, Meter: meter, Description: description, AmountCents: amount,
			Quantity: "0", RateNumerator: "0", RateDenominator: "1"}
		if !start.IsZero() {
			line.PeriodStart = sql.NullTime{Time: start, Valid: true}
			line.PeriodEnd = sql.NullTime{Time: end, Valid: true}
		}
		lines = append(lines, line)
		index++
	}
	allowanceApplied := period.IncludedUsageCents
	if allowanceApplied > totals.GrossCents {
		allowanceApplied = totals.GrossCents
	}
	accountLine("minimum", "Monthly minimum", period.MonthlyMinimumCents, period.StartsAt, period.EndsAt)
	accountLine("included_usage", "Included usage allowance", -allowanceApplied, period.StartsAt, period.EndsAt)
	accountLine("credit", "Credit applied", -totals.CreditAppliedCents, time.Time{}, time.Time{})
	accountLine("prepaid", "Minimum prepaid at period start", -period.PrepaidCents, period.StartsAt, period.EndsAt)
	if next.ID != "" {
		accountLine("next_minimum", "Next period minimum", nextMinimum, next.StartsAt, next.EndsAt)
	}
	accountLine("carried_balance", "Balance carried from earlier statements", carriedFrom, time.Time{}, time.Time{})
	return lines
}

func meterDescription(meter Meter) string {
	switch meter {
	case MeterCPU:
		return "CPU time"
	case MeterMemory:
		return "Memory (total cgroup memory over time)"
	case MeterVolume:
		return "Volume storage (used filesystem blocks over time)"
	case MeterEgress:
		return "Public egress (logical IP bytes at the allocation interface)"
	default:
		return string(meter)
	}
}

// ListStatements returns an account's statements, newest first.
func (s *Service) ListStatements(ctx context.Context, workspaceID string, limit int) ([]Statement, error) {
	account, err := accountByWorkspace(ctx, s.db, workspaceID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 24
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+statementColumns+` FROM billing_statements
		WHERE account_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`, account.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Statement
	for rows.Next() {
		st, err := scanStatement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// GetStatement returns one statement with its lines, scoped to the
// workspace's account.
func (s *Service) GetStatement(ctx context.Context, workspaceID, statementID string) (Statement, error) {
	account, err := accountByWorkspace(ctx, s.db, workspaceID)
	if err != nil {
		return Statement{}, err
	}
	var st Statement
	err = sqlretry.ExecuteTx(ctx, s.db, &sql.TxOptions{ReadOnly: true}, func(tx *sql.Tx) error {
		var err error
		st, err = scanStatement(tx.QueryRowContext(ctx, `SELECT `+statementColumns+` FROM billing_statements WHERE id = $1 AND account_id = $2`, statementID, account.ID))
		if err != nil {
			return err
		}
		st.Lines, err = loadLinesTx(ctx, tx, st.ID)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Statement{}, fmt.Errorf("%w: statement not found", ErrNoAccount)
	}
	return st, err
}

// StatementCSV renders a statement for download. Amounts are decimal EUR.
func StatementCSV(st Statement) string {
	var b strings.Builder
	b.WriteString("line,project,meter,description,quantity,unit,rate_cents_per_unit,amount_eur,period_start,period_end\n")
	for _, line := range st.Lines {
		rate := ""
		if line.RateDenominator != "" && line.RateDenominator != "0" && line.RateNumerator != "0" {
			num, _ := new(big.Int).SetString(line.RateNumerator, 10)
			den, _ := new(big.Int).SetString(line.RateDenominator, 10)
			if num != nil && den != nil && den.Sign() != 0 {
				rate = new(big.Rat).SetFrac(num, den).FloatString(12)
			}
		}
		start, end := "", ""
		if line.PeriodStart.Valid {
			start = line.PeriodStart.Time.UTC().Format(time.RFC3339)
			end = line.PeriodEnd.Time.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "%d,%s,%s,%s,%s,%s,%s,%s,%s,%s\n", line.Index, csvCell(line.ProjectName),
			line.Meter, csvCell(line.Description), line.Quantity, line.Unit, rate,
			FormatCents(line.AmountCents), start, end)
	}
	fmt.Fprintf(&b, "total,,,,,,,,%s,,\n", FormatCents(st.AmountDueCents))
	return b.String()
}

func csvCell(value string) string {
	if strings.ContainsAny(value, ",\"\n") {
		return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
	}
	return value
}
