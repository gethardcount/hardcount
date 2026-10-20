package billing_test

// #1193 on real Postgres. Ingest accepts negative quantities as usage
// corrections, so a usage bucket can total below zero over a billing window.
// The period close and the cancel's final invoice failed on such a bucket
// ("compute amount for meter …: invalid pricing config"), so no invoice was
// made at all. Now the bucket bills 0, the rest of the invoice is built, and
// one subscription.net_negative_usage audit row on the decision's own
// transaction names each meter and its amount below zero. The rows are read
// the way the subscription's activity timeline reads them.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
)

// netNegativeNotes returns the subscription.net_negative_usage rows of one
// subscription, through the audit query its activity timeline uses.
func netNegativeNotes(t *testing.T, ctx context.Context, logger *audit.Logger, tenantID, subID string) []domain.AuditEntry {
	t.Helper()
	entries, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
		ResourceType: "subscription", ResourceID: subID, Action: "subscription.net_negative_usage", Limit: 100,
	})
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	return entries
}

// noteQuantities maps meter_key → quantity from one row's metadata, as it
// comes back from JSON.
func noteQuantities(t *testing.T, e domain.AuditEntry) map[string]string {
	t.Helper()
	ms, ok := e.Metadata["meters"].([]any)
	if !ok {
		t.Fatalf("meters metadata: got %T (%v), want a list", e.Metadata["meters"], e.Metadata["meters"])
	}
	out := map[string]string{}
	for _, m := range ms {
		mm, _ := m.(map[string]any)
		key, _ := mm["meter_key"].(string)
		qty, _ := mm["quantity"].(string)
		out[key] = qty
	}
	return out
}

// noteFrom returns the start of the window one meter's usage below zero was
// priced over, from one row's metadata.
func noteFrom(t *testing.T, e domain.AuditEntry, meterKey string) string {
	t.Helper()
	ms, _ := e.Metadata["meters"].([]any)
	for _, m := range ms {
		mm, _ := m.(map[string]any)
		if mm["meter_key"] == meterKey {
			from, _ := mm["from"].(string)
			return from
		}
	}
	t.Fatalf("row has no entry for %s: %v", meterKey, e.Metadata["meters"])
	return ""
}

