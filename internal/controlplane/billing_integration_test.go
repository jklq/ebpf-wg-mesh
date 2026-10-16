//go:build integration

package controlplane

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/billing"
)

// fakeStripe is an in-memory StripeAdapter. Invoice amounts are the sum of
// added items, so statement verification sees real arithmetic.
type fakeStripe struct {
	mu            sync.Mutex
	customers     int
	invoices      map[string]*billing.InvoiceSnapshot
	items         map[string]int
	checkoutCount int
	nextInvoice   int
}

func newFakeStripe() *fakeStripe {
	return &fakeStripe{invoices: map[string]*billing.InvoiceSnapshot{}, items: map[string]int{}}
}

func (f *fakeStripe) CreateCustomer(_ context.Context, _, _, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.customers++
	return fmt.Sprintf("cus_fake_%d", f.customers), nil
}

func (f *fakeStripe) CreateCheckoutSetup(_ context.Context, _, _, _, _, _ string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkoutCount++
	return fmt.Sprintf("cs_fake_%d", f.checkoutCount), "https://stripe.test/checkout", nil
}

func (f *fakeStripe) CheckoutPaymentMethod(_ context.Context, _ string) (string, error) {
	return "pm_fake", nil
}

func (f *fakeStripe) SetCustomerDefaultPaymentMethod(_ context.Context, _, _ string) error {
	return nil
}

func (f *fakeStripe) CreateDraftInvoice(_ context.Context, customerID, statementID, _ string) (billing.InvoiceSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextInvoice++
	snap := &billing.InvoiceSnapshot{
		ID: fmt.Sprintf("in_fake_%d", f.nextInvoice), Status: "draft",
		Currency: "eur", CustomerID: customerID, StatementID: statementID,
		HostedURL: "https://stripe.test/invoice",
	}
	f.invoices[snap.ID] = snap
	return *snap, nil
}

func (f *fakeStripe) AddInvoiceItem(_ context.Context, req billing.InvoiceItemRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inv := f.invoices[req.InvoiceID]
	inv.AmountDueCents += req.AmountCents
	f.items[req.InvoiceID]++
	return fmt.Sprintf("ii_%s_%d", req.InvoiceID, f.items[req.InvoiceID]), nil
}

func (f *fakeStripe) FinalizeInvoice(_ context.Context, invoiceID, _ string) (billing.InvoiceSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inv := f.invoices[invoiceID]
	inv.Status = "open"
	return *inv, nil
}

func (f *fakeStripe) RetrieveInvoice(_ context.Context, invoiceID string) (billing.InvoiceSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inv, ok := f.invoices[invoiceID]
	if !ok {
		return billing.InvoiceSnapshot{}, fmt.Errorf("no such invoice %s", invoiceID)
	}
	return *inv, nil
}

func (f *fakeStripe) FindInvoiceByStatement(_ context.Context, _, statementID string) (billing.InvoiceSnapshot, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, inv := range f.invoices {
		if inv.StatementID == statementID {
			return *inv, true, nil
		}
	}
	return billing.InvoiceSnapshot{}, false, nil
}

func (f *fakeStripe) setStatus(invoiceID, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invoices[invoiceID].Status = status
	if status == "paid" {
		f.invoices[invoiceID].AmountPaidCents = f.invoices[invoiceID].AmountDueCents
	}
}

type billingFixture struct {
	store     *persistence
	svc       *billing.Service
	workspace string
	project   string
	anchor    time.Time
	clock     *time.Time
}

