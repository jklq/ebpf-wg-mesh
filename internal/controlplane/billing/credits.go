package billing

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// grantCreditTx records one credit grant or signed adjustment. The dedup key
// makes grants and corrections safe to replay; nothing edits entries.
func grantCreditTx(ctx context.Context, tx *sql.Tx, accountID, kind string, amountCents int64, expiresAt time.Time, dedupKey, note string, now time.Time) error {
	var expiry sql.NullTime
	if !expiresAt.IsZero() {
		expiry = sql.NullTime{Time: expiresAt.UTC(), Valid: true}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO billing_credit_entries
		(id, account_id, kind, grant_id, amount_cents, expires_at, statement_id, dedup_key, note, created_at)
		VALUES ($1, $2, $3, '', $4, $5, '', $6, $7, $8)
		ON CONFLICT (dedup_key) DO NOTHING`,
		uuid.NewString(), accountID, kind, amountCents, expiry, dedupKey, note, now)
	if err != nil {
		return fmt.Errorf("record credit %s: %w", dedupKey, err)
	}
	return nil
}

// GrantCredit records an operator adjustment or refund credit for an
// account. Refunds and corrections are explicit entries, never edits.
func (s *Service) GrantCredit(ctx context.Context, workspaceID, kind string, amountCents int64, expiresAt time.Time, dedupKey, note string) error {
	if kind != "grant" && kind != "adjustment" && kind != "refund" {
		return fmt.Errorf("%w: credit kind %q", ErrInvalidInput, kind)
	}
	if amountCents == 0 || dedupKey == "" {
		return fmt.Errorf("%w: credit amount and dedup key are required", ErrInvalidInput)
	}
	account, err := accountByWorkspace(ctx, s.db, workspaceID)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := grantCreditTx(ctx, tx, account.ID, kind, amountCents, expiresAt, dedupKey, note, s.now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

type grantBalance struct {
	id        string
	remaining int64
	expiresAt sql.NullTime
}

// grantBalancesTx lists each unexpired grant-like entry's remaining amount,
// earliest expiry first. The caller holds the account row lock when these
// balances will be consumed.
func grantBalancesTx(ctx context.Context, tx *sql.Tx, accountID string, now time.Time) ([]grantBalance, error) {
	rows, err := tx.QueryContext(ctx, `SELECT g.id, g.amount_cents + COALESCE(sum(c.amount_cents), 0), g.expires_at
		  FROM billing_credit_entries g
		  LEFT JOIN billing_credit_entries c ON c.grant_id = g.id AND c.kind = 'consumption'
		 WHERE g.account_id = $1 AND g.kind <> 'consumption' AND g.amount_cents > 0
		   AND (g.expires_at IS NULL OR g.expires_at > $2)
		 GROUP BY g.id, g.amount_cents, g.expires_at, g.created_at
		 ORDER BY g.expires_at NULLS LAST, g.created_at, g.id`, accountID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []grantBalance
	for rows.Next() {
		var g grantBalance
		if err := rows.Scan(&g.id, &g.remaining, &g.expiresAt); err != nil {
			return nil, err
		}
		if g.remaining > 0 {
			out = append(out, g)
		}
	}
	return out, rows.Err()
}

// CreditBalance is the unexpired, unconsumed credit available to an account.
func (s *Service) CreditBalance(ctx context.Context, accountID string, now time.Time) (int64, error) {
	var balance sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT sum(remaining) FROM (
		SELECT g.amount_cents + COALESCE(sum(c.amount_cents), 0) AS remaining
		  FROM billing_credit_entries g
		  LEFT JOIN billing_credit_entries c ON c.grant_id = g.id AND c.kind = 'consumption'
		 WHERE g.account_id = $1 AND g.kind <> 'consumption' AND g.amount_cents > 0
		   AND (g.expires_at IS NULL OR g.expires_at > $2)
		 GROUP BY g.id, g.amount_cents) grants WHERE remaining > 0`, accountID, now).Scan(&balance)
	if err != nil {
		return 0, err
	}
	return balance.Int64, nil
}

// consumeCreditTx applies up to want cents of credit to a statement,
// earliest expiry first, recording each grant's consumption once. The
// account row must already be locked by the caller (period close locks it).
func consumeCreditTx(ctx context.Context, tx *sql.Tx, accountID, statementID string, want int64, now time.Time) (applied int64, err error) {
	if want <= 0 {
		return 0, nil
	}
	grants, err := grantBalancesTx(ctx, tx, accountID, now)
	if err != nil {
		return 0, err
	}
	for _, grant := range grants {
		if applied >= want {
			break
		}
		take := grant.remaining
		if remaining := want - applied; take > remaining {
			take = remaining
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO billing_credit_entries
			(id, account_id, kind, grant_id, amount_cents, statement_id, dedup_key, note, created_at)
			VALUES ($1, $2, 'consumption', $3, $4, $5, $6, '', $7)
			ON CONFLICT (dedup_key) DO NOTHING`,
			uuid.NewString(), accountID, grant.id, -take, statementID,
			fmt.Sprintf("consume:%s:%s", statementID, grant.id), now); err != nil {
			return 0, fmt.Errorf("consume credit: %w", err)
		}
		applied += take
	}
	return applied, nil
}