// seatsMeter creates a single-rule sum meter priced at 5c a seat.
func seatsMeter(t *testing.T, ctx context.Context, f *thresholdFixture, key string) domain.Meter {
	t.Helper()
	rr, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: key + "_flat", Name: key, Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	m, err := f.pricingSvc.CreateMeter(ctx, f.tenantID, pricing.CreateMeterInput{
		Key: key, Name: "Seats", Unit: "seats", Aggregation: "sum", RatingRuleVersionID: rr.ID,
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	return m
}

// netNegativePlan is one plan with three meters: the fixture's calls meter
// (single rule, 1c a call), a tokens meter priced by a pricing rule (1c a
// model=a token) and a seats meter (single rule, 5c a seat).
func netNegativePlan(t *testing.T, ctx context.Context, f *thresholdFixture, suffix string) (tokens, seats domain.Meter, planID string) {
	t.Helper()
	tokens = tokensMeter(t, ctx, f, "tok_"+suffix)
	seats = seatsMeter(t, ctx, f, "seats_"+suffix)
	plan, err := f.pricingSvc.CreatePlan(ctx, f.tenantID, pricing.CreatePlanInput{
		Code: "pln_neg_" + suffix, Name: "Neg " + suffix, Currency: "USD",
		BillingInterval: domain.BillingMonthly, MeterIDs: []string{f.meterID, tokens.ID, seats.ID},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return tokens, seats, plan.ID
}

// TestNetNegativeUsage_CycleClose_BillsZeroAndRecordsIt: calls bill as usual;
// the tokens bucket (pricing-rule path) nets -50 and seats (single-rule path)
// net -7. The invoice is made with the calls line only, and one row records
// both meters below zero.
func TestNetNegativeUsage_CycleClose_BillsZeroAndRecordsIt(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Net Negative Cycle")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 120*time.Second)
	defer cancel()
	logger := audit.NewLogger(f.db)
	f.engine.SetAuditLogger(logger)

	tokens, seats, planID := netNegativePlan(t, ctx, f, "cyc")
	custID, subID := seedUnpricedSub(t, ctx, f, "cus_neg_cyc", planID, 0)
	ingestUnpricedAt(t, ctx, f, custID, f.meterID, 300, f.cycleStart.Add(time.Minute), nil)
	ingestUnpricedAt(t, ctx, f, custID, tokens.ID, 20, f.cycleStart.Add(2*time.Minute), map[string]any{"model": "a"})
	ingestUnpricedAt(t, ctx, f, custID, tokens.ID, -70, f.cycleStart.Add(3*time.Minute), map[string]any{"model": "a"})
	ingestUnpricedAt(t, ctx, f, custID, seats.ID, 3, f.cycleStart.Add(4*time.Minute), nil)
	ingestUnpricedAt(t, ctx, f, custID, seats.ID, -10, f.cycleStart.Add(5*time.Minute), nil)

	endPeriodNow(t, ctx, f, subID)
	generated, failures := f.engine.RunCycleForTenant(ctx, f.tenantID, 50)
	if len(failures) > 0 {
		t.Fatalf("cycle failures: %v (usage below zero must not fail the close)", failures)
	}
	if generated != 1 {
		t.Fatalf("generated = %d, want 1", generated)
	}
	invs, _, err := f.invStore.List(ctx, invoice.ListFilter{TenantID: f.tenantID, SubscriptionID: subID})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	if len(invs) != 1 {
		t.Fatalf("invoices = %d, want 1", len(invs))
	}
	inv := invs[0]
	if inv.SubtotalCents != 300 {
		t.Errorf("subtotal = %d, want 300 (calls only; tokens and seats at 0)", inv.SubtotalCents)
	}
	lines, err := f.invStore.ListLineItems(ctx, f.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("list lines: %v", err)
	}
	if len(lines) != 1 || lines[0].MeterID != f.meterID || lines[0].Quantity != 300 || lines[0].AmountCents != 300 {
		t.Fatalf("lines = %+v, want one calls line of 300 × 1¢", lines)
	}

	notes := netNegativeNotes(t, ctx, logger, f.tenantID, subID)
	if len(notes) != 1 {
		t.Fatalf("net-negative rows = %d, want 1", len(notes))
	}
	got := noteQuantities(t, notes[0])
	if len(got) != 2 || got["tok_cyc"] != "-50" || got["seats_cyc"] != "-7" {
		t.Errorf("meters = %v, want tok_cyc: -50 and seats_cyc: -7", got)
	}
	if notes[0].Metadata["invoice_id"] != inv.ID || notes[0].Metadata["billing_reason"] != string(domain.BillingReasonSubscriptionCycle) {
		t.Errorf("row metadata = %v, want invoice_id %s and billing_reason subscription_cycle", notes[0].Metadata, inv.ID)
	}
}

// TestNetNegativeUsage_ImmediateCancelTx_BillsZeroAndRecordsIt runs the
// cancel's production path (the caller's transaction). The final invoice
// carries the calls line, and the row rides the same transaction: an attempt
// that rolls back leaves no row, and the retry that commits leaves one.
func TestNetNegativeUsage_ImmediateCancelTx_BillsZeroAndRecordsIt(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Net Negative Cancel")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 120*time.Second)
	defer cancel()
	logger := audit.NewLogger(f.db)
	f.engine.SetAuditLogger(logger)

	tokens, seats, planID := netNegativePlan(t, ctx, f, "cxl")
	custID, subID := seedUnpricedSub(t, ctx, f, "cus_neg_cxl", planID, 0)
	ingestUnpricedAt(t, ctx, f, custID, f.meterID, 40, f.cycleStart.Add(time.Minute), nil)
	ingestUnpricedAt(t, ctx, f, custID, tokens.ID, -25, f.cycleStart.Add(2*time.Minute), map[string]any{"model": "a"})
	ingestUnpricedAt(t, ctx, f, custID, seats.ID, -2, f.cycleStart.Add(3*time.Minute), nil)

	canceledAt := time.Now().UTC().Truncate(time.Microsecond)
	sub, err := f.subStore.Get(ctx, f.tenantID, subID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	sub.Status = domain.SubscriptionCanceled
	sub.CanceledAt = &canceledAt
	attempt := func() (domain.Invoice, *sql.Tx) {
		tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET status = 'canceled', canceled_at = $1 WHERE id = $2`, canceledAt, subID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		inv, _, err := f.engine.BillFinalOnImmediateCancelTx(ctx, tx, sub)
		if err != nil {
			postgres.Rollback(tx)
			t.Fatalf("bill final (tx): %v (usage below zero must not fail the cancel)", err)
		}
		return inv, tx
	}

	_, tx := attempt()
	postgres.Rollback(tx)
	if n := len(netNegativeNotes(t, ctx, logger, f.tenantID, subID)); n != 0 {
		t.Fatalf("after a rolled-back cancel: rows = %d, want 0", n)
	}

	inv, tx := attempt()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if inv.ID == "" || inv.SubtotalCents != 40 {
		t.Fatalf("final invoice = %q subtotal %d, want one invoice of 40¢ (calls only)", inv.ID, inv.SubtotalCents)
	}
	notes := netNegativeNotes(t, ctx, logger, f.tenantID, subID)
	if len(notes) != 1 {
		t.Fatalf("net-negative rows = %d, want 1", len(notes))
	}
	got := noteQuantities(t, notes[0])
	if len(got) != 2 || got["tok_cxl"] != "-25" || got["seats_cxl"] != "-2" {
		t.Errorf("meters = %v, want tok_cxl: -25 and seats_cxl: -2", got)
	}
	if notes[0].Metadata["invoice_id"] != inv.ID || notes[0].Metadata["billing_reason"] != string(domain.BillingReasonSubscriptionCancel) {
		t.Errorf("row metadata = %v, want invoice_id %s and billing_reason subscription_cancel", notes[0].Metadata, inv.ID)
	}
}

// TestNetNegativeUsage_ThresholdFire_RecordedOnce: a spend-threshold invoice
// already left a bucket below zero off (preview rule), but said so nowhere.
// The fire now records it on its own transaction, and the period close, which
// bills only after the fire, does not record it again.
func TestNetNegativeUsage_ThresholdFire_RecordedOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Net Negative Threshold")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 120*time.Second)
	defer cancel()
	logger := audit.NewLogger(f.db)
	f.engine.SetAuditLogger(logger)

	tokens, _, planID := netNegativePlan(t, ctx, f, "thr")
	custID, subID := seedUnpricedSub(t, ctx, f, "cus_neg_thr", planID, 500)
	for j := 0; j < 10; j++ {
		ingestUnpricedAt(t, ctx, f, custID, f.meterID, 100, f.cycleStart.Add(time.Duration(j+1)*time.Minute), nil)
	}
	ingestUnpricedAt(t, ctx, f, custID, tokens.ID, 10, f.cycleStart.Add(20*time.Minute), map[string]any{"model": "a"})
	ingestUnpricedAt(t, ctx, f, custID, tokens.ID, -40, f.cycleStart.Add(21*time.Minute), map[string]any{"model": "a"})

	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 || fired != 1 {
		t.Fatalf("fired = %d, errs = %v; want one fire", fired, errs)
	}
	invs, _, err := f.invStore.List(ctx, invoice.ListFilter{TenantID: f.tenantID, SubscriptionID: subID})
	if err != nil || len(invs) != 1 {
		t.Fatalf("invoices = %d (err %v), want the threshold invoice", len(invs), err)
	}
	fire := invs[0]
	if fire.SubtotalCents != 1000 {
		t.Errorf("threshold invoice subtotal = %d, want 1000 (calls only)", fire.SubtotalCents)
	}
	notes := netNegativeNotes(t, ctx, logger, f.tenantID, subID)
	if len(notes) != 1 {
		t.Fatalf("after the fire: rows = %d, want 1", len(notes))
	}
	if got := noteQuantities(t, notes[0]); got["tok_thr"] != "-30" {
		t.Errorf("meters = %v, want tok_thr: -30", got)
	}
	if notes[0].Metadata["invoice_id"] != fire.ID || notes[0].Metadata["billing_reason"] != string(domain.BillingReasonThreshold) {
		t.Errorf("row metadata = %v, want invoice_id %s and billing_reason threshold", notes[0].Metadata, fire.ID)
	}
	if from, want := noteFrom(t, notes[0], "tok_thr"), f.cycleStart.UTC().Format(time.RFC3339Nano); from != want {
		t.Errorf("tok_thr from = %q, want the period start %s (the fire priced the period so far)", from, want)
	}

	time.Sleep(10 * time.Millisecond)
	endPeriodNow(t, ctx, f, subID)
	if _, failures := f.engine.RunCycleForTenant(ctx, f.tenantID, 50); len(failures) > 0 {
		t.Fatalf("cycle failures: %v", failures)
	}
	if n := len(netNegativeNotes(t, ctx, logger, f.tenantID, subID)); n != 1 {
		t.Fatalf("after the close: rows = %d, want 1 (the close bills only after the fire)", n)
	}
}

// TestNetNegativeUsage_CycleClose_UsageCapCountsWhatBills: a block usage cap
// totals what the close bills. The tokens meter's model=a bucket is +150 and
// its bucket of events no rule matched is -50, so the meter's total is +100.
// The invoice bills the +150 bucket only; with a cap of 80 it must bill 80
// tokens, not 120 (the +150 bucket scaled by 80/100).
func TestNetNegativeUsage_CycleClose_UsageCapCountsWhatBills(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Net Negative Cap")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 120*time.Second)
	defer cancel()
	logger := audit.NewLogger(f.db)
	f.engine.SetAuditLogger(logger)

	tokens := tokensMeter(t, ctx, f, "tok_cap")
	plan, err := f.pricingSvc.CreatePlan(ctx, f.tenantID, pricing.CreatePlanInput{
		Code: "pln_neg_cap", Name: "Neg cap", Currency: "USD",
		BillingInterval: domain.BillingMonthly, MeterIDs: []string{tokens.ID},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	custID, subID := seedUnpricedSub(t, ctx, f, "cus_neg_cap", plan.ID, 0)
	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET usage_cap_units = 80, overage_action = 'block' WHERE id = $1`, subID); err != nil {
		postgres.Rollback(tx)
		t.Fatalf("set cap: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ingestUnpricedAt(t, ctx, f, custID, tokens.ID, 150, f.cycleStart.Add(time.Minute), map[string]any{"model": "a"})
	ingestUnpricedAt(t, ctx, f, custID, tokens.ID, -50, f.cycleStart.Add(2*time.Minute), map[string]any{"model": "b"})

	endPeriodNow(t, ctx, f, subID)
	if _, failures := f.engine.RunCycleForTenant(ctx, f.tenantID, 50); len(failures) > 0 {
		t.Fatalf("cycle failures: %v", failures)
	}
	invs, _, err := f.invStore.List(ctx, invoice.ListFilter{TenantID: f.tenantID, SubscriptionID: subID})
	if err != nil || len(invs) != 1 {
		t.Fatalf("invoices = %d (err %v), want 1", len(invs), err)
	}
	if invs[0].SubtotalCents != 80 {
		t.Errorf("subtotal = %d, want 80 (80 tokens at the block cap of 80)", invs[0].SubtotalCents)
	}
	notes := netNegativeNotes(t, ctx, logger, f.tenantID, subID)
	if len(notes) != 1 {
		t.Fatalf("net-negative rows = %d, want 1", len(notes))
	}
	if got := noteQuantities(t, notes[0]); got["tok_cap"] != "-50" {
		t.Errorf("meters = %v, want tok_cap: -50", got)
	}
}
