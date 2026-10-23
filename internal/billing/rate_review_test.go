package billing

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Review round 1 of the one rater (ADR-130 PR-1). These tests guard what the
// differential alone guarded, or did not guard at all, so they still hold
// once PR-4 deletes the old loops.

// A closer walks its meters in a fixed order, so a failure names the same
// meter on every run (and on every retry of the same subscription). It used
// to walk them in map order: a close failing on every meter named a random
// one.
func TestCloser_FailureNamesTheSameMeterEveryRun(t *testing.T) {
	engine, subs, _, pricing, _ := setupEngine()
	pricing.overrideErr = fmt.Errorf("connection reset by peer")
	sub := subs.subs["sub_1"]
	ps, pe := *sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd
	for i := 0; i < 16; i++ {
		_, _, _, err := engine.buildLineItems(context.Background(), sub, pe, ps, pe, clonePlans(pricing.plans), "USD", time.UTC, unpricedUsage{}, netNegativeUsage{})
		if err == nil || !strings.Contains(err.Error(), "for meter mtr_api:") {
			t.Fatalf("run %d: close err = %v, want the first meter by id (mtr_api) named", i, err)
		}
		_, _, err = engine.unbilledUsageCents(context.Background(), sub)
		if err == nil || !strings.Contains(err.Error(), "rate meter mtr_api ") {
			t.Fatalf("run %d: live balance err = %v, want the first meter by id (mtr_api) named", i, err)
		}
	}
}

// A call whose new result moves between two runs fails the differential,
// unless its test is listed as one whose fake changes between runs on
// purpose. Filing it as unstable and passing hid an order-dependent rating.
func TestRateDifferential_UnstableCallFails(t *testing.T) {
	d := &rateDifferential{feats: map[string]map[string]*featTally{}, calls: map[string]int{}}
	d.compare("period close", domain.Subscription{ID: "sub_1"}, "a", "b", func() string { return "c" })
	if len(d.diffs) != 1 {
		t.Fatalf("diffs = %d (unstable %v), want the unstable call counted as a difference", len(d.diffs), d.unstable)
	}
}

// fireAt10 is a threshold invoice for setupEngine's period that billed
// through March 10: 700 API calls and 50 GB.
func fireAt10() thresholdWatermark {
	t := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	return thresholdWatermark{billedThrough: &t, lines: []domain.InvoiceLineItem{
		usageLine("mtr_api", "rrv_api", 700, 7000),
		usageLine("mtr_storage", "rrv_storage", 50, 2500),
	}}
}

// Each meter's tiers continue from what the threshold invoice billed on that
// meter, not on another one. setupEngine's two meters are both single-rule
// (no pricing rules); storage is made graduated here.
func TestCancelAfterFire_EachMeterContinuesItsOwnTiers(t *testing.T) {
	engine, subs, usage, pricing, _ := setupEngine()
	pricing.rules["rrv_storage"] = domain.RatingRuleVersion{
		ID: "rrv_storage", RuleKey: "storage_gb", Version: 1, Mode: domain.PricingGraduated,
		GraduatedTiers: []domain.RatingTier{
			{UpTo: 100, UnitAmountCents: decimal.NewFromInt(50)},
			{UpTo: 0, UnitAmountCents: decimal.NewFromInt(20)},
		},
	}
	sub := subs.subs["sub_1"]
	ps := *sub.CurrentBillingPeriodStart
	wm := fireAt10()
	canceledAt := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	usage.perInterval = map[string]int64{
		mockIntervalKey("mtr_api", *wm.billedThrough, canceledAt):     600,
		mockIntervalKey("mtr_storage", *wm.billedThrough, canceledAt): 100,
	}
	lines, _, err := engine.buildCancelLineItems(context.Background(), sub, wm, clonePlans(pricing.plans), "USD", ps, canceledAt, time.UTC, unpricedUsage{}, netNegativeUsage{}, cancelUsageSuffix)
	if err != nil {
		t.Fatalf("cancel lines: %v", err)
	}
	got := map[string]int64{}
	for _, l := range lines {
		if l.LineType == domain.LineTypeUsage {
			got[l.MeterID] += l.AmountCents
		}
	}
	// API: 700 billed, 600 more = 300 at 10¢ + 300 at 5¢. Storage: 50
	// billed, 100 more = 50 at 50¢ + 50 at 20¢.
	if got["mtr_api"] != 4500 || got["mtr_storage"] != 3500 {
		t.Errorf("usage amounts = %v, want mtr_api 4500 and mtr_storage 3500 (each meter continuing its own tiers)", got)
	}
}

