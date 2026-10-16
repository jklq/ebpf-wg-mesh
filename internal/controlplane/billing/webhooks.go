package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"ebof-wg-mesh/internal/sqlretry"
	"ebof-wg-mesh/internal/telemetry"

	"github.com/google/uuid"
	stripe "github.com/stripe/stripe-go/v82"
)

// MaxWebhookBytes bounds a forwarded Stripe webhook body.
const MaxWebhookBytes = 256 << 10

var (
	ErrWebhookTooLarge         = errors.New("billing: stripe webhook payload too large")
	ErrWebhookInvalidSignature = errors.New("billing: stripe webhook signature invalid")
)

// IngestWebhook verifies the signature over the raw forwarded bytes and
// durably records the event before returning success. Duplicate deliveries
// are successful no-ops; processing happens asynchronously.
func (s *Service) IngestWebhook(ctx context.Context, payload []byte, signature string) error {
	if s.cfg.WebhookSecret == "" {
		return ErrStripeNotConfigured
	}
	if len(payload) > MaxWebhookBytes {
		return ErrWebhookTooLarge
	}
	event, err := VerifyWebhook(payload, signature, s.cfg.WebhookSecret)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrWebhookInvalidSignature, err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO billing_provider_events
		(stripe_event_id, event_type, payload, received_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (stripe_event_id) DO NOTHING`,
		event.ID, string(event.Type), payload, s.now().UTC())
	return err
}

// ProcessWebhooks applies recorded provider events in receipt order.
func (s *Service) ProcessWebhooks(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT stripe_event_id, event_type, payload FROM billing_provider_events
		WHERE processed_at IS NULL ORDER BY received_at, stripe_event_id LIMIT 50`)
	if err != nil {
		return 0, err
	}
	type pending struct {
		id, eventType string
		payload       []byte
	}
	var events []pending
	for rows.Next() {
		var e pending
		if err := rows.Scan(&e.id, &e.eventType, &e.payload); err != nil {
			rows.Close()
			return 0, err
		}
		events = append(events, e)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	processed := 0
	for _, e := range events {
		telemetry.Set(ctx, telemetry.String("billing.stripe.event_id", e.id),
			telemetry.String("billing.stripe.event_type", e.eventType))
		if err := s.processWebhook(ctx, e.eventType, e.payload); err != nil {
			telemetry.RecordBillingWebhook(ctx, "failed")
			_, _ = s.db.ExecContext(ctx, `UPDATE billing_provider_events SET process_error = $2 WHERE stripe_event_id = $1`,
				e.id, err.Error())
			return processed, fmt.Errorf("process stripe event %s: %w", e.id, err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE billing_provider_events SET processed_at = $2, process_error = '' WHERE stripe_event_id = $1`,
			e.id, s.now().UTC()); err != nil {
			return processed, err
		}
		telemetry.RecordBillingWebhook(ctx, "applied")
		processed++
	}
	return processed, nil
}

func (s *Service) processWebhook(ctx context.Context, eventType string, payload []byte) error {
	var event stripe.Event
	if err := json.Unmarshal(payload, &event); err != nil {
		return fmt.Errorf("decode stripe event: %w", err)
	}
	switch eventType {
	case "invoice.paid", "invoice.payment_succeeded", "invoice.finalized", "invoice.voided", "invoice.marked_uncollectible":
		var inv stripe.Invoice
		if err := json.Unmarshal(event.Data.Raw, &inv); err != nil {
			return err
		}
		return s.applyInvoiceEvent(ctx, eventType, snapshot(&inv))
	case "invoice.payment_failed":
		var inv stripe.Invoice
		if err := json.Unmarshal(event.Data.Raw, &inv); err != nil {
			return err
		}
		return s.applyInvoiceEvent(ctx, eventType, snapshot(&inv))
	case "checkout.session.completed":
		var session stripe.CheckoutSession
		if err := json.Unmarshal(event.Data.Raw, &session); err != nil {
			return err
		}
		return s.applyCheckoutCompleted(ctx, &session)
	default:
		return nil
	}
}

func (s *Service) statementIDForInvoice(ctx context.Context, tx *sql.Tx, snap InvoiceSnapshot) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM billing_statements WHERE stripe_invoice_id = $1`, snap.ID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if snap.StatementID != "" {
		err := tx.QueryRowContext(ctx, `SELECT id FROM billing_statements WHERE id = $1`, snap.StatementID).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	return "", sql.ErrNoRows
}

// applyInvoiceEvent folds an invoice webhook into statement and account
// state. A paid statement never regresses, whatever arrives later.
func (s *Service) applyInvoiceEvent(ctx context.Context, eventType string, snap InvoiceSnapshot) error {
	return sqlretry.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		statementID, err := s.statementIDForInvoice(ctx, tx, snap)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // an invoice we did not issue
		}
		if err != nil {
			return err
		}
		statement, err := scanStatement(tx.QueryRowContext(ctx, `SELECT `+statementColumns+` FROM billing_statements WHERE id = $1 FOR UPDATE`, statementID))
		if err != nil {
			return err
		}
		if statement.State == StatementPaid || statement.State == StatementWaived || statement.State == StatementVoid {
			return nil
		}
		now := s.now().UTC()
		switch eventType {
		case "invoice.paid", "invoice.payment_succeeded":
			if _, err := tx.ExecContext(ctx, `UPDATE billing_statements SET state = 'paid', stripe_hosted_invoice_url = $2, updated_at = $3 WHERE id = $1`,
				statement.ID, snap.HostedURL, now); err != nil {
				return err
			}
			return s.onStatementPaidTx(ctx, tx, statement, now)
		case "invoice.payment_failed":
			if _, err := tx.ExecContext(ctx, `UPDATE billing_statements SET state = 'payment_failed', updated_at = $2 WHERE id = $1`,
				statement.ID, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE billing_accounts SET state = 'past_due', updated_at = $2
				WHERE id = $1 AND state = 'active'`, statement.AccountID, now); err != nil {
				return err
			}
			payload, _ := json.Marshal(map[string]any{"statement_id": statement.ID, "stripe_invoice_id": snap.ID})
			_, err := tx.ExecContext(ctx, `INSERT INTO billing_notices
				(id, account_id, kind, dedup_key, payload, created_at)
				VALUES ($1,$2,'payment_failed',$3,$4,$5) ON CONFLICT (dedup_key) DO NOTHING`,
				uuid.NewString(), statement.AccountID, "payment-failed:"+statement.ID, payload, now)
			return err
		case "invoice.finalized":
			_, err := tx.ExecContext(ctx, `UPDATE billing_statements SET state = 'open', stripe_hosted_invoice_url = $2, updated_at = $3
				WHERE id = $1 AND state = 'finalized'`, statement.ID, snap.HostedURL, now)
			return err
		case "invoice.voided", "invoice.marked_uncollectible":
			_, err := tx.ExecContext(ctx, `UPDATE billing_statements SET state = 'void', updated_at = $2 WHERE id = $1`, statement.ID, now)
			return err
		}
		return nil
	})
}

// applyCheckoutCompleted saves the confirmed payment method as the
// customer's invoice default and continues a pending paid activation.
func (s *Service) applyCheckoutCompleted(ctx context.Context, session *stripe.CheckoutSession) error {
	if session.Mode != stripe.CheckoutSessionModeSetup {
		return nil
	}
	accountID := session.ClientReferenceID
	if accountID == "" && session.Metadata != nil {
		accountID = session.Metadata["billing_account_id"]
	}
	if accountID == "" {
		return errors.New("billing: checkout session without account reference")
	}
	account, err := accountByID(ctx, s.db, accountID)
	if err != nil {
		return err
	}
	if err := s.requireStripe(); err != nil {
		return err
	}
	paymentMethod, err := s.stripe.CheckoutPaymentMethod(ctx, session.ID)
	if err != nil {
		return err
	}
	customerID := account.StripeCustomerID
	if session.Customer != nil && session.Customer.ID != "" {
		customerID = session.Customer.ID
	}
	if err := s.stripe.SetCustomerDefaultPaymentMethod(ctx, customerID, paymentMethod); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE billing_accounts SET payment_ready = TRUE, stripe_customer_id = $2, updated_at = $3 WHERE id = $1`,
		account.ID, customerID, s.now().UTC()); err != nil {
		return err
	}
	if account.State == AccountPendingActivation && account.PendingPlanID != "" {
		_, period, err := s.AccountForWorkspace(ctx, account.WorkspaceID)
		if err != nil {
			return err
		}
		return s.createActivationStatement(ctx, account.ID, period.ID, account.PendingPlanID)
	}
	return nil
}
