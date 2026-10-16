package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	stripe "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/client"
	"github.com/stripe/stripe-go/v82/webhook"
)

// StripeAPIVersion is the API version pinned by the SDK dependency. The
// billing module owns rating and allowances; Stripe only collects.
const StripeAPIVersion = stripe.APIVersion

// InvoiceSnapshot is the provider state billing reconciles against.
type InvoiceSnapshot struct {
	ID              string
	Status          string
	AmountDueCents  int64
	AmountPaidCents int64
	HostedURL       string
	Currency        string
	CustomerID      string
	StatementID     string
}

// InvoiceItemRequest is one provider line for a draft invoice.
type InvoiceItemRequest struct {
	LineID         string
	StatementID    string
	CustomerID     string
	InvoiceID      string
	Description    string
	AmountCents    int64
	PeriodStart    time.Time
	PeriodEnd      time.Time
	IdempotencyKey string
}

// StripeAdapter is the narrow provider surface inside the billing module.
// There is exactly one payment provider; this interface exists so invoice
// settlement can be exercised with a fake in tests, not as a framework.
type StripeAdapter interface {
	CreateCustomer(ctx context.Context, accountID, workspaceID, idempotencyKey string) (string, error)
	CreateCheckoutSetup(ctx context.Context, customerID, accountID, successURL, cancelURL, idempotencyKey string) (sessionID, url string, err error)
	CheckoutPaymentMethod(ctx context.Context, sessionID string) (paymentMethodID string, err error)
	SetCustomerDefaultPaymentMethod(ctx context.Context, customerID, paymentMethodID string) error
	CreateDraftInvoice(ctx context.Context, customerID, statementID, idempotencyKey string) (InvoiceSnapshot, error)
	AddInvoiceItem(ctx context.Context, req InvoiceItemRequest) (string, error)
	FinalizeInvoice(ctx context.Context, invoiceID, idempotencyKey string) (InvoiceSnapshot, error)
	RetrieveInvoice(ctx context.Context, invoiceID string) (InvoiceSnapshot, error)
	FindInvoiceByStatement(ctx context.Context, customerID, statementID string) (InvoiceSnapshot, bool, error)
}

// sdkStripe implements StripeAdapter with the official Stripe Go SDK.
type sdkStripe struct {
	api *client.API
}

// NewStripeAdapter builds the SDK adapter. baseURL overrides the Stripe API
// endpoint for tests against a local fake; empty uses production Stripe.
func NewStripeAdapter(secretKey, baseURL string) StripeAdapter {
	api := &client.API{}
	if baseURL != "" {
		backend := stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
			URL:        stripe.String(baseURL),
			HTTPClient: &http.Client{Timeout: 30 * time.Second},
		})
		api.Init(secretKey, &stripe.Backends{API: backend, Connect: backend, Uploads: backend, MeterEvents: backend})
	} else {
		api.Init(secretKey, nil)
	}
	return &sdkStripe{api: api}
}

func snapshot(inv *stripe.Invoice) InvoiceSnapshot {
	out := InvoiceSnapshot{
		ID: inv.ID, Status: string(inv.Status), AmountDueCents: inv.AmountDue,
		AmountPaidCents: inv.AmountPaid, HostedURL: inv.HostedInvoiceURL,
		Currency: string(inv.Currency),
	}
	if inv.Customer != nil {
		out.CustomerID = inv.Customer.ID
	}
	if inv.Metadata != nil {
		out.StatementID = inv.Metadata["statement_id"]
	}
	return out
}

func (a *sdkStripe) CreateCustomer(ctx context.Context, accountID, workspaceID, idempotencyKey string) (string, error) {
	params := &stripe.CustomerParams{
		Metadata: map[string]string{"billing_account_id": accountID, "workspace_id": workspaceID},
	}
	params.Context = ctx
	params.IdempotencyKey = stripe.String(idempotencyKey)
	customer, err := a.api.Customers.New(params)
	if err != nil {
		return "", fmt.Errorf("stripe create customer: %w", err)
	}
	return customer.ID, nil
}

func (a *sdkStripe) CreateCheckoutSetup(ctx context.Context, customerID, accountID, successURL, cancelURL, idempotencyKey string) (string, string, error) {
	params := &stripe.CheckoutSessionParams{
		Mode:               stripe.String(string(stripe.CheckoutSessionModeSetup)),
		Customer:           stripe.String(customerID),
		Currency:           stripe.String(StripeCurrency),
		PaymentMethodTypes: stripe.StringSlice([]string{"card", "sepa_debit"}),
		SuccessURL:         stripe.String(successURL),
		CancelURL:          stripe.String(cancelURL),
		ClientReferenceID:  stripe.String(accountID),
		Metadata:           map[string]string{"billing_account_id": accountID},
	}
	params.Context = ctx
	params.IdempotencyKey = stripe.String(idempotencyKey)
	session, err := a.api.CheckoutSessions.New(params)
	if err != nil {
		return "", "", fmt.Errorf("stripe create checkout session: %w", err)
	}
	return session.ID, session.URL, nil
}

