// Package billing owns workspace billing accounts, periods, exact rating of
// metered usage, credits, frozen statements, spend restrictions, and Stripe
// collection. Its interface hides invoice arithmetic, settlement, retries,
// and provider state from the rest of the control plane.
//
// Modes gate every financial effect. In shadow mode the module computes
// statements and records restrictions as dry runs, but never charges and
// delivery never enforces. A full shadow period reconciled against
// independent measurements is the launch gate for enabled mode; see
// docs/billing.md.
package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"ebof-wg-mesh/internal/alarm"
	"ebof-wg-mesh/internal/controlplane/durablework"
)

// Currency is the single launch currency. Money is integer cents of it.
const Currency = "EUR"

// StripeCurrency is the lowercase ISO code Stripe expects.
const StripeCurrency = "eur"

// StripeMinimumChargeCents is Stripe's minimum charge amount for EUR.
const StripeMinimumChargeCents = 50

// Mode gates billing's financial effects.
type Mode string

const (
	ModeDisabled Mode = "disabled"
	ModeShadow   Mode = "shadow"
	ModeEnabled  Mode = "enabled"
)

// ParseMode validates an operator-supplied mode. Empty means shadow, the
// safe default: measurement and statements run, nothing charges.
func ParseMode(value string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(value))) {
	case "", ModeShadow:
		return ModeShadow, nil
	case ModeDisabled:
		return ModeDisabled, nil
	case ModeEnabled:
		return ModeEnabled, nil
	default:
		return "", fmt.Errorf("billing: unknown mode %q", value)
	}
}

// Plan is a monthly minimum with an equal or explicit included-usage
// allowance, applied once across the account.
type Plan struct {
	ID                  string
	MonthlyMinimumCents int64
	IncludedUsageCents  int64
}

// Launch plans, mirroring the Railway reference points in
// docs/usage-billing-plan.md. Their prices are provisional until the shadow
// run prices the platform properly (plan section 12).
const (
	PlanTrial   = "trial"
	PlanHobby   = "hobby"
	PlanPro     = "pro"
	PlanStorage = "storage"
)

var Plans = map[string]Plan{
	PlanTrial: {ID: PlanTrial, MonthlyMinimumCents: 0, IncludedUsageCents: 0},
	PlanHobby: {ID: PlanHobby, MonthlyMinimumCents: 500, IncludedUsageCents: 500},
	PlanPro:   {ID: PlanPro, MonthlyMinimumCents: 2000, IncludedUsageCents: 2000},
	// PlanStorage covers retained volumes after cancellation: storage-only
	// periods with no minimum and no compute allowance.
	PlanStorage: {ID: PlanStorage, MonthlyMinimumCents: 0, IncludedUsageCents: 0},
}

// TrialCreditCents and TrialDuration bound the initial trial grant.
const (
	TrialCreditCents = 500
	TrialDuration    = 30 * 24 * time.Hour
)

// PaymentGrace is how long a failed payment blocks growth before it also
// suspends compute.
const PaymentGrace = 7 * 24 * time.Hour

// PeriodCloseLateness is the window after a period ends during which late
// usage evidence may still arrive before the period closes.
const PeriodCloseLateness = 24 * time.Hour

var (
	// ErrDisabled reports a billing operation while billing is disabled.
	ErrDisabled = errors.New("billing: disabled")
	// ErrNoAccount reports a workspace without a billing account.
	ErrNoAccount = errors.New("billing: no billing account")
	// ErrGrowthBlocked reports new resource consumption under an enforced
	// restriction.
	ErrGrowthBlocked = errors.New("billing: account is restricted")
	// ErrStripeNotConfigured reports a provider operation without Stripe.
	ErrStripeNotConfigured = errors.New("billing: stripe is not configured")
	// ErrCollectionPaused reports provider work held by the kill switch.
	ErrCollectionPaused = errors.New("billing: collection is paused")
	// ErrInvalidInput reports a caller-supplied value outside its domain.
	ErrInvalidInput = errors.New("billing: invalid input")
)

// ProductTx runs fn inside the control plane's journaled product
// transaction, so restriction writes become durable product state.
type ProductTx func(ctx context.Context, fn func(context.Context, *sql.Tx) error) error

// Config carries billing's operator settings and dependencies.
type Config struct {
	Mode               Mode
	WebhookSecret      string
	CheckoutSuccessURL string
	CheckoutCancelURL  string
	// HourlyRetention bounds rating windows; maintenance warns when it is
	// shorter than a period plus the close lateness window.
	HourlyRetention time.Duration
}

// Service is the billing module facade.
type Service struct {
	db        *sql.DB
	work      *durablework.Store
	productTx ProductTx
	stripe    StripeAdapter
	alarms    *alarm.Sender
	cfg       Config
	now       func() time.Time
}

// New builds the billing service. stripe may be nil outside enabled mode;
// provider operations then fail with ErrStripeNotConfigured.
func New(db *sql.DB, work *durablework.Store, productTx ProductTx, stripe StripeAdapter, alarms *alarm.Sender, cfg Config) *Service {
	if cfg.Mode == "" {
		cfg.Mode = ModeShadow
	}
	return &Service{db: db, work: work, productTx: productTx, stripe: stripe, alarms: alarms, cfg: cfg, now: time.Now}
}

// Mode reports the configured billing mode.
func (s *Service) Mode() Mode { return s.cfg.Mode }

// SetClock replaces the service clock; tests use it to control periods.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

func (s *Service) alarm(ctx context.Context, event, subject, message, next string) {
	if s.alarms == nil {
		return
	}
	_ = s.alarms.Send(ctx, alarm.Event{Event: event, Subject: subject, Message: message, NextAction: next})
}
