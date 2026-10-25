package billing_test

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// Amounts this large (near 9.2 × 10^18 cents, about $92 quadrillion) are not
// realistic. They are tested because every invoice amount is int64 cents:
// an amount past that must be an error, never a wrapped number. On an
// invoice path the invoice is refused (nothing is written, and the close is
// retried); on a read path (preview, live balance, the spend-threshold
// check) the read fails. Inside int64 every amount is what it was.

// prorated is fee × num / den rounded half to even, worked exactly.
func prorated(fee int64, num, den int) int64 {
	p := new(big.Int).Mul(big.NewInt(fee), big.NewInt(int64(num)))
	q, r := new(big.Int).QuoRem(p, big.NewInt(int64(den)), new(big.Int))
	twice := new(big.Int).Mul(r, big.NewInt(2))
	if c := twice.Cmp(big.NewInt(int64(den))); c > 0 || (c == 0 && q.Bit(0) == 1) {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		panic(fmt.Sprintf("prorated(%d, %d, %d) does not fit int64", fee, num, den))
	}
	return q.Int64()
}

// setBase gives the fixture plan an in_arrears base fee and the item (and its
// billing interval) a quantity.
func setBase(t *testing.T, lf *liveFixture, base, qty int64) {
	t.Helper()
	lf.exec(t, `UPDATE plans SET base_amount_cents = $1 WHERE id = $2`, base, lf.planID)
	lf.exec(t, `UPDATE subscription_items SET quantity = $1 WHERE id = $2`, qty, lf.itemID)
	lf.exec(t, `UPDATE billing_intervals SET quantity = $1 WHERE subscription_item_id = $2`, qty, lf.itemID)
}

// closeNow ends the period just ahead of now and runs the real close.
func closeNow(t *testing.T, lf *liveFixture) (int, []billing.SubBillError) {
	t.Helper()
	newEnd := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	lf.exec(t, `UPDATE subscriptions SET current_billing_period_end = $1, next_billing_at = $1 WHERE tenant_id = $2`, newEnd, lf.tenantID)
	time.Sleep(time.Until(newEnd.Add(500 * time.Millisecond)))
	return lf.engine.RunCycleForTenant(lf.ctx, lf.tenantID, 50)
}

// baseLine is the one base-fee line of the invoice, and the days it was
// prorated over, read from its description.
func baseLine(t *testing.T, lf *liveFixture, inv domain.Invoice) (domain.InvoiceLineItem, int, int) {
	t.Helper()
	lines, err := lf.invStore.ListLineItems(lf.ctx, lf.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("list lines: %v", err)
	}
	for _, l := range lines {
		if l.LineType != domain.LineTypeBaseFee {
			continue
		}
		var qty int64
		var num, den int
		if _, err := fmt.Sscanf(l.Description, "Threshold Plan - base fee (qty %d, prorated %d/%d days)", &qty, &num, &den); err != nil {
			t.Fatalf("base line %q is not prorated: %v", l.Description, err)
		}
		return l, num, den
	}
	t.Fatalf("invoice %s has no base-fee line", inv.ID)
	return domain.InvoiceLineItem{}, 0, 0
}

var baseFeeCases = []struct {
	name      string
	base, qty int64
	refused   bool // base × quantity does not fit int64 cents
}{
	// base × quantity fits; × the days did not, and wrapped.
	{name: "4e18 full fee, prorated", base: 1_000_000_000_000_000_000, qty: 4},
	{name: "MaxInt64 full fee, prorated", base: math.MaxInt64, qty: 1},
	// base × quantity is 2^63, one past MaxInt64.
	{name: "full fee one past int64", base: 1 << 62, qty: 2, refused: true},
	{name: "full fee 1e19", base: 5_000_000_000_000_000_000, qty: 2, refused: true},
}

// The period close prorates the in_arrears base (here 3 of a month's days,
// the fixture's period so far). The proration is exact for every full fee
// that fits int64 cents; a full fee that does not fit refuses the close.
func TestCheckedMath_CloseBaseFee(t *testing.T) {
	for _, tc := range baseFeeCases {
		t.Run(tc.name, func(t *testing.T) {
			lf := newLiveFixture(t, "Checked Close "+tc.name, 0)
			setBase(t, lf, tc.base, tc.qty)
			generated, failures := closeNow(t, lf)
			invs := lf.listInvoices(t, lf.ctx)
			if tc.refused {
				if len(failures) != 1 || !errors.Is(failures[0].Err, domain.ErrAmountOverflow) {
					t.Fatalf("close failures = %v, want one ErrAmountOverflow", failures)
				}
				if generated != 0 || len(invs) != 0 {
					t.Fatalf("close made %d invoices (%d listed), want none", generated, len(invs))
				}
				return
			}
			if len(failures) > 0 || generated != 1 || len(invs) != 1 {
				t.Fatalf("close: generated=%d failures=%v invoices=%d, want 1 invoice", generated, failures, len(invs))
			}
			line, num, den := baseLine(t, lf, invs[0])
			want := prorated(tc.base*tc.qty, num, den)
			if line.AmountCents != want || invs[0].SubtotalCents != want || invs[0].TotalAmountCents != want {
				t.Errorf("base line %d, subtotal %d, total %d; want %d (%d × %d/%d)",
					line.AmountCents, invs[0].SubtotalCents, invs[0].TotalAmountCents, want, tc.base*tc.qty, num, den)
			}
		})
	}
}