func (a *sdkStripe) CheckoutPaymentMethod(ctx context.Context, sessionID string) (string, error) {
	params := &stripe.CheckoutSessionParams{}
	params.Context = ctx
	session, err := a.api.CheckoutSessions.Get(sessionID, params)
	if err != nil {
		return "", fmt.Errorf("stripe retrieve checkout session: %w", err)
	}
	if session.SetupIntent == nil || session.SetupIntent.ID == "" {
		return "", errors.New("stripe checkout session has no setup intent")
	}
	intentParams := &stripe.SetupIntentParams{}
	intentParams.Context = ctx
	intent, err := a.api.SetupIntents.Get(session.SetupIntent.ID, intentParams)
	if err != nil {
		return "", fmt.Errorf("stripe retrieve setup intent: %w", err)
	}
	if intent.PaymentMethod == nil || intent.PaymentMethod.ID == "" {
		return "", errors.New("stripe setup intent has no payment method")
	}
	return intent.PaymentMethod.ID, nil
}

func (a *sdkStripe) SetCustomerDefaultPaymentMethod(ctx context.Context, customerID, paymentMethodID string) error {
	params := &stripe.CustomerParams{
		InvoiceSettings: &stripe.CustomerInvoiceSettingsParams{
			DefaultPaymentMethod: stripe.String(paymentMethodID),
		},
	}
	params.Context = ctx
	if _, err := a.api.Customers.Update(customerID, params); err != nil {
		return fmt.Errorf("stripe set default payment method: %w", err)
	}
	return nil
}

func (a *sdkStripe) CreateDraftInvoice(ctx context.Context, customerID, statementID, idempotencyKey string) (InvoiceSnapshot, error) {
	params := &stripe.InvoiceParams{
		Customer:         stripe.String(customerID),
		Currency:         stripe.String(StripeCurrency),
		AutoAdvance:      stripe.Bool(false),
		CollectionMethod: stripe.String(string(stripe.InvoiceCollectionMethodChargeAutomatically)),
		Metadata:         map[string]string{"statement_id": statementID},
	}
	params.Context = ctx
	params.IdempotencyKey = stripe.String(idempotencyKey)
	inv, err := a.api.Invoices.New(params)
	if err != nil {
		return InvoiceSnapshot{}, fmt.Errorf("stripe create invoice: %w", err)
	}
	return snapshot(inv), nil
}

func (a *sdkStripe) AddInvoiceItem(ctx context.Context, req InvoiceItemRequest) (string, error) {
	params := &stripe.InvoiceItemParams{
		Customer:    stripe.String(req.CustomerID),
		Invoice:     stripe.String(req.InvoiceID),
		Currency:    stripe.String(StripeCurrency),
		Amount:      stripe.Int64(req.AmountCents),
		Description: stripe.String(req.Description),
		Metadata:    map[string]string{"statement_id": req.StatementID, "statement_line_id": req.LineID},
	}
	if !req.PeriodStart.IsZero() {
		params.Period = &stripe.InvoiceItemPeriodParams{
			Start: stripe.Int64(req.PeriodStart.Unix()),
			End:   stripe.Int64(req.PeriodEnd.Unix()),
		}
	}
	params.Context = ctx
	params.IdempotencyKey = stripe.String(req.IdempotencyKey)
	item, err := a.api.InvoiceItems.New(params)
	if err != nil {
		return "", fmt.Errorf("stripe create invoice item: %w", err)
	}
	return item.ID, nil
}

func (a *sdkStripe) FinalizeInvoice(ctx context.Context, invoiceID, idempotencyKey string) (InvoiceSnapshot, error) {
	params := &stripe.InvoiceFinalizeInvoiceParams{}
	params.Context = ctx
	params.IdempotencyKey = stripe.String(idempotencyKey)
	inv, err := a.api.Invoices.FinalizeInvoice(invoiceID, params)
	if err != nil {
		return InvoiceSnapshot{}, fmt.Errorf("stripe finalize invoice: %w", err)
	}
	return snapshot(inv), nil
}

func (a *sdkStripe) RetrieveInvoice(ctx context.Context, invoiceID string) (InvoiceSnapshot, error) {
	params := &stripe.InvoiceParams{}
	params.Context = ctx
	inv, err := a.api.Invoices.Get(invoiceID, params)
	if err != nil {
		return InvoiceSnapshot{}, fmt.Errorf("stripe retrieve invoice: %w", err)
	}
	return snapshot(inv), nil
}

// FindInvoiceByStatement searches a customer's invoices for our statement
// metadata. It disambiguates a create whose response was lost beyond
// Stripe's 24-hour idempotency window before any retry creates a second
// invoice.
func (a *sdkStripe) FindInvoiceByStatement(ctx context.Context, customerID, statementID string) (InvoiceSnapshot, bool, error) {
	params := &stripe.InvoiceSearchParams{
		SearchParams: stripe.SearchParams{
			Query: fmt.Sprintf("customer:'%s' AND metadata['statement_id']:'%s'", customerID, statementID),
		},
	}
	params.Context = ctx
	iter := a.api.Invoices.Search(params)
	for iter.Next() {
		return snapshot(iter.Invoice()), true, nil
	}
	if err := iter.Err(); err != nil {
		return InvoiceSnapshot{}, false, fmt.Errorf("stripe search invoices: %w", err)
	}
	return InvoiceSnapshot{}, false, nil
}

// VerifyWebhook validates a Stripe webhook signature over the raw body and
// returns the parsed event. Callers persist the event before acknowledging.
func VerifyWebhook(payload []byte, signature, secret string) (stripe.Event, error) {
	return webhook.ConstructEvent(payload, signature, secret)
}