// readLog is a usage fake that records the windows of every whole-invoice
// read (AggregateForBillingPeriodByAgg).
type readLog struct {
	*mockUsage
	mu   sync.Mutex
	from []time.Time
}

func (r *readLog) AggregateForBillingPeriodByAgg(ctx context.Context, tenantID, customerID string, meters map[string]string, from, to time.Time) (map[string]decimal.Decimal, error) {
	r.mu.Lock()
	r.from = append(r.from, from)
	r.mu.Unlock()
	return r.mockUsage.AggregateForBillingPeriodByAgg(ctx, tenantID, customerID, meters, from, to)
}

// After a threshold invoice, a closer reads a meter without pricing rules
// over the whole period only when the invoice deferred it (a max or last
// meter). An additive meter's full-window pass prices nothing, so it reads
// nothing: that read scans the period's pre-fire usage, and a failure on it
// failed a close that had nothing to bill from it.
func TestCloserAfterFire_AdditiveMeterSkipsTheFullWindowRead(t *testing.T) {
	engine, subs, usage, pricing, _ := setupEngine()
	reads := &readLog{mockUsage: usage}
	engine.usage = reads
	sub := subs.subs["sub_1"]
	ps := *sub.CurrentBillingPeriodStart
	canceledAt := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	if _, _, err := engine.buildCancelLineItems(context.Background(), sub, fireAt10(), clonePlans(pricing.plans), "USD", ps, canceledAt, time.UTC, unpricedUsage{}, netNegativeUsage{}, cancelUsageSuffix); err != nil {
		t.Fatalf("cancel lines: %v", err)
	}
	for _, from := range reads.from {
		if from.Equal(ps) {
			t.Fatalf("reads started at %v; want none from the period start (no meter is deferred)", reads.from)
		}
	}
}

// Review round 2 of the one rater (ADR-130 PR-1).

// seedFireAt10 stores fireAt10 as a threshold invoice for sub_1's March
// period, so every reader of the watermark finds it.
func seedFireAt10(invoices *mockInvoices) {
	wm := fireAt10()
	invoices.invoices = append(invoices.invoices, domain.Invoice{
		ID: "inv_thr", TenantID: "t1", CustomerID: "cus_1", SubscriptionID: "sub_1",
		BillingReason: domain.BillingReasonThreshold, Status: domain.InvoiceFinalized,
		BillingPeriodStart: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), BillingPeriodEnd: *wm.billedThrough,
	})
	for _, li := range wm.lines {
		li.InvoiceID = "inv_thr"
		invoices.lineItems = append(invoices.lineItems, li)
	}
}

// A pricing rule bound to the same graduated version as its meter's own
// binding makes the threshold invoice's line ambiguous: its quantity could
// seed either bucket's tiers. Only the closers refuse (checkLadderSeeds):
// they would write the guess on an invoice. The close-mirror preview and the
// live balance are estimates and still answer.
func TestLadderSeedCheck_OnlyTheClosersRefuse(t *testing.T) {
	engine, subs, _, pricing, invoices := setupEngine()
	pricing.meterPricingRules = map[string][]domain.MeterPricingRule{
		"mtr_api": {{ID: "mpr_gpt", MeterID: "mtr_api", RatingRuleVersionID: "rrv_api",
			DimensionMatch: map[string]any{"model": "gpt"}, AggregationMode: domain.AggSum}},
	}
	seedFireAt10(invoices)
	ctx := context.Background()
	sub := subs.subs["sub_1"]
	ps := *sub.CurrentBillingPeriodStart
	canceledAt := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	if _, _, err := engine.buildCancelLineItems(ctx, sub, fireAt10(), clonePlans(pricing.plans), "USD", ps, canceledAt, time.UTC, unpricedUsage{}, netNegativeUsage{}, cancelUsageSuffix); err == nil || !strings.Contains(err.Error(), "resolve to rating rule rrv_api") {
		t.Fatalf("cancel err = %v, want the ambiguous-seed refusal", err)
	}
	if _, err := engine.previewCycle(ctx, sub); err != nil {
		t.Errorf("close-mirror preview: %v, want an estimate", err)
	}
	if _, _, err := engine.unbilledUsageCents(ctx, sub); err != nil {
		t.Errorf("live balance: %v, want a total", err)
	}
}

