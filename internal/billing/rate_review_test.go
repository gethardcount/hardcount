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
		if err == nil || !strings.Contains(err.Error(), "for meter mtr_api price prc_api:") {
			t.Fatalf("run %d: close err = %v, want the first meter by id (mtr_api) named", i, err)
		}
		_, _, err = engine.unbilledUsageCents(context.Background(), sub)
		if err == nil || !strings.Contains(err.Error(), "rate meter mtr_api ") {
			t.Fatalf("run %d: live balance err = %v, want the first meter by id (mtr_api) named", i, err)
		}
	}
}

// fireAt10 is a threshold invoice for setupEngine's period that billed
// through March 10: 700 API calls and 50 GB.
func fireAt10() thresholdWatermark {
	start, t := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	line := func(meterID, priceID string, qty, amt int64) domain.InvoiceLineItem {
		return domain.InvoiceLineItem{LineType: domain.LineTypeUsage, MeterID: meterID, PriceID: priceID,
			Quantity: qty, QuantityDecimal: decimal.NewFromInt(qty), AmountCents: amt,
			BillingPeriodStart: &start, BillingPeriodEnd: &t}
	}
	return thresholdWatermark{billedThrough: &t, lines: []domain.InvoiceLineItem{
		line("mtr_api", "prc_api", 700, 7000),
		line("mtr_storage", "prc_storage", 50, 2500),
	}}
}

// Each price's tiers continue from what the threshold invoice billed for that
// price, not for another one. setupEngine's two meters each have one
// catch-all price; storage is made graduated here.
func TestCancelAfterFire_EachMeterContinuesItsOwnTiers(t *testing.T) {
	engine, subs, usage, pricing, _ := setupEngine()
	pricing.prices["pln_1"][1] = priceOf("prc_storage", "mtr_storage", domain.PriceShape{Mode: domain.PricingGraduated, Tiers: []domain.RatingTier{
		{UpTo: new(int64(100)), UnitAmountCents: decimal.NewFromInt(50)},
		{UpTo: nil, UnitAmountCents: decimal.NewFromInt(20)},
	}})
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

// After a threshold invoice, a closer reads a meter priced by one catch-all
// price over the whole period only when the invoice deferred it (a max or last
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

// A preview lists its usage lines by meter id, then plan id, the order the
// closers walk their windows in, so the order is the same on every call.
// With a plan the subscription left this period carrying another meter, the
// order once followed map iteration.
func TestPreview_UsageLinesInMeterIDOrder(t *testing.T) {
	engine, subs, usage, pricing, _ := setupEngine()
	for planID, meterID := range map[string]string{"pln_a": "mtr_a", "pln_b": "mtr_b"} {
		pricing.plans[planID] = domain.Plan{ID: planID, Name: planID, Currency: "USD", BillingInterval: domain.BillingMonthly}
		pricing.meters[meterID] = domain.Meter{ID: meterID, Name: meterID, Unit: "calls"}
		pricing.prices[planID] = []domain.PlanPrice{perUnit("prc_"+meterID, meterID, 2500)}
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
	ctx := context.Background()
	usageMeters := func(res PreviewResult) string {
		var ids []string
		for _, l := range res.Lines {
			if l.LineType == "usage" {
				ids = append(ids, l.MeterID)
			}
		}
		return strings.Join(ids, ",")
	}
	for i := 0; i < 20; i++ {
		closeMirror, err := engine.previewCycle(ctx, sub)
		if err != nil {
			t.Fatalf("close-mirror preview: %v", err)
		}
		if got := usageMeters(closeMirror); got != "mtr_a,mtr_api,mtr_b,mtr_storage" {
			t.Fatalf("call %d: close-mirror usage lines = %s, want mtr_a,mtr_api,mtr_b,mtr_storage", i, got)
		}
		flat, err := engine.previewWithWindow(ctx, sub, *sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd)
		if err != nil {
			t.Fatalf("flat preview: %v", err)
		}
		if got := usageMeters(flat); got != "mtr_a,mtr_api,mtr_b,mtr_storage" {
			t.Fatalf("call %d: flat-window usage lines = %s, want mtr_a,mtr_api,mtr_b,mtr_storage", i, got)
		}
	}
}
