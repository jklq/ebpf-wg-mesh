package billing

import (
	"context"
	"database/sql"
	"fmt"
)

// Schema creates the billing tables. Financial rows deliberately carry no
// foreign keys into the catalog: deleting a workspace, project, or resource
// must never cascade into its bill, and payer attribution is snapshotted in
// usage_attributions at assignment time.
var Schema = []string{
	`CREATE TABLE billing_control (
		singleton BOOL PRIMARY KEY CHECK (singleton = TRUE),
		collection_paused BOOL NOT NULL DEFAULT FALSE,
		pause_reason TEXT NOT NULL DEFAULT '',
		updated_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE TABLE billing_accounts (
		id TEXT PRIMARY KEY,
		workspace_id TEXT NOT NULL UNIQUE,
		stripe_customer_id TEXT NOT NULL DEFAULT '',
		currency TEXT NOT NULL,
		plan_id TEXT NOT NULL,
		pending_plan_id TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL CHECK (state IN ('trial', 'pending_activation', 'active', 'past_due', 'cancelled')),
		anchor_at TIMESTAMPTZ NOT NULL,
		trial_ends_at TIMESTAMPTZ NULL,
		cancel_effective_at TIMESTAMPTZ NULL,
		budget_stop_cents INT8 NULL CHECK (budget_stop_cents IS NULL OR budget_stop_cents > 0),
		payment_ready BOOL NOT NULL DEFAULT FALSE,
		created_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE TABLE billing_periods (
		id TEXT PRIMARY KEY,
		account_id TEXT NOT NULL,
		period_index INT8 NOT NULL,
		kind TEXT NOT NULL CHECK (kind IN ('standard', 'storage_only')),
		plan_id TEXT NOT NULL,
		starts_at TIMESTAMPTZ NOT NULL,
		ends_at TIMESTAMPTZ NOT NULL,
		monthly_minimum_cents INT8 NOT NULL CHECK (monthly_minimum_cents >= 0),
		included_usage_cents INT8 NOT NULL CHECK (included_usage_cents >= 0),
		currency TEXT NOT NULL,
		rate_version INT8 NOT NULL,
		prepaid_cents INT8 NOT NULL DEFAULT 0 CHECK (prepaid_cents >= 0),
		state TEXT NOT NULL CHECK (state IN ('open', 'closed')),
		created_at TIMESTAMPTZ NOT NULL,
		UNIQUE (account_id, period_index),
		UNIQUE (account_id, starts_at)
	)`,
	`CREATE INDEX idx_billing_periods_open ON billing_periods(state, ends_at) WHERE state = 'open'`,
	`CREATE TABLE billing_rates (
		version INT8 NOT NULL,
		currency TEXT NOT NULL,
		meter TEXT NOT NULL,
		unit TEXT NOT NULL,
		numerator NUMERIC(38,0) NOT NULL,
		denominator NUMERIC(38,0) NOT NULL,
		PRIMARY KEY (version, currency, meter)
	)`,
	// Credit entries are an append-only ledger: grants and adjustments are
	// positive, consumptions are negative and name the grant they consume.
	`CREATE TABLE billing_credit_entries (
		id TEXT PRIMARY KEY,
		account_id TEXT NOT NULL,
		kind TEXT NOT NULL CHECK (kind IN ('grant', 'consumption', 'adjustment', 'refund')),
		grant_id TEXT NOT NULL DEFAULT '',
		amount_cents INT8 NOT NULL CHECK (amount_cents <> 0),
		expires_at TIMESTAMPTZ NULL,
		statement_id TEXT NOT NULL DEFAULT '',
		dedup_key TEXT NOT NULL UNIQUE,
		note TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE INDEX idx_billing_credit_entries_account ON billing_credit_entries(account_id, created_at)`,
	`CREATE TABLE billing_statements (
		id TEXT PRIMARY KEY,
		account_id TEXT NOT NULL,
		period_id TEXT NOT NULL,
		kind TEXT NOT NULL CHECK (kind IN ('activation', 'period_close')),
		currency TEXT NOT NULL,
		shadow BOOL NOT NULL,
		state TEXT NOT NULL CHECK (state IN ('shadow', 'finalized', 'open', 'paid', 'payment_failed', 'accumulating', 'carried', 'waived', 'void')),
		gross_cents INT8 NOT NULL,
		included_cents INT8 NOT NULL,
		after_allowance_cents INT8 NOT NULL,
		credit_cents INT8 NOT NULL,
		minimum_cents INT8 NOT NULL,
		prepaid_applied_cents INT8 NOT NULL,
		next_minimum_cents INT8 NOT NULL,
		carried_from_cents INT8 NOT NULL DEFAULT 0,
		period_total_cents INT8 NOT NULL,
		amount_due_cents INT8 NOT NULL,
		coverage JSONB NOT NULL DEFAULT '{}',
		stripe_invoice_id TEXT NOT NULL DEFAULT '',
		stripe_hosted_invoice_url TEXT NOT NULL DEFAULT '',
		provider_attempt_at TIMESTAMPTZ NULL,
		carried_into_statement_id TEXT NOT NULL DEFAULT '',
		finalized_at TIMESTAMPTZ NULL,
		created_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL,
		UNIQUE (period_id, kind)
	)`,
	`CREATE INDEX idx_billing_statements_account ON billing_statements(account_id, created_at DESC)`,
	`CREATE INDEX idx_billing_statements_stripe ON billing_statements(stripe_invoice_id) WHERE stripe_invoice_id <> ''`,
	`CREATE TABLE billing_statement_lines (
		id TEXT PRIMARY KEY,
		statement_id TEXT NOT NULL,
		line_index INT8 NOT NULL,
		project_id TEXT NOT NULL DEFAULT '',
		project_name TEXT NOT NULL DEFAULT '',
		meter TEXT NOT NULL,
		description TEXT NOT NULL,
		quantity NUMERIC(38,0) NOT NULL DEFAULT 0,
		unit TEXT NOT NULL DEFAULT '',
		rate_numerator NUMERIC(38,0) NOT NULL DEFAULT 0,
		rate_denominator NUMERIC(38,0) NOT NULL DEFAULT 1,
		amount_cents INT8 NOT NULL,
		period_start TIMESTAMPTZ NULL,
		period_end TIMESTAMPTZ NULL,
		stripe_invoice_item_id TEXT NOT NULL DEFAULT '',
		UNIQUE (statement_id, line_index)
	)`,
	`CREATE TABLE billing_restrictions (
		workspace_id TEXT PRIMARY KEY,
		account_id TEXT NOT NULL DEFAULT '',
		restricted BOOL NOT NULL DEFAULT FALSE,
		growth_blocked BOOL NOT NULL DEFAULT FALSE,
		dry_run BOOL NOT NULL DEFAULT TRUE,
		reason TEXT NOT NULL DEFAULT '',
		since TIMESTAMPTZ NULL,
		updated_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE TABLE billing_notices (
		id TEXT PRIMARY KEY,
		account_id TEXT NOT NULL,
		period_id TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL,
		threshold_percent INT8 NOT NULL DEFAULT 0,
		dedup_key TEXT NOT NULL UNIQUE,
		payload JSONB NOT NULL DEFAULT '{}',
		created_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE TABLE billing_provider_events (
		stripe_event_id TEXT PRIMARY KEY,
		event_type TEXT NOT NULL,
		payload BYTEA NOT NULL,
		received_at TIMESTAMPTZ NOT NULL,
		processed_at TIMESTAMPTZ NULL,
		process_error TEXT NOT NULL DEFAULT ''
	)`,
}

// SeedTx inserts the singleton control row and rate version 1. It runs in
// the schema bootstrap transaction.
func SeedTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO billing_control(singleton, collection_paused, pause_reason, updated_at)
		VALUES (TRUE, FALSE, '', statement_timestamp()) ON CONFLICT (singleton) DO NOTHING`); err != nil {
		return fmt.Errorf("seed billing control: %w", err)
	}
	v1 := RateVersion1()
	for _, meter := range Meters {
		rate := v1.Rates[meter]
		if _, err := tx.ExecContext(ctx, `INSERT INTO billing_rates(version, currency, meter, unit, numerator, denominator)
			VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric) ON CONFLICT (version, currency, meter) DO NOTHING`,
			v1.Version, v1.Currency, string(meter), meter.Unit(), rate.Num.String(), rate.Den.String()); err != nil {
			return fmt.Errorf("seed billing rate %s: %w", meter, err)
		}
	}
	return nil
}