// The immediate cancel bills the in_arrears base prorated to the cancel,
// with the same math.
func TestCheckedMath_CancelBaseFee(t *testing.T) {
	for _, tc := range baseFeeCases {
		t.Run(tc.name, func(t *testing.T) {
			lf := newLiveFixture(t, "Checked Cancel "+tc.name, 0)
			setBase(t, lf, tc.base, tc.qty)
			sub, err := lf.subStore.Get(lf.ctx, lf.tenantID, lf.subID)
			if err != nil {
				t.Fatalf("get sub: %v", err)
			}
			now := time.Now().UTC()
			sub.Status, sub.CanceledAt = domain.SubscriptionCanceled, &now
			inv, err := lf.engine.BillFinalOnImmediateCancel(lf.ctx, sub)
			invs := lf.listInvoices(t, lf.ctx)
			if tc.refused {
				if !errors.Is(err, domain.ErrAmountOverflow) {
					t.Fatalf("cancel err = %v, want ErrAmountOverflow", err)
				}
				if len(invs) != 0 {
					t.Fatalf("cancel wrote %d invoices, want none", len(invs))
				}
				return
			}
			if err != nil || inv.ID == "" || len(invs) != 1 {
				t.Fatalf("cancel: err=%v invoice=%q invoices=%d, want one invoice", err, inv.ID, len(invs))
			}
			line, num, den := baseLine(t, lf, invs[0])
			if want := prorated(tc.base*tc.qty, num, den); line.AmountCents != want || invs[0].TotalAmountCents != want {
				t.Errorf("base line %d, total %d; want %d (%d × %d/%d)", line.AmountCents, invs[0].TotalAmountCents, want, tc.base*tc.qty, num, den)
			}
		})
	}
}