func newBillingFixture(t *testing.T, mode billing.Mode, adapter billing.StripeAdapter) *billingFixture {
	t.Helper()
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	f := &billingFixture{store: store, anchor: time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)}
	if err := store.db.QueryRow(`SELECT id, workspace_id FROM projects WHERE name = 'demo'`).Scan(&f.project, &f.workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE workspaces SET created_at = $2 WHERE id = $1`, f.workspace, f.anchor); err != nil {
		t.Fatal(err)
	}
	clock := f.anchor.Add(time.Hour)
	f.clock = &clock
	f.svc = billing.New(store.db, store.source.Work(), store.withProductTx, adapter, nil, billing.Config{
		Mode: mode, WebhookSecret: "whsec_billing_test",
		CheckoutSuccessURL: "https://console.test/billing/ok", CheckoutCancelURL: "https://console.test/billing",
	})
	f.svc.SetClock(func() time.Time { return *f.clock })
	if _, err := f.svc.EnsureAccounts(ctx); err != nil {
		t.Fatal(err)
	}
	return f
}

// seedUsage records one allocation's hourly usage inside the first period:
// half a vCPU-month of CPU (1000 cents), half a GiB-month of memory (500
// cents), and 2 GiB of egress (10 cents) at rate version 1.
func (f *billingFixture) seedUsage(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	giBMonthMillis := new(big.Int).Mul(big.NewInt(1<<30), big.NewInt(2_592_000_000))
	halfMemory := new(big.Int).Div(giBMonthMillis, big.NewInt(2))
	hour := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if _, err := f.store.db.ExecContext(ctx, `INSERT INTO usage_attributions
		(resource_kind, resource_id, agent_id, workspace_id, project_id, environment_id, service_id, environment_name, resource_name, created_at)
		VALUES ('allocation', 'alloc-billing-1', 'node-a', $1, $2, 'env-1', 'svc-1', 'production', 'web', $3)`,
		f.workspace, f.project, f.anchor); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(ctx, `INSERT INTO usage_hourly
		(hour, resource_kind, resource_id, project_id, environment_id, service_id, records, measured_ms, gap_ms, uncertain_ms,
		 cpu_usec, memory_byte_ms, memory_covered_ms, volume_byte_ms, volume_covered_ms, retained_volume_byte_ms,
		 egress_public_direct, egress_public_ingress, egress_private, egress_platform, egress_probe, first_start, last_end)
		VALUES ($1, 'allocation', 'alloc-billing-1', $2, 'env-1', 'svc-1', 60, 3600000, 0, 0,
		 1296000000000, $3::numeric, 3600000, 0, 0, 0, 2147483648, 0, 0, 0, 0, $1, $1)`,
		hour, f.project, halfMemory.String()); err != nil {
		t.Fatal(err)
	}
}

func (f *billingFixture) setClock(at time.Time) { *f.clock = at }

func TestBillingShadowStatementCloseRatesSeededUsage(t *testing.T) {
	f := newBillingFixture(t, billing.ModeShadow, nil)
	ctx := context.Background()
	f.seedUsage(t)
	f.setClock(time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC))

	summary, err := f.svc.AccountSummary(ctx, f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if summary.GrossCents != 1510 || summary.EstimatedTotalCents != 1010 || summary.CreditBalanceCents != 500 {
		t.Fatalf("summary gross=%d estimated=%d credit=%d, want 1510/1010/500",
			summary.GrossCents, summary.EstimatedTotalCents, summary.CreditBalanceCents)
	}
	if summary.Period.StartsAt.Day() != 31 || summary.Period.EndsAt.Month() != 2 || summary.Period.EndsAt.Day() != 28 {
		t.Fatalf("period = %s..%s, want Jan 31..Feb 28 anchor clamp", summary.Period.StartsAt, summary.Period.EndsAt)
	}

	f.setClock(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC))
	periodID := summary.Period.ID

	// Concurrent closers produce exactly one statement.
	ids := make([]string, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, err := f.svc.ClosePeriod(ctx, periodID)
			ids[i], errs[i] = st.ID, err
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
	}
	if ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("concurrent close statement ids = %q, %q", ids[0], ids[1])
	}
	var statements int
	if err := f.store.db.QueryRow(`SELECT count(*) FROM billing_statements`).Scan(&statements); err != nil {
		t.Fatal(err)
	}
	if statements != 1 {
		t.Fatalf("statements = %d, want 1", statements)
	}

	st, err := f.svc.GetStatement(ctx, f.workspace, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if !st.Shadow || st.State != billing.StatementShadow {
		t.Fatalf("statement shadow=%v state=%s, want a shadow statement", st.Shadow, st.State)
	}
	if st.GrossCents != 1510 || st.CreditCents != 500 || st.PeriodTotalCents != 1010 || st.AmountDueCents != 1010 {
		t.Fatalf("statement gross=%d credit=%d total=%d due=%d, want 1510/500/1010/1010",
			st.GrossCents, st.CreditCents, st.PeriodTotalCents, st.AmountDueCents)
	}
	var lineSum int64
	for _, line := range st.Lines {
		lineSum += line.AmountCents
	}
	if lineSum != st.AmountDueCents {
		t.Fatalf("statement lines sum to %d, amount due is %d", lineSum, st.AmountDueCents)
	}
	balance, err := f.svc.CreditBalance(ctx, summary.Account.ID, *f.clock)
	if err != nil {
		t.Fatal(err)
	}
	if balance != 0 {
		t.Fatalf("credit balance after close = %d, want 0 (trial credit consumed once)", balance)
	}

	// Statement project names are frozen snapshots.
	if _, err := f.store.db.Exec(`UPDATE projects SET name = 'renamed' WHERE id = $1`, f.project); err != nil {
		t.Fatal(err)
	}
	st, err = f.svc.GetStatement(ctx, f.workspace, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range st.Lines {
		if line.ProjectID == f.project && line.ProjectName != "demo" {
			t.Fatalf("statement project name = %q after rename, want frozen %q", line.ProjectName, "demo")
		}
	}
}

func TestBillingBudgetRestrictionIsDryRunInShadow(t *testing.T) {
	f := newBillingFixture(t, billing.ModeShadow, nil)
	ctx := context.Background()
	f.seedUsage(t)
	f.setClock(time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC))
	if err := f.svc.SetBudget(ctx, f.workspace, 1000); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.svc.EvaluateRestrictions(ctx); err != nil {
			t.Fatal(err)
		}
	}
	restriction, found, err := f.svc.RestrictionForWorkspace(ctx, f.workspace)
	if err != nil || !found {
		t.Fatalf("restriction found=%v err=%v", found, err)
	}
	if !restriction.Restricted || !restriction.DryRun || restriction.Enforced() {
		t.Fatalf("restriction = %+v, want restricted dry-run that is not enforced", restriction)
	}
	if err := f.svc.CheckGrowth(ctx, f.workspace); err != nil {
		t.Fatalf("shadow CheckGrowth = %v, want nil", err)
	}
	var notices int
	if err := f.store.db.QueryRow(`SELECT count(*) FROM billing_notices`).Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if notices != 3 {
		t.Fatalf("budget notices = %d, want 3 (75/90/100 percent, deduplicated)", notices)
	}
}

func stripeTestSignature(payload []byte, secret string, at time.Time) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.%s", at.Unix(), payload)
	return fmt.Sprintf("t=%d,v1=%s", at.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

func TestBillingEnabledInvoiceSettlementAndWebhooks(t *testing.T) {
	fake := newFakeStripe()
	f := newBillingFixture(t, billing.ModeEnabled, fake)
	ctx := context.Background()
	f.seedUsage(t)

	// Convert the trial account into a paid hobby account whose first
	// minimum is already prepaid, as activation would leave it.
	if _, err := f.store.db.Exec(`UPDATE billing_accounts
		SET plan_id = 'hobby', state = 'active', payment_ready = TRUE, trial_ends_at = NULL`); err != nil {
		t.Fatal(err)
	}
	summary, err := f.svc.AccountSummary(ctx, f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec(`UPDATE billing_periods
		SET plan_id = 'hobby', monthly_minimum_cents = 500, included_usage_cents = 500, prepaid_cents = 500
		WHERE id = $1`, summary.Period.ID); err != nil {
		t.Fatal(err)
	}

	f.setClock(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC))
	st, err := f.svc.ClosePeriod(ctx, summary.Period.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Gross 1510, allowance 500, trial credit 500: total 500+1010-500=1010;
	// due = 1010 - 500 prepaid + 500 next minimum = 1010.
	if st.State != billing.StatementFinalized || st.AmountDueCents != 1010 || st.NextMinimumCents != 500 {
		t.Fatalf("statement state=%s due=%d next=%d, want finalized/1010/500", st.State, st.AmountDueCents, st.NextMinimumCents)
	}

	// Settlement is idempotent across queue retries.
	for range 2 {
		if err := f.svc.ProcessInvoice(ctx, st.ID); err != nil {
			t.Fatal(err)
		}
	}
	fake.mu.Lock()
	invoices, items := len(fake.invoices), fake.items["in_fake_1"]
	fake.mu.Unlock()
	if invoices != 1 {
		t.Fatalf("fake invoices = %d, want exactly 1", invoices)
	}
	nonZeroLines := 0
	for _, line := range st.Lines {
		if line.AmountCents != 0 {
			nonZeroLines++
		}
	}
	if items != nonZeroLines {
		t.Fatalf("fake invoice items = %d, want %d non-zero statement lines", items, nonZeroLines)
	}
	st, err = f.svc.GetStatement(ctx, f.workspace, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != billing.StatementOpen || st.StripeInvoiceID != "in_fake_1" {
		t.Fatalf("statement state=%s invoice=%s, want open/in_fake_1", st.State, st.StripeInvoiceID)
	}

	deliver := func(eventID, eventType string) {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"id": eventID, "type": eventType, "api_version": billing.StripeAPIVersion,
			"data": map[string]any{"object": map[string]any{
				"id": "in_fake_1", "object": "invoice", "status": "paid",
				"metadata": map[string]string{"statement_id": st.ID},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		sig := stripeTestSignature(payload, "whsec_billing_test", time.Now())
		// Duplicate delivery is a successful no-op.
		for range 2 {
			if err := f.svc.IngestWebhook(ctx, payload, sig); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.svc.ProcessWebhooks(ctx); err != nil {
			t.Fatal(err)
		}
	}
	deliver("evt_paid_1", "invoice.paid")
	st, err = f.svc.GetStatement(ctx, f.workspace, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != billing.StatementPaid {
		t.Fatalf("statement state after paid webhook = %s", st.State)
	}
	// An out-of-order failure event never regresses a paid invoice.
	deliver("evt_failed_late", "invoice.payment_failed")
	st, err = f.svc.GetStatement(ctx, f.workspace, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != billing.StatementPaid {
		t.Fatalf("statement regressed to %s after late failure event", st.State)
	}
	var events int
	if err := f.store.db.QueryRow(`SELECT count(*) FROM billing_provider_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 2 {
		t.Fatalf("provider events = %d, want 2 unique events", events)
	}
}

