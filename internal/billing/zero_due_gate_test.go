package billing

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// setupZeroCycleEngine: a due sub on a free-rated plan ($0 base + $0/call
// with usage) so cycle close emits a $0 invoice with zero-priced usage lines
// (the engine's zero-amount line convention).
func setupZeroCycleEngine(t *testing.T) (*Engine, *thresholdMockSubs, *mockInvoices) {
	t.Helper()
	engine, subs, invoices := setupThresholdEngine(nil, 1000)
	engine.clock = clock.NewFake(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)) // == next_billing_at
	engine.SetDunningResolver(&recordingDunningResolver{})                    // $0 auto-paid settle resolves dunning (required post-#442)
	mp := engine.pricing.(*mockPricing)
	pln := mp.plans["pln_1"]
	pln.BaseAmountCents = 0
	mp.plans["pln_1"] = pln
	mp.prices["pln_1"][0].UnitAmountCents = decimal.Zero
	return engine, subs, invoices
}

// TestBillOnePeriod_ZeroTotal_AutoPaid is T12 for the CYCLE writer: a $0
// cycle-close invoice (free-rated usage) must be auto-marked paid, exactly
// like the threshold writer — pre-fix the `totalWithTax > 0` conjunct
// stranded it payment_pending forever (never charged: amount_due=0 skips the
// charge arm; never paid; permanently "awaiting payment"). Mutation seam:
// restore the conjunct in billOnePeriod's gate and this fails.
func TestBillOnePeriod_ZeroTotal_AutoPaid(t *testing.T) {
	engine, subs, invoices := setupZeroCycleEngine(t)

	generated, failures := engine.RunCycleForTenant(postgres.WithLivemode(context.Background(), true), "t1", 50)
	if len(failures) != 0 {
		t.Fatalf("cycle failures: %v", failures)
	}
	if generated != 1 {
		t.Fatalf("generated = %d, want 1", generated)
	}
	if len(invoices.invoices) != 1 {
		t.Fatalf("invoices = %d, want 1", len(invoices.invoices))
	}
	inv := invoices.invoices[0]
	if inv.TotalAmountCents != 0 {
		t.Fatalf("total = %d, want 0 (free-rated plan)", inv.TotalAmountCents)
	}
	if inv.Status != domain.InvoicePaid || inv.PaymentStatus != domain.PaymentSucceeded {
		t.Errorf("$0 cycle invoice stranded: status=%q payment=%q, want paid/succeeded", inv.Status, inv.PaymentStatus)
	}
	if !subs.cycleUpdated["sub_1"] {
		t.Error("cycle must still advance after the $0 auto-pay")
	}
}
