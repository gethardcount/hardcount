package billing_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testclock"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// testPaymentSetupsReady reports a chargeable card for everyone.
type testPaymentSetupsReady struct{}

func (testPaymentSetupsReady) ResolveForCharge(_ context.Context, _, _ string) (string, string, error) {
	return "cus_stripe", "pm_card", nil
}

// countingCharger counts charges per invoice. delay holds each charge open
// so concurrent collectors overlap inside the charge leg. A successful charge
// marks the invoice paid through store, as the real charger's settle does:
// the queue hand-off clears the flag only once an outcome is on the row.
type countingCharger struct {
	mu    sync.Mutex
	n     map[string]int
	delay time.Duration
	store *invoice.PostgresStore
}

func (c *countingCharger) ChargeInvoice(ctx context.Context, tenantID string, inv domain.Invoice, _, _ string) (domain.Invoice, error) {
	time.Sleep(c.delay)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[inv.ID]++
	if c.store != nil {
		paid, err := c.store.MarkPaid(ctx, tenantID, inv.ID, "pi_test_"+inv.ID, time.Now().UTC())
		if err != nil {
			return inv, err
		}
		return paid, nil
	}
	return inv, nil
}

func (c *countingCharger) count(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[id]
}

type collectHarness struct {
	ctx      context.Context
	tenantID string
	invoices *invoice.PostgresStore
	newEng   func(ps billing.PaymentReadiness, ch billing.InvoiceCharger, ds billing.DunningStarter) *billing.Engine
	seed     func(t *testing.T, num string) domain.Invoice
	db       *postgres.DB
}

func newCollectHarness(t *testing.T) collectHarness {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Collect Recovery")
	invoiceStore := invoice.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	customerStore := customer.NewPostgresStore(db)

	newEng := func(ps billing.PaymentReadiness, ch billing.InvoiceCharger, ds billing.DunningStarter) *billing.Engine {
		e := billing.NewEngine(
			&subStoreAdapter{subStore}, &usageStoreAdapter{usage.NewPostgresStore(db)},
			&pricingStoreAdapter{pricing.NewPostgresStore(db)}, &invoiceStoreAdapter{invoiceStore},
			nil, tenant.NewSettingsStore(db), ps, ch, clock.NewFake(time.Now().UTC()),
		)
		e.SetTaxProviderResolver(tax.NewResolver(nil))
		e.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
		e.SetDunningStarter(testDunningStarter{})
		e.SetDunningResolver(&testDunningResolver{})
		if ds != nil {
			e.SetDunningStarter(ds)
		}
		return e
	}
	// seed writes a finalized, unpaid invoice through the real store: the
	// same write a finalize commits. Nothing after it runs, which is exactly
	// the state a crash right after the finalize commit leaves behind.
	seed := func(t *testing.T, num string) domain.Invoice {
		t.Helper()
		cust, err := customerStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_" + num, DisplayName: num})
		if err != nil {
			t.Fatalf("create customer: %v", err)
		}
		inv, err := invoiceStore.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num,
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
			Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
			BillingPeriodStart: time.Now().UTC().Add(-30 * 24 * time.Hour),
			BillingPeriodEnd:   time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("create invoice: %v", err)
		}
		return inv
	}
	return collectHarness{ctx: ctx, tenantID: tenantID, invoices: invoiceStore, newEng: newEng, seed: seed, db: db}
}

