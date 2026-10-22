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
	Quota               Quota
}

// Quota bounds one workspace's desired state and build usage. The trial
// plan is the free tier: its values are the abuse ceiling for public
// signup. Paid values are generous fleet protection, not a pricing
// feature. A zero MaxServicesPerProject, MaxCPUMillisPerService,
// MaxMemoryMebibytesPerService, or MaxBuildMinutes30Days means no cap on
// that dimension. Quotas are validated on desired-state mutations
// whenever billing is not disabled (shadow included), unlike spend
// restrictions, which only enforce in enabled mode; see
// ValidateServiceQuotaTx.
//
// Ephemeral storage is not a Quota dimension: it has no per-service spec
// field. The fleet agent caps every allocation's writable layer at
// 1 GiB regardless of plan, which is the free tier's ephemeral limit.
type Quota struct {
	MaxCPUMillis                 int64
	MaxMemoryMebibytes           int64
	MaxCPUMillisPerService       int64
	MaxMemoryMebibytesPerService int64
	MaxServicesPerEnvironment    int64
	MaxServicesPerProject        int64
	MaxReplicasPerService        int64
	MaxProjects                  int64
	MaxVolumeBytes               int64
	MaxConcurrentBuilds          int64
	MaxBuildMinutes30Days        int64
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
	// The free tier: one project with up to 3 services, each at most
	// 1 vCPU and 512 MiB with a single replica, and 512 MiB of volume
	// storage in total. The workspace CPU/memory totals are the three
	// per-service caps summed. One free workspace per user is enforced
	// separately; see ValidateWorkspaceCreateTx.
	PlanTrial: {ID: PlanTrial, MonthlyMinimumCents: 0, IncludedUsageCents: 0, Quota: Quota{
		MaxCPUMillis: 3_000, MaxMemoryMebibytes: 1_536,
		MaxCPUMillisPerService: 1_000, MaxMemoryMebibytesPerService: 512,
		MaxServicesPerEnvironment: 3, MaxServicesPerProject: 3, MaxReplicasPerService: 1, MaxProjects: 1,
		MaxVolumeBytes: 512 << 20, MaxConcurrentBuilds: 1, MaxBuildMinutes30Days: 300,
	}},
	PlanHobby: {ID: PlanHobby, MonthlyMinimumCents: 500, IncludedUsageCents: 500, Quota: Quota{
		MaxCPUMillis: 8_000, MaxMemoryMebibytes: 16_384,
		MaxServicesPerEnvironment: 25, MaxReplicasPerService: 10, MaxProjects: 10,
		MaxVolumeBytes: 100 << 30, MaxConcurrentBuilds: 2,
	}},
	PlanPro: {ID: PlanPro, MonthlyMinimumCents: 2000, IncludedUsageCents: 2000, Quota: Quota{
		MaxCPUMillis: 32_000, MaxMemoryMebibytes: 65_536,
		MaxServicesPerEnvironment: 100, MaxReplicasPerService: 64, MaxProjects: 100,
		MaxVolumeBytes: 1 << 40, MaxConcurrentBuilds: 5,
	}},
	// PlanStorage covers retained volumes after cancellation: storage-only
	// periods with no minimum and no compute allowance. Its quota never
	// binds: cancelled accounts are restricted before quotas matter.
	PlanStorage: {ID: PlanStorage, MonthlyMinimumCents: 0, IncludedUsageCents: 0, Quota: Quota{
		MaxCPUMillis: 32_000, MaxMemoryMebibytes: 65_536,
		MaxServicesPerEnvironment: 100, MaxReplicasPerService: 64, MaxProjects: 100,
		MaxVolumeBytes: 1 << 40, MaxConcurrentBuilds: 5,
	}},
}

// TrialCreditCents and TrialDuration bound the initial trial grant. One
// grant exists per user ever (billing_trial_grants): only a user's first
// workspace receives it, and deleting that workspace does not restore
// eligibility.
const (
	TrialCreditCents = 500
	TrialDuration    = 30 * 24 * time.Hour
)

// TrialEgressCapBytes is the trial-only public-egress ceiling per billing
// period. The maintenance pass restricts a trial account whose rated
// egress passes it; the datapath is unchanged.
const TrialEgressCapBytes = 25 << 30

// GitHubMinAccountAge is how old the trial owner's GitHub account must be
// before trial compute runs. Younger (or unverifiable) GitHub accounts keep
// a restricted trial workspace until paid activation.
const GitHubMinAccountAge = 30 * 24 * time.Hour

// BuildMinutesWindow is the rolling window for the trial build-minute cap.
const BuildMinutesWindow = 30 * 24 * time.Hour

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