// The operator preview of the period (the close's own base-fee math) shows
// the same amount, or fails where the close would be refused. The
// explicit-window estimate bills the full fee: one that does not fit is an
// error there too.
func TestCheckedMath_PreviewBaseFee(t *testing.T) {
	for _, tc := range baseFeeCases {
		t.Run(tc.name, func(t *testing.T) {
			lf := newLiveFixture(t, "Checked Preview "+tc.name, 0)
			setBase(t, lf, tc.base, tc.qty)
			svc := billing.NewPreviewService(lf.engine, customer.NewPostgresStore(lf.db), lf.subStore)
			got, err := svc.CreatePreview(lf.ctx, lf.tenantID, billing.CreatePreviewRequest{CustomerID: lf.customerID, SubscriptionID: lf.subID})
			if tc.refused {
				if !errors.Is(err, domain.ErrAmountOverflow) {
					t.Fatalf("preview err = %v, want ErrAmountOverflow", err)
				}
			} else {
				if err != nil {
					t.Fatalf("preview: %v", err)
				}
				var base *billing.PreviewLine
				for i := range got.Lines {
					if got.Lines[i].LineType == "base_fee" {
						base = &got.Lines[i]
					}
				}
				if base == nil {
					t.Fatalf("preview has no base line: %+v", got.Lines)
				}
				var qty int64
				var num, den int
				if _, err := fmt.Sscanf(base.Description, "Threshold Plan - base fee (qty %d, prorated %d/%d days)", &qty, &num, &den); err != nil {
					t.Fatalf("preview base line %q is not prorated: %v", base.Description, err)
				}
				if want := prorated(tc.base*tc.qty, num, den); base.AmountCents != want || len(got.Totals) != 1 || got.Totals[0].AmountCents != want {
					t.Errorf("preview base %d, totals %+v; want %d", base.AmountCents, got.Totals, want)
				}
			}

			// The explicit window bills the full fee, base × quantity.
			start := lf.cycleStart
			end := start.Add(24 * time.Hour)
			win, err := svc.CreatePreview(lf.ctx, lf.tenantID, billing.CreatePreviewRequest{
				CustomerID: lf.customerID, SubscriptionID: lf.subID, Period: billing.CreatePreviewPeriod{From: start, To: end},
			})
			if tc.refused {
				if !errors.Is(err, domain.ErrAmountOverflow) {
					t.Errorf("window preview err = %v, want ErrAmountOverflow", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("window preview: %v", err)
			}
			if len(win.Totals) != 1 || win.Totals[0].AmountCents != tc.base*tc.qty {
				t.Errorf("window preview totals %+v, want %d", win.Totals, tc.base*tc.qty)
			}
		})
	}
}

// twoHugeBuckets gives the fixture meter two pricing buckets at
// 999999999999999999 cents a unit (the largest rate a new rule may have)
// and 5 units in each: each bucket's amount, 4999999999999999995 cents, fits
// int64; the two together do not.
func twoHugeBuckets(t *testing.T, lf *liveFixture) {
	t.Helper()
	for _, m := range []string{"a", "b"} {
		v := createRule(t, lf, flatRule("model_"+m+"_max", "999999999999999999"))
		seedPricingRule(t, lf.db, lf.ctx, lf.tenantID, pricing.UpsertMeterPricingRuleInput{
			MeterID: lf.meterID, RatingRuleVersionID: v.ID, DimensionMatch: map[string]any{"model": m}, Priority: 10,
		})
		insertUsageAt(t, lf, lf.cycleStart.Add(time.Minute), "5", map[string]any{"model": m})
	}
}

// Each usage line fits int64 cents but their sum does not: the preview, the
// live credit balance and the cost dashboard fail rather than show a wrapped
// total, and the period close is refused.
func TestCheckedMath_UsageSumPastInt64(t *testing.T) {
	lf := newLiveFixture(t, "Checked Usage Sum", 0)
	twoHugeBuckets(t, lf)

	svc := billing.NewPreviewService(lf.engine, customer.NewPostgresStore(lf.db), lf.subStore)
	if got, err := svc.CreatePreview(lf.ctx, lf.tenantID, billing.CreatePreviewRequest{CustomerID: lf.customerID, SubscriptionID: lf.subID}); !errors.Is(err, domain.ErrAmountOverflow) {
		t.Errorf("preview err = %v (totals %+v), want ErrAmountOverflow", err, got.Totals)
	}
	end := time.Now().UTC()
	if got, err := svc.CreatePreview(lf.ctx, lf.tenantID, billing.CreatePreviewRequest{
		CustomerID: lf.customerID, SubscriptionID: lf.subID, Period: billing.CreatePreviewPeriod{From: lf.cycleStart, To: end},
	}); !errors.Is(err, domain.ErrAmountOverflow) {
		t.Errorf("window preview err = %v (totals %+v), want ErrAmountOverflow", err, got.Totals)
	}
	if got, err := lf.engine.LiveCreditBalance(lf.ctx, lf.tenantID, lf.customerID); !errors.Is(err, domain.ErrAmountOverflow) {
		t.Errorf("live balance err = %v (%+v), want ErrAmountOverflow", err, got.Balances)
	}
	if _, err := lf.dashboard.GetByToken(lf.ctx, lf.token); !errors.Is(err, domain.ErrAmountOverflow) {
		t.Errorf("cost dashboard err = %v, want ErrAmountOverflow", err)
	}

	generated, failures := closeNow(t, lf)
	if len(failures) != 1 || !errors.Is(failures[0].Err, domain.ErrAmountOverflow) {
		t.Errorf("close failures = %v, want one ErrAmountOverflow", failures)
	}
	if invs := lf.listInvoices(t, lf.ctx); generated != 0 || len(invs) != 0 {
		t.Errorf("close made %d invoices (%d listed), want none", generated, len(invs))
	}
}

// The spend-threshold check adds the usage lines to compare them with the
// cap. A sum past int64 is an error for the subscription (the scan retries
// it next tick), never a wrapped figure below the cap that skips the fire.
func TestCheckedMath_ThresholdRunningTotalPastInt64(t *testing.T) {
	lf := newLiveFixture(t, "Checked Threshold Sum", 0)
	twoHugeBuckets(t, lf)
	if _, err := lf.subSvc.SetBillingThresholds(lf.ctx, lf.tenantID, lf.subID, subscription.BillingThresholdsInput{AmountGTE: 1}); err != nil {
		t.Fatalf("set thresholds: %v", err)
	}
	fired, errs := lf.engine.ScanThresholds(lf.ctx, 50)
	if fired != 0 || len(errs) != 1 || !errors.Is(errs[0], domain.ErrAmountOverflow) {
		t.Errorf("threshold scan: fired=%d errs=%v, want no fire and one ErrAmountOverflow", fired, errs)
	}
	if invs := lf.listInvoices(t, lf.ctx); len(invs) != 0 {
		t.Errorf("%d invoices, want none", len(invs))
	}
}

// The day-1 in_advance invoice adds its base lines before it checks for a
// zero subtotal. Here 35 in_advance items each bill a base that fits int64
// cents, and their sum does not: the invoice is refused, not skipped as if
// there were nothing to bill.
func TestCheckedMath_OnCreateSubtotalPastInt64(t *testing.T) {
	lf := newLiveFixture(t, "Checked On Create", 0)
	const base = 290_000_000_000_000_000 // × 31 days fits int64
	lf.exec(t, `UPDATE plans SET base_bill_timing = 'in_advance', base_amount_cents = $1, meter_ids = '[]'::jsonb WHERE id = $2`, base, lf.planID)
	for i := range 34 {
		p, err := lf.pricingSvc.CreatePlan(lf.ctx, lf.tenantID, pricing.CreatePlanInput{
			Code: fmt.Sprintf("pln_big_%d", i), Name: fmt.Sprintf("Big %d", i), Currency: "USD",
			BillingInterval: domain.BillingMonthly, BaseAmountCents: base, BaseBillTiming: domain.BillInAdvance,
		})
		if err != nil {
			t.Fatalf("create plan %d: %v", i, err)
		}
		itemID := postgres.NewID("vlx_si")
		lf.exec(t, `INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at, livemode)
			VALUES ($1, $2, $3, $4, 1, '{}'::jsonb, now(), now(), false)`, itemID, lf.tenantID, lf.subID, p.ID)
	}
	sub, err := lf.subStore.Get(lf.ctx, lf.tenantID, lf.subID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	if len(sub.Items) != 35 {
		t.Fatalf("subscription has %d items, want 35", len(sub.Items))
	}
	inv, err := lf.engine.BillOnCreate(lf.ctx, sub)
	if !errors.Is(err, domain.ErrAmountOverflow) {
		t.Errorf("BillOnCreate err = %v (invoice %q, total %d), want ErrAmountOverflow", err, inv.ID, inv.TotalAmountCents)
	}
	if invs := lf.listInvoices(t, lf.ctx); len(invs) != 0 {
		t.Errorf("%d invoices, want none", len(invs))
	}
}

// The immediate cancel adds its usage lines and skips the final invoice when
// the subtotal is 0 or less. A sum past int64 is refused instead: a wrapped,
// negative subtotal would have skipped the bill with no error.
func TestCheckedMath_CancelUsageSumPastInt64(t *testing.T) {
	lf := newLiveFixture(t, "Checked Cancel Usage Sum", 0)
	twoHugeBuckets(t, lf)
	sub, err := lf.subStore.Get(lf.ctx, lf.tenantID, lf.subID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	now := time.Now().UTC()
	sub.Status, sub.CanceledAt = domain.SubscriptionCanceled, &now
	inv, err := lf.engine.BillFinalOnImmediateCancel(lf.ctx, sub)
	if !errors.Is(err, domain.ErrAmountOverflow) {
		t.Errorf("cancel err = %v (invoice %q), want ErrAmountOverflow", err, inv.ID)
	}
	if invs := lf.listInvoices(t, lf.ctx); len(invs) != 0 {
		t.Errorf("%d invoices, want none", len(invs))
	}
}

// A threshold counts and bills usage only, so its fire does not depend on
// the base fee: not on base × quantity fitting int64, and not on base plus
// usage fitting. Each case fires a usage-only invoice that fits.
func TestCheckedMath_ThresholdFiresWhenBasePlusUsagePastInt64(t *testing.T) {
	const hugeUsage = 4_999_999_999_999_999_995 // 5 units at 999999999999999999
	cases := []struct {
		name      string
		base, qty int64
		huge      bool // one bucket of hugeUsage cents, else 100 cents
		item      bool // an item (usage_gte) threshold, else amount_gte
		want      int64
	}{
		// 5e18 base and 5e18 usage each fit; their sum does not.
		{name: "base plus usage", base: 5_000_000_000_000_000_000, qty: 1, huge: true, want: hugeUsage},
		// 5e18 × 2 seats does not fit; 100 cents of usage does.
		{name: "base times quantity", base: 5_000_000_000_000_000_000, qty: 2, want: 100},
		{name: "base times quantity, item threshold", base: 5_000_000_000_000_000_000, qty: 2, item: true, want: 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lf := newLiveFixture(t, "Checked Threshold "+tc.name, 0)
			setBase(t, lf, tc.base, tc.qty)
			if tc.huge {
				v := createRule(t, lf, flatRule("model_huge", "999999999999999999"))
				seedPricingRule(t, lf.db, lf.ctx, lf.tenantID, pricing.UpsertMeterPricingRuleInput{
					MeterID: lf.meterID, RatingRuleVersionID: v.ID, DimensionMatch: map[string]any{"model": "huge"}, Priority: 10,
				})
				insertUsageAt(t, lf, lf.cycleStart.Add(time.Minute), "5", map[string]any{"model": "huge"})
			} else {
				insertUsageAt(t, lf, lf.cycleStart.Add(time.Minute), "100", nil)
			}
			in := subscription.BillingThresholdsInput{AmountGTE: 1}
			if tc.item {
				in = subscription.BillingThresholdsInput{ItemThresholds: []subscription.ItemThresholdInput{{SubscriptionItemID: lf.itemID, UsageGTE: "1"}}}
			}
			if _, err := lf.subSvc.SetBillingThresholds(lf.ctx, lf.tenantID, lf.subID, in); err != nil {
				t.Fatalf("set thresholds: %v", err)
			}
			fired, errs := lf.engine.ScanThresholds(lf.ctx, 50)
			if fired != 1 || len(errs) != 0 {
				t.Fatalf("threshold scan: fired=%d errs=%v, want one fire and no error", fired, errs)
			}
			invs := lf.listInvoices(t, lf.ctx)
			if len(invs) != 1 {
				t.Fatalf("%d invoices, want the threshold invoice", len(invs))
			}
			inv := invs[0]
			if inv.BillingReason != domain.BillingReasonThreshold || inv.Status != domain.InvoiceFinalized ||
				inv.SubtotalCents != tc.want || inv.TotalAmountCents != tc.want {
				t.Errorf("invoice reason/status/subtotal/total = %s/%s/%d/%d, want threshold/finalized/%d/%d",
					inv.BillingReason, inv.Status, inv.SubtotalCents, inv.TotalAmountCents, tc.want, tc.want)
			}
		})
	}
}

// A max meter's bucket counts toward amount_gte but is left off the fire
// invoice (ADR-066 §4). Here the max bucket and a sum bucket each fit int64
// cents, and so does the invoice (the sum bucket alone), but the running
// total does not: an error for the subscription, never a wrapped running
// total below the cap that skips the fire.
func TestCheckedMath_ThresholdCapRunningPastInt64(t *testing.T) {
	lf := newLiveFixture(t, "Checked Threshold Cap Running", 0)
	v := createRule(t, lf, flatRule("model_huge", "999999999999999999"))
	seedPricingRule(t, lf.db, lf.ctx, lf.tenantID, pricing.UpsertMeterPricingRuleInput{
		MeterID: lf.meterID, RatingRuleVersionID: v.ID, DimensionMatch: map[string]any{"model": "huge"}, Priority: 10,
	})
	insertUsageAt(t, lf, lf.cycleStart.Add(time.Minute), "5", map[string]any{"model": "huge"})

	maxRule := createRule(t, lf, flatRule("gpu_peak", "999999999999999999"))
	maxMeter, err := lf.pricingSvc.CreateMeter(lf.ctx, lf.tenantID, pricing.CreateMeterInput{
		EventName: "gpu", Name: "GPU Concurrency", Unit: "gpus", Aggregation: "max", RatingRuleVersionID: maxRule.ID,
	})
	if err != nil {
		t.Fatalf("create max meter: %v", err)
	}
	lf.exec(t, `UPDATE plans SET meter_ids = meter_ids || to_jsonb($1::text) WHERE id = $2`, maxMeter.ID, lf.planID)
	lf.insertUsage(t, maxMeter.ID, 5, lf.cycleStart.Add(time.Minute))

	if _, err := lf.subSvc.SetBillingThresholds(lf.ctx, lf.tenantID, lf.subID, subscription.BillingThresholdsInput{AmountGTE: 1}); err != nil {
		t.Fatalf("set thresholds: %v", err)
	}
	fired, errs := lf.engine.ScanThresholds(lf.ctx, 50)
	if fired != 0 || len(errs) != 1 || !errors.Is(errs[0], domain.ErrAmountOverflow) {
		t.Errorf("threshold scan: fired=%d errs=%v, want no fire and one ErrAmountOverflow", fired, errs)
	}
	if invs := lf.listInvoices(t, lf.ctx); len(invs) != 0 {
		t.Errorf("%d invoices, want none", len(invs))
	}
}
