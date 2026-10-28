package billing_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/tax"
	"github.com/gethardcount/hardcount/internal/testclock"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/usage"
)

// cardOnFile reports one card for every customer.
type cardOnFile struct{}

func (cardOnFile) ResolveForCharge(context.Context, string, string) (string, string, error) {
	return "cus_stripe_trial", "pm_trial", nil
}

// anchoredCharger is the card charge as payment.Stripe makes it on a test
// clock: the PaymentIntent carries ctx's simulated instant as hardcount_anchor_at,
// and the settle stamps paid_at from that anchor. Only Stripe is left out;
// the settle is the real invoice store's.
type anchoredCharger struct {
	invoices *invoice.PostgresStore
	anchors  []time.Time
}

func (c *anchoredCharger) ChargeInvoice(ctx context.Context, tenantID string, inv domain.Invoice, _ domain.ChargeHold, _, _ string) (domain.Invoice, error) {
	sim, ok := clock.SimOf(ctx)
	if !ok {
		return inv, fmt.Errorf("anchoredCharger: charge of %s ran with no test clock on ctx", inv.ID)
	}
	c.anchors = append(c.anchors, sim.At)
	return c.invoices.MarkPaid(ctx, tenantID, inv.ID, fmt.Sprintf("pi_trial_%d", len(c.anchors)), sim.At)
}

// TestTrialExpiry_TestClock_ChargeNotBeforeIssue is the card half of B12 /
// P-3. The trial runs out at T and the clock is advanced to T+2d. The day-1
// invoice is issued at T, and the card charge the catchup makes for it is
// anchored at T as well, so the invoice is paid when it was issued.
//
// Binding the catchup to trial_end_at alone (without buildOnCreateInvoice
// issuing at the startAt it is given) splits the two: the charge anchor and
// paid_at land on T while issued_at stays at T+2d, and the invoice reads as
// paid two days before it was issued. Mutation check: read the clock instead
// of startAt in buildOnCreateInvoice and this fails on paid_at < issued_at.
func TestTrialExpiry_TestClock_ChargeNotBeforeIssue(t *testing.T) {
	r := newCancelRig(t, "trial expiry card dates")
	f := r.f
	trialEnd := f.cycleStart.Add(48 * time.Hour)
	frozen := trialEnd.Add(48 * time.Hour)

	r.exec(t, `UPDATE plans SET base_bill_timing = 'in_advance' WHERE id = $1`, f.planID)
	r.exec(t, `DELETE FROM billing_intervals WHERE subscription_item_id = $1`, f.itemID)
	r.exec(t, `UPDATE subscriptions SET status = 'trialing', trial_start_at = $1, trial_end_at = $2, activated_at = NULL,
		current_billing_period_start = $2, current_billing_period_end = $3, next_billing_at = $3 WHERE id = $4`,
		f.cycleStart, trialEnd, trialEnd.AddDate(0, 1, 0), f.subID)

	clockID := postgres.NewID("hc_tclk")
	execTx(t, f.db, r.base, f.tenantID,
		`INSERT INTO test_clocks (id, tenant_id, livemode, name, frozen_time, status) VALUES ($1, $2, false, 'trial card clock', $3, 'advancing')`,
		clockID, f.tenantID, frozen)
	r.exec(t, `UPDATE customers SET test_clock_id = $1 WHERE id = $2`, clockID, f.customerID)
	r.exec(t, `UPDATE subscriptions SET test_clock_id = $1 WHERE id = $2`, clockID, f.subID)

	// The engine as router.go wires it, with a card on file and the
	// anchored charger in place of Stripe.
	charger := &anchoredCharger{invoices: f.invStore}
	clocks := testclock.NewPostgresStore(f.db)
	engine := billing.NewEngine(
		&subStoreAdapter{f.subStore}, &usageStoreAdapter{usage.NewPostgresStore(f.db)},
		&pricingStoreAdapter{pricing.NewPostgresStore(f.db)}, &invoiceStoreAdapter{f.invStore},
		nil, f.settings, cardOnFile{}, charger, nil,
	)
	engine.SetIntervalReader(f.subStore)
	engine.SetTaxProviderResolver(tax.NewResolver(nil))
	engine.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	engine.SetDunningStarter(testDunningStarter{})
	engine.SetDunningResolver(&testDunningResolver{})
	engine.SetTestClockReader(clocks)
	engine.SetAuditLogger(audit.NewLogger(f.db))
	f.subSvc.SetBiller(engine)
	f.subSvc.SetDueBiller(engine)
	f.subSvc.SetResolver(engine)

	orchestrator := testclock.NewService(clocks)
	orchestrator.SetBillingRunner(engine)
	orchestrator.SetTrialExpirer(f.subSvc)
	if err := orchestrator.RunCatchup(r.base, testclock.CatchupJob{TenantID: f.tenantID, ClockID: clockID, PrevFrozenTime: f.cycleStart}); err != nil {
		t.Fatalf("catchup: %v", err)
	}

	invs, _ := r.liveInvoices(t)
	inv := invoiceFor(t, invs, trialEnd, trialEnd.AddDate(0, 1, 0))
	if inv.IssuedAt == nil || !inv.IssuedAt.Equal(trialEnd) {
		t.Errorf("day-1 invoice issued_at = %v, want the trial end %v", inv.IssuedAt, trialEnd)
	}
	if len(charger.anchors) != 1 || !charger.anchors[0].Equal(trialEnd) {
		t.Errorf("charge anchors %v, want one at the trial end %v", charger.anchors, trialEnd)
	}
	if inv.Status != domain.InvoicePaid || inv.PaidAt == nil {
		t.Fatalf("day-1 invoice status=%s paid_at=%v, want paid by the card charge", inv.Status, inv.PaidAt)
	}
	if inv.IssuedAt != nil && inv.PaidAt.Before(*inv.IssuedAt) {
		t.Errorf("day-1 invoice paid_at %v is before its issued_at %v: paid before it was issued", inv.PaidAt, inv.IssuedAt)
	}
}

// TestThresholdFire_FinalizeRowIsBornFinalized is P-1 for the spend
// threshold: the invoice a crossed threshold issues is created finalized, so
// its finalize audit row carries born_finalized: true.
func TestThresholdFire_FinalizeRowIsBornFinalized(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold Born Finalized")
	f.engine.SetAuditLogger(audit.NewLogger(f.db))
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	f.ingestUsage(t, ctx, 100, 10)
	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{AmountGTE: 500}); err != nil {
		t.Fatalf("set threshold: %v", err)
	}
	if fired, errs := f.engine.ScanThresholds(ctx, 50); fired != 1 || len(errs) != 0 {
		t.Fatalf("threshold scan: fired=%d errs=%v, want 1 and none", fired, errs)
	}
	invs := f.listInvoices(t, ctx)
	if len(invs) != 1 || invs[0].BillingReason != domain.BillingReasonThreshold {
		t.Fatalf("invoices %+v, want one threshold invoice", invs)
	}

	var born sql.NullBool
	if err := testutil.AdminPool(t).QueryRow(
		`SELECT (metadata->>'born_finalized')::boolean FROM audit_log WHERE resource_type = 'invoice' AND resource_id = $1 AND action = $2`,
		invs[0].ID, domain.AuditActionFinalize).Scan(&born); err != nil {
		t.Fatalf("finalize row: %v", err)
	}
	if !born.Valid || !born.Bool {
		t.Errorf("threshold invoice finalize row born_finalized = %v, want true", born)
	}
}