// A crash between the finalize commit and the post-commit collection used to
// strand the invoice: it was committed with auto_charge_pending=false and only
// the inline step (which never ran) would have set it. Now the finalize write
// queues it, so the next sweep charges it — exactly once across ticks.
func TestCollect_CrashAfterFinalize_SweepChargesExactlyOnce(t *testing.T) {
	h := newCollectHarness(t)
	inv := h.seed(t, "INV-CRASH-1")

	stored, err := h.invoices.Get(h.ctx, h.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !stored.AutoChargePending {
		t.Fatal("a finalized, unpaid invoice must be queued by its finalize write")
	}

	charger := &countingCharger{store: h.invoices}
	e := h.newEng(testPaymentSetupsReady{}, charger, nil)
	for tick := 0; tick < 3; tick++ {
		e.RetryPendingCharges(h.ctx, 50, nil)
	}
	if got := charger.count(inv.ID); got != 1 {
		t.Fatalf("charges = %d across 3 ticks, want exactly 1", got)
	}
}

// The finalize-time nudge and the hourly sweep can reach the same invoice at
// once (a cycle close racing a rival leader's sweep, or a request finishing as
// the tick starts). They share the per-invoice lease, so the card is charged
// once.
func TestCollect_NudgeAndSweepRace_ChargesOnce(t *testing.T) {
	h := newCollectHarness(t)
	inv := h.seed(t, "INV-RACE-1")
	charger := &countingCharger{delay: 50 * time.Millisecond, store: h.invoices}
	e := h.newEng(testPaymentSetupsReady{}, charger, nil)

	var wg sync.WaitGroup
	var started atomic.Int32
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			started.Add(1)
			e.CollectInvoice(h.ctx, h.tenantID, inv.ID, nil)
		}()
		go func() {
			defer wg.Done()
			started.Add(1)
			e.RetryPendingCharges(h.ctx, 50, nil)
		}()
	}
	wg.Wait()
	if got := charger.count(inv.ID); got != 1 {
		t.Fatalf("charges = %d with %d racing collectors, want exactly 1", got, started.Load())
	}
}

// dunningServiceStarter adapts the real dunning.Service to the engine's
// DunningStarter, the way api.dunningStarterAdapter does.
type dunningServiceStarter struct{ svc *dunning.Service }

func (d dunningServiceStarter) StartDunning(ctx context.Context, tenantID, invoiceID, customerID string, failureAt time.Time, cause domain.DunningStartCause) error {
	_, err := d.svc.StartDunning(ctx, tenantID, invoiceID, customerID, failureAt, cause)
	return err
}

// The collector starts no-payment dunning for a card-less invoice, against
// the real dunning store: one run (cause no_payment_method) however many
// ticks revisit it, and none at all for a customer with a card.
func TestCollector_StartsNoPaymentDunning_RealStore(t *testing.T) {
	h := newCollectHarness(t)
	dstore := dunning.NewPostgresStore(h.db)
	if _, err := dstore.UpsertPolicy(h.ctx, h.tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetrySchedule: []string{"72h"}, MaxRetryAttempts: 3,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone, GracePeriodDays: 3,
	}); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	starter := dunningServiceStarter{svc: dunning.NewService(dstore, nil, nil)}

	cardless := h.seed(t, "INV-NOCARD-1")
	e := h.newEng(testPaymentSetupsNoPM{}, testChargerSentinel{}, starter)
	for tick := 0; tick < 3; tick++ {
		e.RetryPendingCharges(h.ctx, 50, nil)
	}
	run, err := dstore.GetRunByInvoice(h.ctx, h.tenantID, cardless.ID)
	if err != nil {
		t.Fatalf("card-less invoice must have a dunning run: %v", err)
	}
	if run.Reason != string(domain.DunningCauseNoPaymentMethod) {
		t.Errorf("run reason = %q, want %q", run.Reason, domain.DunningCauseNoPaymentMethod)
	}

	withCard := h.seed(t, "INV-CARD-1")
	h.newEng(testPaymentSetupsReady{}, &countingCharger{store: h.invoices}, starter).RetryPendingCharges(h.ctx, 50, nil)
	if _, err := dstore.GetRunByInvoice(h.ctx, h.tenantID, withCard.ID); err == nil {
		t.Error("a customer with a card must not be dunned as no_payment_method")
	}
}

