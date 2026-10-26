package billing

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
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

	generated, failures := engine.RunCycleForTenant(context.Background(), "t1", 50)
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

// TestBillOnePeriod_AlreadyExistsHealsStrandedZeroDue: a crash between the
// cycle invoice's create and its $0 MarkPaid re-enters through the
// ErrAlreadyExists branch (the sub is still due — the advance also crashed).
// That branch must heal the stranded payment_pending row BEFORE advancing
// (ADR-066). Mutation seam: drop the heal call from the branch and this
// fails.
func TestBillOnePeriod_AlreadyExistsHealsStrandedZeroDue(t *testing.T) {
	engine, subs, invoices := setupZeroCycleEngine(t)

	// Seed the stranded state: the $0 cycle invoice committed, then the
	// process died before MarkPaid and before the cycle advance.
	periodStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	invoices.invoices = append(invoices.invoices, domain.Invoice{
		ID: "vlx_inv_stranded_cycle", SubscriptionID: "sub_1", TenantID: "t1",
		Status:        domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending,
		TaxFacts: domain.TaxFacts{
			TaxStatus: domain.InvoiceTaxOK,
		},
		AmountDueCents:     0,
		BillingReason:      domain.BillingReasonSubscriptionCycle,
		BillingPeriodStart: periodStart,
		BillingPeriodEnd:   periodEnd,
	})
	// The unique index rejects the re-created invoice on this tick.
	invoices.createErr = errs.ErrAlreadyExists

	generated, failures := engine.RunCycleForTenant(context.Background(), "t1", 50)
	if len(failures) != 0 {
		t.Fatalf("cycle failures: %v", failures)
	}
	if generated != 0 {
		t.Fatalf("generated = %d, want 0 (idempotent skip)", generated)
	}
	healed := invoices.invoices[0]
	if healed.Status != domain.InvoicePaid || healed.PaymentStatus != domain.PaymentSucceeded {
		t.Errorf("stranded $0 cycle invoice not healed on AlreadyExists re-entry: status=%q payment=%q",
			healed.Status, healed.PaymentStatus)
	}
	if !subs.cycleUpdated["sub_1"] {
		t.Error("cycle advance heal must still run after the zero-due heal")
	}
}
