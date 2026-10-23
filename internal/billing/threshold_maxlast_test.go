package billing

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// recordingAudit captures engine audit rows so the once-per-cycle loudness
// floor can be asserted.
type recordingAudit struct {
	rows  []string         // action names, in order
	metas []map[string]any // parallel metadata, for reason-scoped assertions
}

func (r *recordingAudit) Log(_ context.Context, _, action, _, _, _ string, meta map[string]any) error {
	r.rows = append(r.rows, action)
	r.metas = append(r.metas, meta)
	return nil
}

// LogInTx routes to the same recorder: the threshold FINALIZE row now rides the
// invoice-create tx (ADR-090), so without this it would vanish from the recording
// it used to reach via Log, and the once-per-cycle assertions would silently lose
// a row they mean to see.
func (r *recordingAudit) LogInTx(_ context.Context, _ *sql.Tx, e audit.Entry) error {
	r.rows = append(r.rows, e.Action)
	r.metas = append(r.metas, e.Metadata)
	return nil
}

// setupMaxLastEngine wires the threshold fixture with a mixed-mode meter: one
// sum bucket (rrv_api, 1000 calls @ $1 = 100000c) and one max bucket
// (rrv_max, peak 50 @ $1 = 5000c), plus the plan's 4900c in_arrears base.
// Bucket modes ride RuleAggregation.AggregationMode exactly as the real
// store's COALESCE(rule_mode, default) emits them.
func setupMaxLastEngine(thresholds *domain.BillingThresholds) (*Engine, *thresholdMockSubs, *mockInvoices) {
	engine, subs, invoices := setupThresholdEngine(thresholds, 1000)
	engine.clock = clock.NewFake(time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC))
	mp := engine.pricing.(*mockPricing)
	mp.rules["rrv_max"] = domain.RatingRuleVersion{
		ID: "rrv_max", RuleKey: "gpu_peak", Version: 1, Mode: domain.PricingFlat,
		FlatAmountCents: decimal.NewFromInt(100), Currency: "USD",
	}
	mu := engine.usage.(*mockUsage)
	mu.ruleAggs = map[string][]domain.RuleAggregation{
		"mtr_api": {
			{RatingRuleVersionID: "rrv_api", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(1000)},
			{RatingRuleVersionID: "rrv_max", AggregationMode: domain.AggMax, Quantity: decimal.NewFromInt(50)},
		},
	}
	return engine, subs, invoices
}

// TestFireThreshold_ResetFalse_DropsMaxBucket_SubtotalSplit locks ADR-066 §4:
// the max bucket's line is DROPPED from the threshold
// invoice (the cycle close bills it full-period exactly once), its amount
// still counts toward the cap, and the invoice header charges ONLY the
// rendered lines — no phantom money. Mutation seams: classify per meter
// instead of per bucket → the max line rides; feed RunningSubtotal to the
// invoice → subtotal 109900.
func TestFireThreshold_ResetFalse_DropsMaxBucket_SubtotalSplit(t *testing.T) {
	// capRunning = 4900 base + 100000 sum + 5000 max = 109900 ≥ 107000: the
	// cap crosses ONLY because the max bucket counts (sum-only is 104900).
	thresholds := &domain.BillingThresholds{AmountGTE: 107000}
	engine, _, invoices := setupMaxLastEngine(thresholds)

	fired, errs := engine.ScanThresholds(context.Background(), 50)
	if len(errs) != 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1 (max amount must count toward the cap)", fired)
	}
	inv := invoices.invoices[0]
	// Header charges rendered lines only: base 4900 + sum 100000.
	if inv.SubtotalCents != 104900 || inv.TotalAmountCents != 104900 {
		t.Errorf("invoice subtotal/total = %d/%d, want 104900 (dropped max bucket must not be charged)", inv.SubtotalCents, inv.TotalAmountCents)
	}
	var lineSum int64
	for _, li := range invoices.lineItems {
		if li.RatingRuleVersionID == "rrv_max" {
			t.Errorf("max bucket line rode the threshold invoice: %+v", li)
		}
		lineSum += li.AmountCents
	}
	if lineSum != inv.SubtotalCents {
		t.Errorf("header subtotal %d != sum of rendered lines %d", inv.SubtotalCents, lineSum)
	}
}