// The first retry counts from when collection failed, not from the invoice's
// issue date or billing period (found walking FLOW I10, 2026-10-11: an
// invoice issued 10 days into its period was anchored on the period start,
// so its first retry was already past and ran at once). Real store, real
// engine as the clock resolver — the production wiring — so an unpinned
// invoice resolves to wall-clock through the same path it does live.
func TestCollector_FirstRetryCountsFromTheFailure_RealStore(t *testing.T) {
	h := newCollectHarness(t)
	dstore := dunning.NewPostgresStore(h.db)
	if _, err := dstore.UpsertPolicy(h.ctx, h.tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetrySchedule: []string{"72h"}, MaxRetryAttempts: 3,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone, GracePeriodDays: 3,
	}); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	// Truncated to the microsecond: Postgres stores timestamps at that
	// precision, so a time written in the same microsecond as `before`
	// reads back a few hundred nanoseconds earlier than it and failed the
	// lower bound below (seen in CI on 2026-10-13). Same in the test after.
	before := time.Now().UTC().Truncate(time.Microsecond) // the engine's clock is fixed when it is built
	svc := dunning.NewService(dstore, nil, nil)
	e := h.newEng(testPaymentSetupsNoPM{}, testChargerSentinel{}, dunningServiceStarter{svc: svc})
	svc.SetResolver(e)

	cust, err := customer.NewPostgresStore(h.db).Create(h.ctx, h.tenantID, domain.Customer{ExternalID: "cus_anchor", DisplayName: "Anchor"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	periodStart := time.Now().UTC().Add(-10 * 24 * time.Hour)
	issued := periodStart.Add(time.Hour)
	inv, err := h.invoices.Create(h.ctx, h.tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-ANCHOR-1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingPeriodStart: periodStart, BillingPeriodEnd: periodStart.Add(30 * 24 * time.Hour),
		IssuedAt: &issued,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	e.RetryPendingCharges(h.ctx, 50, nil)
	after := time.Now().UTC()

	run, err := dstore.GetRunByInvoice(h.ctx, h.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("card-less invoice must have a dunning run: %v", err)
	}
	grace := 3 * 24 * time.Hour
	if run.NextActionAt == nil || run.NextActionAt.Before(before.Add(grace)) || run.NextActionAt.After(after.Add(grace)) {
		t.Errorf("first retry = %v, want failure + 3d, in [%v, %v] (issued %v would give %v)",
			run.NextActionAt, before.Add(grace), after.Add(grace), issued, issued.Add(grace))
	}
}

// The clock-pinned twin, outside any Advance (a decline webhook or the
// reconciler reports it): the real resolver finds the customer's clock, so
// the first retry counts from the invoice's simulated issue instant — inside
// the window the operator advances through — not from wall-clock now.
func TestStartDunning_ClockPinnedInvoiceCountsFromIssue_RealStore(t *testing.T) {
	h := newCollectHarness(t)
	dstore := dunning.NewPostgresStore(h.db)
	if _, err := dstore.UpsertPolicy(h.ctx, h.tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetrySchedule: []string{"72h"}, MaxRetryAttempts: 3,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone, GracePeriodDays: 3,
	}); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	svc := dunning.NewService(dstore, nil, nil)
	resolver := h.newEng(testPaymentSetupsNoPM{}, testChargerSentinel{}, nil)
	resolver.SetCustomerReader(customer.NewPostgresStore(h.db)) // as api/router.go wires it
	resolver.SetTestClockReader(testclock.NewPostgresStore(h.db))
	svc.SetResolver(resolver)

	frozen := time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC)
	clockID := postgres.NewID("vlx_tclk")
	execTx(t, h.db, h.ctx, h.tenantID,
		`INSERT INTO test_clocks (id, tenant_id, livemode, name, frozen_time, status)
		 VALUES ($1, $2, false, 'Anchor Clock', $3, 'ready')`, clockID, h.tenantID, frozen)
	cust, err := customer.NewPostgresStore(h.db).Create(h.ctx, h.tenantID, domain.Customer{ExternalID: "cus_anchor_sim", DisplayName: "Anchor Sim", TestClockID: clockID})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	if cust.TestClockID != clockID {
		t.Fatalf("customer test_clock_id = %q, want %q", cust.TestClockID, clockID)
	}
	issued := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	inv, err := h.invoices.Create(h.ctx, h.tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-ANCHOR-SIM",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		IssuedAt: &issued,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	run, err := svc.StartDunning(h.ctx, h.tenantID, inv.ID, cust.ID, issued, domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	if want := issued.Add(3 * 24 * time.Hour); run.NextActionAt == nil || !run.NextActionAt.Equal(want) {
		t.Errorf("first retry = %v, want %v (simulated issue + 3d)", run.NextActionAt, want)
	}
}

type decliningRetrier struct{}