// A preview lists usage lines in the plan's meter order, not in meter id
// order. setupEngine's plan lists its meters in id order, so it is reversed
// here.
func TestPreview_UsageLinesFollowThePlansMeterOrder(t *testing.T) {
	engine, subs, _, pricing, _ := setupEngine()
	plan := pricing.plans["pln_1"]
	plan.MeterIDs = []string{"mtr_storage", "mtr_api"}
	pricing.plans["pln_1"] = plan
	sub := subs.subs["sub_1"]
	ctx := context.Background()
	usageMeters := func(res PreviewResult) []string {
		var ids []string
		for _, l := range res.Lines {
			if l.LineType == "usage" {
				ids = append(ids, l.MeterID)
			}
		}
		return ids
	}
	closeMirror, err := engine.previewCycle(ctx, sub)
	if err != nil {
		t.Fatalf("close-mirror preview: %v", err)
	}
	flat, err := engine.previewWithWindow(ctx, sub, *sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd)
	if err != nil {
		t.Fatalf("flat preview: %v", err)
	}
	for name, res := range map[string]PreviewResult{"close mirror": closeMirror, "flat window": flat} {
		if got := strings.Join(usageMeters(res), ","); got != "mtr_storage,mtr_api" {
			t.Errorf("%s usage lines = %s, want mtr_storage,mtr_api (the plan's order)", name, got)
		}
	}
}

// The close-mirror preview lists the meters only a segment plan carries (a
// plan the subscription left this period) after the current plans' meters,
// by plan id. It walked those plans in map order, so with two such plans
// the line order changed from call to call.
func TestPreview_SegmentPlanMetersInPlanIDOrder(t *testing.T) {
	engine, subs, usage, pricing, _ := setupEngine()
	for planID, meterID := range map[string]string{"pln_a": "mtr_a", "pln_b": "mtr_b"} {
		pricing.plans[planID] = domain.Plan{ID: planID, Name: planID, Currency: "USD", BillingInterval: domain.BillingMonthly, MeterIDs: []string{meterID}}
		pricing.meters[meterID] = domain.Meter{ID: meterID, Name: meterID, Unit: "calls", RatingRuleVersionID: "rrv_storage"}
		usage.totals[meterID] = 10
	}
	day := func(d int) time.Time { return time.Date(2026, 3, d, 0, 0, 0, 0, time.UTC) }
	mar5, mar10 := day(5), day(10)
	subs.intervals = []domain.ItemInterval{
		{PlanID: "pln_b", Quantity: 1, StartsAt: day(1), EndsAt: &mar5},
		{PlanID: "pln_a", Quantity: 1, StartsAt: mar5, EndsAt: &mar10},
		{PlanID: "pln_1", Quantity: 1, StartsAt: mar10},
	}
	sub := subs.subs["sub_1"]
	for i := 0; i < 20; i++ {
		res, err := engine.previewCycle(context.Background(), sub)
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		var ids []string
		for _, l := range res.Lines {
			if l.LineType == "usage" {
				ids = append(ids, l.MeterID)
			}
		}
		if got := strings.Join(ids, ","); got != "mtr_api,mtr_storage,mtr_a,mtr_b" {
			t.Fatalf("call %d: usage lines = %s, want mtr_api,mtr_storage,mtr_a,mtr_b", i, got)
		}
	}
}