// TestScanThresholds_PureMaxCrossing_SkipsWithOneArtifact locks ADR-066 §4b:
// a pure-max sub whose committed spend crosses the cap emits NO invoice
// (empty billable set — cycle close bills the bucket), NO per-tick errors
// (the pre-fix currency bail errored every tick forever), consumes NO invoice
// numbers, and leaves exactly ONE audit artifact across repeated ticks.
func TestScanThresholds_PureMaxCrossing_SkipsWithOneArtifact(t *testing.T) {
	thresholds := &domain.BillingThresholds{AmountGTE: 1000}
	engine, _, invoices := setupMaxLastEngine(thresholds)
	// Pure max: strip the sum bucket and the base fee.
	mu := engine.usage.(*mockUsage)
	mu.ruleAggs["mtr_api"] = []domain.RuleAggregation{
		{RatingRuleVersionID: "rrv_max", AggregationMode: domain.AggMax, Quantity: decimal.NewFromInt(50)},
	}
	mp := engine.pricing.(*mockPricing)
	pln := mp.plans["pln_1"]
	pln.BaseAmountCents = 0
	mp.plans["pln_1"] = pln
	audit := &recordingAudit{}
	engine.SetAuditLogger(audit)

	for tick := 0; tick < 3; tick++ {
		fired, errs := engine.ScanThresholds(context.Background(), 50)
		if len(errs) != 0 {
			t.Fatalf("tick %d errors: %v (crossed-but-deferred must not error)", tick, errs)
		}
		if fired != 0 {
			t.Fatalf("tick %d fired = %d, want 0", tick, fired)
		}
	}
	if len(invoices.invoices) != 0 {
		t.Fatalf("invoices = %d, want 0", len(invoices.invoices))
	}
	deferred := 0
	for _, a := range audit.rows {
		if a == "subscription.threshold_deferred" {
			deferred++
		}
	}
	if deferred != 1 {
		t.Errorf("threshold_deferred audit rows = %d across 3 ticks, want exactly 1 (loudness floor, not tick-spam)", deferred)
	}
	// No invoice number consumed by the deferral path.
	ms := engine.settings.(*mockSettings)
	if ms.next != 0 {
		t.Errorf("deferral consumed %d invoice numbers, want 0 (guard must sit before NextInvoiceNumber)", ms.next)
	}
}

// TestNonAdditiveMode_Classification pins the non-additive set (ADR-066 §4):
// max + BOTH last variants (the original plan forgot last_ever, which
// ignores window bounds entirely). sum/count stay additive.
func TestNonAdditiveMode_Classification(t *testing.T) {
	for mode, want := range map[domain.AggregationMode]bool{
		domain.AggMax:              true,
		domain.AggLastDuringPeriod: true,
		domain.AggLastEver:         true,
		domain.AggSum:              false,
		domain.AggCount:            false,
		domain.AggregationMode(""): false,
	} {
		if got := nonAdditiveMode(mode); got != want {
			t.Errorf("nonAdditiveMode(%q) = %v, want %v", mode, got, want)
		}
	}
}

// TestBillOnePeriod_DeferredMaxBucket_BilledExactlyOnceAtClose locks the
// close-side pass routing (ADR-066 §4): with a threshold watermark whose
// invoice carries the SUM bucket's line but not the MAX bucket's (the fire
// deferred it), the cycle close bills each bucket EXACTLY once — sum from the
// clamped pass, max from the full-window pass — and the routing keys on the
// watermark invoice's LINES (ground truth), not the sub's threshold config
// (which this test leaves entirely unset, as if the operator cleared it
// between fire and close). Mutation seam: drop the pass filter and each
// bucket bills twice.
func TestBillOnePeriod_DeferredMaxBucket_BilledExactlyOnceAtClose(t *testing.T) {
	// No thresholds on the sub config — ground truth must come from the
	// watermark invoice alone.
	engine, _, invoices := setupMaxLastEngine(nil)
	engine.clock = clock.NewFake(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)) // due
	// billOnePeriod's multi-dim branch requires meter pricing rules.
	mp := engine.pricing.(*mockPricing)
	mp.meterPricingRules = map[string][]domain.MeterPricingRule{
		"mtr_api": {
			{ID: "mpr_sum", MeterID: "mtr_api", RatingRuleVersionID: "rrv_api", AggregationMode: domain.AggSum},
			{ID: "mpr_max", MeterID: "mtr_api", RatingRuleVersionID: "rrv_max", AggregationMode: domain.AggMax},
		},
	}

	// Watermark: a reset=false threshold fire billed the sum bucket through
	// Apr 10; the max bucket was deferred (no line).
	periodStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	fireAt := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	invoices.invoices = append(invoices.invoices, domain.Invoice{
		ID: "vlx_inv_fire", SubscriptionID: "sub_1", TenantID: "t1",
		Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
		TaxFacts:           domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		BillingReason:      domain.BillingReasonThreshold,
		BillingPeriodStart: periodStart,
		BillingPeriodEnd:   fireAt,
	})
	invoices.lineItems = append(invoices.lineItems, domain.InvoiceLineItem{
		InvoiceID: "vlx_inv_fire", LineType: domain.LineTypeUsage,
		MeterID: "mtr_api", RatingRuleVersionID: "rrv_api", AmountCents: 40000,
	})

	generated, failures := engine.RunCycleForTenant(context.Background(), "t1", 50)
	if len(failures) != 0 {
		t.Fatalf("cycle failures: %v", failures)
	}
	if generated != 1 {
		t.Fatalf("generated = %d, want 1", generated)
	}

	var sumLines, maxLines int
	var cycleInvoiceID string
	for _, inv := range invoices.invoices {
		if inv.BillingReason != domain.BillingReasonThreshold {
			cycleInvoiceID = inv.ID
		}
	}
	for _, li := range invoices.lineItems {
		if li.InvoiceID != cycleInvoiceID || li.LineType != domain.LineTypeUsage {
			continue
		}
		switch li.RatingRuleVersionID {
		case "rrv_api":
			sumLines++
		case "rrv_max":
			maxLines++
		}
	}
	if sumLines != 1 {
		t.Errorf("sum bucket lines on cycle invoice = %d, want exactly 1 (clamped pass)", sumLines)
	}
	if maxLines != 1 {
		t.Errorf("deferred max bucket lines on cycle invoice = %d, want exactly 1 (full-window pass; 0 = billed by NOBODY, 2 = double-billed)", maxLines)
	}
}
