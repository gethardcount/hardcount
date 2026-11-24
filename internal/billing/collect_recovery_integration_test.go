package billing_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/tax"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/usage"
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

func (c *countingCharger) ChargeInvoice(ctx context.Context, tenantID string, inv domain.Invoice, _ domain.ChargeHold, _, _ string) (domain.Invoice, error) {
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
	clock    *clock.Fake // every engine's clock: a charger given it dates Stripe's answers on it
}

func newCollectHarness(t *testing.T) collectHarness {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Collect Recovery")
	invoiceStore := invoice.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	customerStore := customer.NewPostgresStore(db)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond)) // what Postgres stores, so a time read back compares equal

	newEng := func(ps billing.PaymentReadiness, ch billing.InvoiceCharger, ds billing.DunningStarter) *billing.Engine {
		e := billing.NewEngine(
			&subStoreAdapter{subStore}, &usageStoreAdapter{usage.NewPostgresStore(db)},
			&pricingStoreAdapter{pricing.NewPostgresStore(db)}, &invoiceStoreAdapter{invoiceStore},
			nil, tenant.NewSettingsStore(db), ps, ch, clk,
		)
		e.SetTaxProviderResolver(tax.NewResolver(nil))
		e.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
		e.SetDunningStarter(testDunningStarter{})
		e.SetStripeReadiness(testStripeConnected{})
		e.SetInvoiceEmailer(&testInvoiceEmailer{})
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
		inv, err := invoiceStore.CreateWithLineItems(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num,
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
			Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
			BillingPeriodStart: time.Now().UTC().Add(-30 * 24 * time.Hour),
			BillingPeriodEnd:   time.Now().UTC(),
		}, nil)
		if err != nil {
			t.Fatalf("create invoice: %v", err)
		}
		return inv
	}
	return collectHarness{ctx: ctx, tenantID: tenantID, invoices: invoiceStore, newEng: newEng, seed: seed, db: db, clock: clk}
}