func (decliningRetrier) RetryPayment(context.Context, string, string, string) error {
	return fmt.Errorf("card declined")
}

// A wall-clock retry that runs late — the scheduler was down, or its first
// retry was mis-anchored in the past — schedules the next one from when it
// ran, not from when it was due. With the production resolver wired, every
// run used to read as simulated, so each later retry was dated in the past
// too and fired on the next tick (VLX-000081: "Oct 4" then "Oct 7", both on
// Oct 11).
func TestProcessDueRuns_LateWallClockRetryKeepsCadence_RealStore(t *testing.T) {
	h := newCollectHarness(t)
	dstore := dunning.NewPostgresStore(h.db)
	if _, err := dstore.UpsertPolicy(h.ctx, h.tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetrySchedule: []string{"72h", "120h"}, MaxRetryAttempts: 3,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone, GracePeriodDays: 3,
	}); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	before := time.Now().UTC().Truncate(time.Microsecond) // the engine's clock is fixed when it is built
	svc := dunning.NewService(dstore, decliningRetrier{}, nil)
	resolver := h.newEng(testPaymentSetupsNoPM{}, testChargerSentinel{}, nil)
	resolver.SetCustomerReader(customer.NewPostgresStore(h.db))
	resolver.SetTestClockReader(testclock.NewPostgresStore(h.db))
	svc.SetResolver(resolver)

	inv := h.seed(t, "INV-CADENCE-1")
	run, err := svc.StartDunning(h.ctx, h.tenantID, inv.ID, inv.CustomerID, inv.IssuedAtOrZero(), domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	// The first retry is ten days overdue.
	overdue := time.Now().UTC().Add(-10 * 24 * time.Hour)
	execTx(t, h.db, h.ctx, h.tenantID, `UPDATE invoice_dunning_runs SET next_action_at = $1 WHERE id = $2`, overdue, run.ID)

	tick := leadertest.Token(t, testutil.AdminPool(t), h.ctx, leader.RoleDunning) // a led dunning tick, as the scheduler runs it
	if _, errs := svc.ProcessDueRuns(tick, h.tenantID, 20); len(errs) > 0 {
		t.Fatalf("process due runs: %v", errs)
	}
	got, err := dstore.GetRunByInvoice(h.ctx, h.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.AttemptCount != 1 {
		t.Fatalf("attempt_count = %d, want 1 (one retry per tick)", got.AttemptCount)
	}
	if got.NextActionAt == nil || got.NextActionAt.Before(before.Add(72*time.Hour)) {
		t.Errorf("next retry = %v, want at least %v (when this retry ran + 72h); %v would fire on the next tick",
			got.NextActionAt, before.Add(72*time.Hour), overdue.Add(72*time.Hour))
	}
}

// A clock-pinned invoice charged on a LATER Advance than its issue (a paused
// subscription's draft finalized later): its first retry counts from the
// charge, so the Advance that charged it runs ONE retry and stops — not the
// whole campaign. Counting from the issue put every retry in the past (each
// chains off the last in simulated time) and one dunning phase exhausted the
// run. Real store, real resolver, the catchup's own due-run loop.
func TestStartDunning_ClockPinnedChargedLater_OneRetryNotTheCampaign_RealStore(t *testing.T) {
	h := newCollectHarness(t)
	dstore := dunning.NewPostgresStore(h.db)
	if _, err := dstore.UpsertPolicy(h.ctx, h.tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetrySchedule: []string{"72h", "72h"}, MaxRetryAttempts: 3,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone, GracePeriodDays: 3,
	}); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	svc := dunning.NewService(dstore, decliningRetrier{}, nil)
	resolver := h.newEng(testPaymentSetupsNoPM{}, testChargerSentinel{}, nil)
	resolver.SetCustomerReader(customer.NewPostgresStore(h.db))
	resolver.SetTestClockReader(testclock.NewPostgresStore(h.db))
	svc.SetResolver(resolver)

	issued := time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC)  // cycle close; paused → draft
	charged := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC) // finalized and declined on a later Advance
	frozen := time.Date(2027, 6, 5, 0, 0, 0, 0, time.UTC)  // that Advance's target
	clockID := postgres.NewID("vlx_tclk")
	execTx(t, h.db, h.ctx, h.tenantID,
		`INSERT INTO test_clocks (id, tenant_id, livemode, name, frozen_time, status)
		 VALUES ($1, $2, false, 'Charged Later', $3, 'ready')`, clockID, h.tenantID, frozen)
	cust, err := customer.NewPostgresStore(h.db).Create(h.ctx, h.tenantID, domain.Customer{ExternalID: "cus_charged_later", DisplayName: "Charged Later", TestClockID: clockID})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	inv, err := h.invoices.Create(h.ctx, h.tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-CHARGED-LATER",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
		Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		IssuedAt: &issued,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	run, err := svc.StartDunning(h.ctx, h.tenantID, inv.ID, cust.ID, charged, domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	if want := charged.Add(3 * 24 * time.Hour); run.NextActionAt == nil || !run.NextActionAt.Equal(want) {
		t.Fatalf("first retry = %v, want %v (charge + 3d)", run.NextActionAt, want)
	}

	if _, errs := svc.ProcessDueRunsForClock(h.ctx, h.tenantID, clockID, frozen, 50); len(errs) > 0 {
		t.Fatalf("process due runs for clock: %v", errs)
	}
	got, err := dstore.GetRunByInvoice(h.ctx, h.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.AttemptCount != 1 || got.State != domain.DunningActive {
		t.Errorf("after the Advance to %v: attempt_count=%d state=%s, want 1 retry (Jun 4) and still active — the next is Jun 7",
			frozen, got.AttemptCount, got.State)
	}
}

// The card-less twin: a paused subscription's draft (issued at the May 1 cycle
// close) finalized on a later Advance enters collection only then, so its
// no_payment_method run counts from that collection — not from May 1, which
// put every retry in the past and let one dunning phase escalate it.
func TestCollector_CardlessDraftCollectedLater_CountsFromCollection_RealStore(t *testing.T) {
	h := newCollectHarness(t)
	dstore := dunning.NewPostgresStore(h.db)
	if _, err := dstore.UpsertPolicy(h.ctx, h.tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetrySchedule: []string{"72h", "72h"}, MaxRetryAttempts: 3,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone, GracePeriodDays: 3,
	}); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	svc := dunning.NewService(dstore, nil, nil)
	e := h.newEng(testPaymentSetupsNoPM{}, testChargerSentinel{}, dunningServiceStarter{svc: svc})
	e.SetCustomerReader(customer.NewPostgresStore(h.db))
	e.SetTestClockReader(testclock.NewPostgresStore(h.db))
	svc.SetResolver(e)

	issued := time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC)
	frozen := time.Date(2027, 6, 5, 0, 0, 0, 0, time.UTC) // the Advance that collects it
	clockID := postgres.NewID("vlx_tclk")
	execTx(t, h.db, h.ctx, h.tenantID,
		`INSERT INTO test_clocks (id, tenant_id, livemode, name, frozen_time, status)
		 VALUES ($1, $2, false, 'Cardless Later', $3, 'ready')`, clockID, h.tenantID, frozen)
	cust, err := customer.NewPostgresStore(h.db).Create(h.ctx, h.tenantID, domain.Customer{ExternalID: "cus_cardless_later", DisplayName: "Cardless Later", TestClockID: clockID})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	inv, err := h.invoices.Create(h.ctx, h.tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-CARDLESS-LATER",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingReason: domain.BillingReasonSubscriptionCycle, IssuedAt: &issued,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	advCtx := clock.WithSim(h.ctx, clock.Sim{At: frozen, TestClockID: clockID}) // as catchup binds Phase 3
	if _, errs := e.RetryPendingChargesForClock(advCtx, h.tenantID, clockID, 50); len(errs) > 0 {
		t.Fatalf("retry pending charges for clock: %v", errs)
	}
	run, err := dstore.GetRunByInvoice(h.ctx, h.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("card-less invoice must have a dunning run: %v", err)
	}
	if want := frozen.Add(3 * 24 * time.Hour); run.NextActionAt == nil || !run.NextActionAt.Equal(want) {
		t.Errorf("first retry = %v, want %v (collection + 3d), not %v (issue + 3d)", run.NextActionAt, want, issued.Add(3*24*time.Hour))
	}
}
