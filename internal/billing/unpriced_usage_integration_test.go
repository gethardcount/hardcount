package billing_test

// #981: usage that matches no price is left off the invoice (the peer norm),
// but never silently. Each invoice that is built without it logs a warning
// and adds its quantity to velox_usage_unpriced_quantity_total{meter_id}.
// These tests run the real engine on real Postgres and read the counter from
// the default Prometheus registry. Meter ids are unique per test, so the
// counter of one test cannot leak into another.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// unpricedCounter reads velox_usage_unpriced_quantity_total for one meter
// in test mode (every test here runs with livemode=false); 0 when the
// series does not exist.
func unpricedCounter(t *testing.T, meterID string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "velox_usage_unpriced_quantity_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var meterOK, modeOK bool
			for _, lp := range m.GetLabel() {
				switch lp.GetName() {
				case "meter_id":
					meterOK = lp.GetValue() == meterID
				case "mode":
					modeOK = lp.GetValue() == "test"
				}
			}
			if meterOK && modeOK {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// tokensMeter creates a sum meter with ONE pricing rule {model: a} at 1c and
// no default binding, so model=b usage matches no price.
func tokensMeter(t *testing.T, ctx context.Context, f *thresholdFixture, key string) domain.Meter {
	t.Helper()
	rr, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: key + "_a", Name: key + " a", Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	m, err := f.pricingSvc.CreateMeter(ctx, f.tenantID, pricing.CreateMeterInput{
		Key: key, Name: key, Unit: "tokens", Aggregation: "sum",
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	if _, err := f.pricingSvc.UpsertMeterPricingRule(ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
		MeterID: m.ID, RatingRuleVersionID: rr.ID, DimensionMatch: map[string]any{"model": "a"},
	}); err != nil {
		t.Fatalf("bind rule: %v", err)
	}
	return m
}

// seedUnpricedSub creates a customer and an active subscription on planID
// over [f.cycleStart, f.cycleEnd). amountGTE > 0 sets a spend threshold
// (reset_billing_cycle=false).
func seedUnpricedSub(t *testing.T, ctx context.Context, f *thresholdFixture, ext, planID string, amountGTE int64) (custID, subID string) {
	t.Helper()
	cust, err := customer.NewService(customer.NewPostgresStore(f.db)).Create(ctx, f.tenantID, customer.CreateInput{
		ExternalID: ext, DisplayName: ext, Email: ext + "@example.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	subID = postgres.NewID("vlx_sub")
	itemID := postgres.NewID("vlx_si")
	var gte any
	if amountGTE > 0 {
		gte = amountGTE
	}
	now := time.Now().UTC()
	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscriptions (
			id, tenant_id, code, display_name, customer_id, status, billing_time,
			current_billing_period_start, current_billing_period_end, next_billing_at,
			billing_threshold_amount_gte, billing_threshold_reset_cycle,
			created_at, updated_at
		) VALUES ($1, $2, $3, $3, $4, 'active', 'anniversary', $5, $6, $6, $7, FALSE, $8, $8)
	`, subID, f.tenantID, "code-"+ext, cust.ID, f.cycleStart, f.cycleEnd, gte, now); err != nil {
		t.Fatalf("insert sub: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 1, '{}'::jsonb, $5, $5)
	`, itemID, f.tenantID, subID, planID, f.cycleStart); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO billing_intervals (tenant_id, subscription_id, subscription_item_id, plan_id, quantity, starts_at, source)
		VALUES ($1, $2, $3, $4, 1, $5, 'create')
	`, f.tenantID, subID, itemID, planID, f.cycleStart); err != nil {
		t.Fatalf("insert interval: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return cust.ID, subID
}

func ingestUnpricedAt(t *testing.T, ctx context.Context, f *thresholdFixture, custID, meterID string, qty int64, ts time.Time, dims map[string]any) {
	t.Helper()
	if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
		CustomerID: custID, MeterID: meterID, Quantity: decimal.NewFromInt(qty),
		Dimensions: dims, Timestamp: &ts,
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
}

// endPeriodNow moves the subscription's period end to the current instant so
// the next billing run closes it (the test engine has no invoice grace).
func endPeriodNow(t *testing.T, ctx context.Context, f *thresholdFixture, subID string) {
	t.Helper()
	end := time.Now().UTC().Truncate(time.Microsecond)
	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		UPDATE subscriptions SET current_billing_period_end = $1, next_billing_at = $1 WHERE id = $2
	`, end, subID); err != nil {
		t.Fatalf("end period: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestUnpricedUsage_ThresholdThenCycle_CountedOnce: a spend-threshold
// invoice that leaves usage unpriced logs the cycle path's warning and
// counts the pre-fire quantity; the period close then counts only the
// post-fire quantity. The close's full-window pass (it exists after a fire,
// for deferred max/last buckets) reads the whole period again and must not
// count it a second time.
func TestUnpricedUsage_ThresholdThenCycle_CountedOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Unpriced Threshold")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 120*time.Second)
	defer cancel()
	logs := captureLogs(t)

	tok := tokensMeter(t, ctx, f, "tok_thr")
	plan, err := f.pricingSvc.CreatePlan(ctx, f.tenantID, pricing.CreatePlanInput{
		Code: "pln_unpriced_thr", Name: "Unpriced Thr", Currency: "USD",
		BillingInterval: domain.BillingMonthly, MeterIDs: []string{f.meterID, tok.ID},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	custID, subID := seedUnpricedSub(t, ctx, f, "cus_unpriced_thr", plan.ID, 500)

	// Pre-fire: 1000c of priced calls crosses the 500c cap; 70 tokens of
	// model=b match no price; 10 tokens of model=a are priced.
	for j := 0; j < 10; j++ {
		ingestUnpricedAt(t, ctx, f, custID, f.meterID, 100, f.cycleStart.Add(time.Duration(j+1)*time.Minute), nil)
	}
	ingestUnpricedAt(t, ctx, f, custID, tok.ID, 70, f.cycleStart.Add(20*time.Minute), map[string]any{"model": "b"})
	ingestUnpricedAt(t, ctx, f, custID, tok.ID, 10, f.cycleStart.Add(21*time.Minute), map[string]any{"model": "a"})

	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1", fired)
	}
	if got := unpricedCounter(t, tok.ID); got != 70 {
		t.Fatalf("after the threshold invoice: counter = %v, want 70 (the pre-fire unpriced quantity)", got)
	}
	if !strings.Contains(logs.String(), "threshold: usage matched no rating rule and meter has no default binding") ||
		!strings.Contains(logs.String(), "quantity=70") {
		t.Errorf("threshold invoice must log the unpriced quantity like the cycle path does; logs:\n%s", logs.String())
	}

	// Post-fire: 30 more unpriced tokens, then the period ends.
	time.Sleep(10 * time.Millisecond)
	ingestUnpricedAt(t, ctx, f, custID, tok.ID, 30, time.Now().UTC(), map[string]any{"model": "b"})
	time.Sleep(10 * time.Millisecond)
	endPeriodNow(t, ctx, f, subID)

	if _, failures := f.engine.RunCycleForTenant(ctx, f.tenantID, 50); len(failures) > 0 {
		t.Fatalf("cycle failures: %v", failures)
	}
	sub, err := f.subStore.Get(ctx, f.tenantID, subID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodStart.Equal(f.cycleStart) {
		t.Fatalf("the period must have closed, start is still %v", sub.CurrentBillingPeriodStart)
	}
	if got := unpricedCounter(t, tok.ID); got != 100 {
		t.Fatalf("after the period close: counter = %v, want 100 (70 on the threshold invoice + 30 after it; 200 means the full-window pass counted the period again)", got)
	}
}

// TestUnpricedUsage_CycleClose_CountsBothPaths: one invoice built with a
// priced line, an unpriced dimension (multi-rule path) and a meter whose only
// price was unlinked before the subscription existed (single-rule path).
// The preview warning carries each quantity; the close counts each one once.
func TestUnpricedUsage_CycleClose_CountsBothPaths(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Unpriced Cycle")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 120*time.Second)
	defer cancel()

	tok := tokensMeter(t, ctx, f, "tok_cyc")
	rr, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "seats_flat", Name: "Seats", Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	single, err := f.pricingSvc.CreateMeter(ctx, f.tenantID, pricing.CreateMeterInput{
		Key: "seats_cyc", Name: "Seats", Unit: "seats", Aggregation: "sum", RatingRuleVersionID: rr.ID,
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	plan, err := f.pricingSvc.CreatePlan(ctx, f.tenantID, pricing.CreatePlanInput{
		Code: "pln_unpriced_cyc", Name: "Unpriced Cyc", Currency: "USD",
		BillingInterval: domain.BillingMonthly, MeterIDs: []string{tok.ID, single.ID},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	// No live subscription yet, so unlinking the only price is allowed.
	empty := ""
	if _, err := f.pricingSvc.UpdateMeter(ctx, f.tenantID, single.ID, pricing.UpdateMeterInput{RatingRuleVersionID: &empty}); err != nil {
		t.Fatalf("unlink before any subscription: %v", err)
	}
	custID, subID := seedUnpricedSub(t, ctx, f, "cus_unpriced_cyc", plan.ID, 0)

	ingestUnpricedAt(t, ctx, f, custID, tok.ID, 10, f.cycleStart.Add(time.Minute), map[string]any{"model": "a"})
	ingestUnpricedAt(t, ctx, f, custID, tok.ID, 25, f.cycleStart.Add(2*time.Minute), map[string]any{"model": "b"})
	ingestUnpricedAt(t, ctx, f, custID, single.ID, 7, f.cycleStart.Add(3*time.Minute), nil)

	sub, err := f.subStore.Get(ctx, f.tenantID, subID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	preview, err := f.engine.Preview(ctx, sub)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	joined := strings.Join(preview.Warnings, "\n")
	if !strings.Contains(joined, `meter "tok_cyc": 25 `) || !strings.Contains(joined, `meter "seats_cyc": 7 `) {
		t.Errorf("preview warnings must carry each unpriced quantity, got:\n%s", joined)
	}

	endPeriodNow(t, ctx, f, subID)
	generated, failures := f.engine.RunCycleForTenant(ctx, f.tenantID, 50)
	if len(failures) > 0 {
		t.Fatalf("cycle failures: %v", failures)
	}
	if generated != 1 {
		t.Fatalf("generated = %d, want 1 (the model=a line)", generated)
	}
	if got := unpricedCounter(t, tok.ID); got != 25 {
		t.Errorf("multi-rule meter: counter = %v, want 25", got)
	}
	if got := unpricedCounter(t, single.ID); got != 7 {
		t.Errorf("single-rule meter: counter = %v, want 7", got)
	}
}

// TestUnpricedUsage_ImmediateCancel_NoInvoice_Counted: a cancel whose only
// usage has no price builds no invoice (nothing to bill), and the unpriced
// quantity is still counted.
func TestUnpricedUsage_ImmediateCancel_NoInvoice_Counted(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Unpriced Cancel")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 120*time.Second)
	defer cancel()

	tok := tokensMeter(t, ctx, f, "tok_cxl")
	plan, err := f.pricingSvc.CreatePlan(ctx, f.tenantID, pricing.CreatePlanInput{
		Code: "pln_unpriced_cxl", Name: "Unpriced Cxl", Currency: "USD",
		BillingInterval: domain.BillingMonthly, MeterIDs: []string{tok.ID},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	custID, subID := seedUnpricedSub(t, ctx, f, "cus_unpriced_cxl", plan.ID, 0)
	ingestUnpricedAt(t, ctx, f, custID, tok.ID, 40, f.cycleStart.Add(time.Minute), map[string]any{"model": "b"})

	canceledAt := time.Now().UTC().Truncate(time.Microsecond)
	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET status = 'canceled', canceled_at = $1 WHERE id = $2`, canceledAt, subID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	sub, err := f.subStore.Get(ctx, f.tenantID, subID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	inv, err := f.engine.BillFinalOnImmediateCancel(ctx, sub)
	if err != nil {
		t.Fatalf("bill final: %v", err)
	}
	if inv.ID != "" {
		t.Fatalf("no priced usage and no base: want no invoice, got %s", inv.ID)
	}
	if got := unpricedCounter(t, tok.ID); got != 40 {
		t.Fatalf("counter = %v, want 40", got)
	}
}

// TestUnpricedUsage_MaxMeter_ThresholdLeavesItToClose: an unpriced max
// bucket under reset_billing_cycle=false belongs to the period close, whose
// full-window pass reads the whole period (a peak cannot be split across
// two invoices). The threshold invoice must not report it, or the close
// reports the same peak again.
func TestUnpricedUsage_MaxMeter_ThresholdLeavesItToClose(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Unpriced Max")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 120*time.Second)
	defer cancel()

	rr, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "gpu_a", Name: "GPU a", Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	gpu, err := f.pricingSvc.CreateMeter(ctx, f.tenantID, pricing.CreateMeterInput{
		Key: "gpu_max", Name: "GPU", Unit: "gpus", Aggregation: "max",
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	if _, err := f.pricingSvc.UpsertMeterPricingRule(ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
		MeterID: gpu.ID, RatingRuleVersionID: rr.ID, DimensionMatch: map[string]any{"model": "a"},
		AggregationMode: domain.AggMax,
	}); err != nil {
		t.Fatalf("bind rule: %v", err)
	}
	plan, err := f.pricingSvc.CreatePlan(ctx, f.tenantID, pricing.CreatePlanInput{
		Code: "pln_unpriced_max", Name: "Unpriced Max", Currency: "USD",
		BillingInterval: domain.BillingMonthly, MeterIDs: []string{f.meterID, gpu.ID},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	custID, subID := seedUnpricedSub(t, ctx, f, "cus_unpriced_max", plan.ID, 500)
	for j := 0; j < 10; j++ {
		ingestUnpricedAt(t, ctx, f, custID, f.meterID, 100, f.cycleStart.Add(time.Duration(j+1)*time.Minute), nil)
	}
	ingestUnpricedAt(t, ctx, f, custID, gpu.ID, 50, f.cycleStart.Add(20*time.Minute), map[string]any{"model": "b"})

	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 || fired != 1 {
		t.Fatalf("fired = %d, errs = %v; want one fire", fired, errs)
	}
	if got := unpricedCounter(t, gpu.ID); got != 0 {
		t.Fatalf("after the threshold invoice: counter = %v, want 0 (the close reports a max bucket)", got)
	}

	time.Sleep(10 * time.Millisecond)
	endPeriodNow(t, ctx, f, subID)
	if _, failures := f.engine.RunCycleForTenant(ctx, f.tenantID, 50); len(failures) > 0 {
		t.Fatalf("cycle failures: %v", failures)
	}
	if got := unpricedCounter(t, gpu.ID); got != 50 {
		t.Fatalf("after the period close: counter = %v, want 50 (the peak, once)", got)
	}
}

// TestUnpricedUsage_ImmediateCancelTx_WithInvoice_Counted runs the cancel's
// production path (the caller's transaction): the final invoice carries the
// priced usage and the unpriced quantity is counted, once, after the commit.
// A first attempt whose transaction rolls back (as a refused relief does,
// and the billing run then retries it) counts nothing.
func TestUnpricedUsage_ImmediateCancelTx_WithInvoice_Counted(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Unpriced Cancel Tx")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 120*time.Second)
	defer cancel()

	tok := tokensMeter(t, ctx, f, "tok_cxltx")
	plan, err := f.pricingSvc.CreatePlan(ctx, f.tenantID, pricing.CreatePlanInput{
		Code: "pln_unpriced_cxltx", Name: "Unpriced Cxl Tx", Currency: "USD",
		BillingInterval: domain.BillingMonthly, MeterIDs: []string{tok.ID},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	custID, subID := seedUnpricedSub(t, ctx, f, "cus_unpriced_cxltx", plan.ID, 0)
	ingestUnpricedAt(t, ctx, f, custID, tok.ID, 12, f.cycleStart.Add(time.Minute), map[string]any{"model": "a"})
	ingestUnpricedAt(t, ctx, f, custID, tok.ID, 33, f.cycleStart.Add(2*time.Minute), map[string]any{"model": "b"})

	canceledAt := time.Now().UTC().Truncate(time.Microsecond)
	sub, err := f.subStore.Get(ctx, f.tenantID, subID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	sub.Status = domain.SubscriptionCanceled
	sub.CanceledAt = &canceledAt
	attempt := func() (domain.Invoice, func(), *sql.Tx) {
		tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET status = 'canceled', canceled_at = $1 WHERE id = $2`, canceledAt, subID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		inv, afterCommit, err := f.engine.BillFinalOnImmediateCancelTx(ctx, tx, sub)
		if err != nil {
			t.Fatalf("bill final (tx): %v", err)
		}
		return inv, afterCommit, tx
	}

	// First attempt: the caller's later step fails and the tx rolls back.
	_, _, tx := attempt()
	postgres.Rollback(tx)
	if got := unpricedCounter(t, tok.ID); got != 0 {
		t.Fatalf("after a rolled-back cancel: counter = %v, want 0", got)
	}

	// Retry: commits, and the caller runs afterCommit.
	inv, afterCommit, tx := attempt()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	afterCommit()
	if inv.ID == "" || inv.SubtotalCents != 12 {
		t.Fatalf("final invoice = %q subtotal %d, want one invoice of 12c (model=a only)", inv.ID, inv.SubtotalCents)
	}
	if got := unpricedCounter(t, tok.ID); got != 33 {
		t.Fatalf("counter = %v, want 33", got)
	}
}