// The nudge runs on its action's instant, here a finalize a minute before
// Stripe answers. The charger's settle dates paid_at when the answer arrives,
// and so is the nudge's own write after it, the clear of auto_charge_pending:
// the invoice's updated_at never goes back before paid_at.
func TestCollect_TheWriteAfterTheAnswerIsDatedWhenItArrives_RealStore(t *testing.T) {
	h := newCollectHarness(t)
	inv := h.seed(t, "INV-ARRIVAL")
	stub := &stripeReply{}
	stub.set(200, `{"id":"pi_arrival","object":"payment_intent","status":"succeeded","amount":5000,"currency":"usd"}`)
	charger := liveCharger(h, stub, &countingDunning{})
	charger.SetClock(h.clock)
	e := h.newEng(testPaymentSetupsReady{}, charger, nil)

	action := h.clock.Now(h.ctx).Add(-time.Minute)
	e.CollectInvoice(clock.WithEffectiveNow(h.ctx, action), h.tenantID, inv.ID, nil)

	got, err := h.invoices.Get(h.ctx, h.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.InvoicePaid || got.PaidAt == nil || got.AutoChargePending {
		t.Fatalf("invoice %s, paid_at %v, auto_charge_pending %v; want paid and out of the queue", got.Status, got.PaidAt, got.AutoChargePending)
	}
	if got.UpdatedAt.Before(*got.PaidAt) {
		t.Errorf("updated_at %v is before paid_at %v: the clear after the answer took the action's instant (%v)", got.UpdatedAt, *got.PaidAt, action)
	}
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

func (d dunningServiceStarter) StartDunning(ctx context.Context, tenantID, invoiceID, customerID string, cause domain.DunningStartCause) error {
	_, err := d.svc.StartDunning(ctx, tenantID, invoiceID, customerID, cause)
	return err
}

// The collector starts no-payment dunning for a card-less invoice, against
// the real dunning store: one run (cause no_payment_method) however many
// ticks revisit it, and none at all for a customer with a card.
func TestCollector_StartsNoPaymentDunning_RealStore(t *testing.T) {
	h := newCollectHarness(t)
	dstore := dunning.NewPostgresStore(h.db)
	if _, err := dstore.UpsertPolicy(h.ctx, h.tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetryDays: []int{3, 3, 3},
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone,
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
// engine as the clock resolver — the production wiring — so an invoice on no
// clock is dated at the sweep row's instant, read from the engine's clock, as
// it is live.
func TestCollector_FirstRetryCountsFromTheFailure_RealStore(t *testing.T) {
	h := newCollectHarness(t)
	dstore := dunning.NewPostgresStore(h.db)
	if _, err := dstore.UpsertPolicy(h.ctx, h.tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetryDays: []int{3, 3, 3},
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone,
	}); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	failedAt := h.clock.Now(h.ctx) // the sweep row's instant: the engine's clock, fixed in the harness
	svc := dunning.NewService(dstore, nil, nil)
	e := h.newEng(testPaymentSetupsNoPM{}, testChargerSentinel{}, dunningServiceStarter{svc: svc})

	cust, err := customer.NewPostgresStore(h.db).Create(h.ctx, h.tenantID, domain.Customer{ExternalID: "cus_anchor", DisplayName: "Anchor"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	periodStart := time.Now().UTC().Add(-10 * 24 * time.Hour)
	issued := periodStart.Add(time.Hour)
	inv, err := h.invoices.CreateWithLineItems(h.ctx, h.tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-ANCHOR-1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingPeriodStart: periodStart, BillingPeriodEnd: periodStart.Add(30 * 24 * time.Hour),
		IssuedAt: &issued,
	}, nil)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	e.RetryPendingCharges(clock.WithRealTime(h.ctx), 50, nil) // a job's tick, decided at real time as the scheduler decides it

	run, err := dstore.GetRunByInvoice(h.ctx, h.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("card-less invoice must have a dunning run: %v", err)
	}
	grace := 3 * 24 * time.Hour
	if run.NextActionAt == nil || !run.NextActionAt.Equal(failedAt.Add(grace)) {
		t.Errorf("first retry = %v, want %v, the failure plus 3 days (issued %v would give %v)",
			run.NextActionAt, failedAt.Add(grace), issued, issued.Add(grace))
	}
}

type decliningRetrier struct{}

func (decliningRetrier) RetryPayment(context.Context, string, string, string) error {
	return fmt.Errorf("card declined")
}

// A retry at real time that runs late — the scheduler was down, or its first
// retry was mis-anchored in the past — schedules the next one from when it
// ran, not from when it was due. Had a run at real time read as on a test
// clock, each later retry would be dated in the past too and fire on the next
// tick.
func TestProcessDueRuns_LateWallClockRetryKeepsCadence_RealStore(t *testing.T) {
	h := newCollectHarness(t)
	dstore := dunning.NewPostgresStore(h.db)
	if _, err := dstore.UpsertPolicy(h.ctx, h.tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetryDays: []int{3, 3, 5},
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone,
	}); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	before := time.Now().UTC().Truncate(time.Microsecond)
	svc := dunning.NewService(dstore, decliningRetrier{}, nil)

	inv := h.seed(t, "INV-CADENCE-1")
	// The invoice is on no clock: the failure that starts dunning is decided at real time.
	run, err := svc.StartDunning(clock.WithRealTime(h.ctx), h.tenantID, inv.ID, inv.CustomerID, domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	// The first retry is ten days overdue.
	overdue := time.Now().UTC().Add(-10 * 24 * time.Hour)
	execTx(t, h.db, h.ctx, h.tenantID, `UPDATE invoice_dunning_runs SET next_action_at = $1 WHERE id = $2`, overdue, run.ID)

	tick := leadertest.Token(t, testutil.AdminPool(t), clock.WithRealTime(h.ctx), leader.RoleDunning) // a led dunning tick, as the scheduler runs and decides it
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

// execTx runs one statement in its own tenant transaction.
func execTx(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, q string, args ...any) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