func TestBillingRestrictionStopsAndRestoresCompute(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a"})
	service, err := createService(ctx, store, "user-1", envID, "web",
		directImageServiceSpec("example.test/web:1", &platformv1.ServiceRuntime{CpuMillis: 500, MemoryMebibytes: 128}), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if allocations := mustListAllocations(t, store, ctx, service.ID); len(allocations) != 1 {
		t.Fatalf("allocations = %d, want 1 before restriction", len(allocations))
	}
	var workspace string
	if err := store.db.QueryRow(`SELECT workspace_id FROM projects WHERE id = $1`, service.ProjectID).Scan(&workspace); err != nil {
		t.Fatal(err)
	}

	// Anchor the account two days back so the seeded hour falls inside the
	// open period.
	if _, err := store.db.Exec(`UPDATE workspaces SET created_at = now() - interval '2 days' WHERE id = $1`, workspace); err != nil {
		t.Fatal(err)
	}
	svc := billing.New(store.db, store.source.Work(), store.withProductTx, nil, nil, billing.Config{Mode: billing.ModeEnabled})
	if _, err := svc.EnsureAccounts(ctx); err != nil {
		t.Fatal(err)
	}
	// Grow the trial past a one-cent budget with one hourly usage row for
	// the live allocation's existing attribution.
	hour := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
	allocations := mustListAllocations(t, store, ctx, service.ID)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO usage_hourly
		(hour, resource_kind, resource_id, project_id, environment_id, service_id, records, measured_ms, gap_ms, uncertain_ms,
		 cpu_usec, memory_byte_ms, memory_covered_ms, volume_byte_ms, volume_covered_ms, retained_volume_byte_ms,
		 egress_public_direct, egress_public_ingress, egress_private, egress_platform, egress_probe, first_start, last_end)
		VALUES ($1, 'allocation', $2, $3, $4, $5, 60, 3600000, 0, 0,
		 1296000000000, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, $1, $1)`,
		hour, allocations[0].ID, service.ProjectID, envID, service.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetBudget(ctx, workspace, 1); err != nil {
		t.Fatal(err)
	}
	if err := svc.EvaluateRestrictions(ctx); err != nil {
		t.Fatal(err)
	}
	restriction, _, err := svc.RestrictionForWorkspace(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !restriction.Enforced() {
		t.Fatalf("restriction = %+v, want enforced in enabled mode", restriction)
	}
	delivery := newTestDelivery(store, nil, nil, nil)
	if err := delivery.ReconcileFleetCapacity(ctx); err != nil {
		t.Fatal(err)
	}
	if allocations := mustListAllocations(t, store, ctx, service.ID); len(allocations) != 0 {
		t.Fatalf("allocations after restriction = %d, want 0", len(allocations))
	}
	if err := delivery.ReconcileFleetCapacity(ctx); err != nil {
		t.Fatal(err)
	}
	if allocations := mustListAllocations(t, store, ctx, service.ID); len(allocations) != 0 {
		t.Fatalf("restricted reconciliation recreated %d allocations", len(allocations))
	}

	// Clearing the restriction restores the preserved desired state.
	if err := svc.SetBudget(ctx, workspace, 0); err != nil {
		t.Fatal(err)
	}
	if err := svc.EvaluateRestrictions(ctx); err != nil {
		t.Fatal(err)
	}
	if err := delivery.ReconcileFleetCapacity(ctx); err != nil {
		t.Fatal(err)
	}
	if allocations := mustListAllocations(t, store, ctx, service.ID); len(allocations) != 1 {
		t.Fatalf("allocations after restriction cleared = %d, want 1 restored", len(allocations))
	}
}
