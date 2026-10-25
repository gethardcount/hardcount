package billing_test

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// ADR-130 §3 (PR-3) gives the price shapes their names and changes how a
// package bills a part package. The tests here run each price through every
// rating path, against real Postgres: the preview (close mirror and flat
// window), the live balance, the spend-threshold invoice, the preview and
// live balance after it, and the period close or an immediate cancel. Each
// records the money of those paths as text.
//
//   - TestPriceShapes_ExpressibleBeforeBillUnchanged: every price that could
//     be written before and can still be written bills exactly what it
//     billed before. Its expected text was recorded on main (7b110a6c), so
//     the same test passing on both builds is the proof. Its prices are
//     written by the helpers at the bottom, the only part of this file that
//     changes with the build.
//   - TestPriceShapes_EachShapeBillsItsGolden: each shape, written in the
//     API's words, bills the amounts worked out by hand in its comments. Red
//     on main, which has none of these words.
//
// Mutation-verify: price packages with floor instead of ceil → the two
// package cases of the golden go red.

// shapeEvent is one usage event of a story: its quantity, and its model
// dimension ("" for none).
type shapeEvent struct {
	qty   string
	model string
}

// runPriceStory sends the events, reads every rating path, fires a spend
// threshold, sends the events again, reads every path again and ends the
// period (or cancels), then returns what each read and invoice billed. keys
// names each rating rule version in the text. withMode adds each line's
// pricing_mode.
func runPriceStory(t *testing.T, lf *liveFixture, keys map[string]string, events []shapeEvent, cancel, withMode bool) string {
	t.Helper()
	ctx := lf.ctx
	dims := func(model string) map[string]any {
		if model == "" {
			return nil
		}
		return map[string]any{"model": model}
	}
	for i, ev := range events {
		insertUsageAt(t, lf, lf.cycleStart.Add(time.Duration(i+1)*time.Minute), ev.qty, dims(ev.model))
	}
	mode := func(m string) string {
		if !withMode {
			return ""
		}
		return " mode=" + m
	}

	var out strings.Builder
	previews := billing.NewPreviewService(lf.engine, customer.NewPostgresStore(lf.db), lf.subStore)
	writePreviewLines := func(stage string, p billing.PreviewResult) {
		fmt.Fprintf(&out, "%s:\n", stage)
		for _, l := range p.Lines {
			var dm []string
			for k, v := range l.DimensionMatch {
				dm = append(dm, fmt.Sprintf("%s=%v", k, v))
			}
			sort.Strings(dm)
			fmt.Fprintf(&out, "  line %s %q qty=%s unit=%d amount=%d%s rule=%s match=%s\n",
				l.LineType, l.Description, l.Quantity.String(), l.UnitAmountCents, l.AmountCents, mode(l.PricingMode), l.RuleKey, strings.Join(dm, ","))
		}
		for _, tot := range p.Totals {
			fmt.Fprintf(&out, "  total %+v\n", tot)
		}
		for _, w := range p.Warnings {
			fmt.Fprintf(&out, "  warning %s\n", firedThrough.ReplaceAllString(w, "through <the threshold invoice's end>"))
		}
	}
	readAll := func(stage string) {
		t.Helper()
		p, err := previews.CreatePreview(ctx, lf.tenantID, billing.CreatePreviewRequest{CustomerID: lf.customerID, SubscriptionID: lf.subID})
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		writePreviewLines(stage+" preview", p)
		p, err = previews.CreatePreview(ctx, lf.tenantID, billing.CreatePreviewRequest{
			CustomerID: lf.customerID, SubscriptionID: lf.subID,
			Period: billing.CreatePreviewPeriod{From: lf.cycleStart, To: time.Now().UTC()},
		})
		if err != nil {
			t.Fatalf("preview, explicit window: %v", err)
		}
		writePreviewLines(stage+" window preview", p)
		fmt.Fprintf(&out, "%s live: unbilled=%d\n", stage, lf.live(t).UnbilledUsageCents)
	}
	readAll("open")

	if _, err := lf.subSvc.SetBillingThresholds(ctx, lf.tenantID, lf.subID, subscription.BillingThresholdsInput{AmountGTE: 1}); err != nil {
		t.Fatalf("set thresholds: %v", err)
	}
	fired, errs := lf.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("threshold scan: %v", errs)
	}
	fmt.Fprintf(&out, "threshold invoices: %d\n", fired)
	fireEnd := time.Now().UTC()
	if invs := lf.listInvoices(t, ctx); len(invs) == 1 {
		fireEnd = invs[0].BillingPeriodEnd.UTC()
	}
	for i, ev := range events {
		insertUsageAt(t, lf, fireEnd.Add(time.Duration(i+1)*100*time.Millisecond), ev.qty, dims(ev.model))
	}
	if wait := time.Until(fireEnd.Add(time.Duration(len(events)+1) * 100 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	readAll("after fire")

	periodEnd := fireEnd.Add(2 * time.Second).Truncate(time.Microsecond)
	if wait := time.Until(periodEnd.Add(500 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	if cancel {
		canceledAt := time.Now().UTC().Truncate(time.Microsecond)
		bypassExec(t, lf, `UPDATE subscriptions SET status = 'canceled', canceled_at = $1 WHERE id = $2`, canceledAt, lf.subID)
		sub, err := lf.subStore.Get(ctx, lf.tenantID, lf.subID)
		if err != nil {
			t.Fatalf("get sub: %v", err)
		}
		if inv, err := lf.engine.BillFinalOnImmediateCancel(ctx, sub); err != nil || inv.ID == "" {
			t.Fatalf("cancel final invoice: inv=%q err=%v", inv.ID, err)
		}
	} else {
		bypassExec(t, lf, `UPDATE subscriptions SET current_billing_period_end = $1, next_billing_at = $1 WHERE id = $2`, periodEnd, lf.subID)
		if generated, failures := lf.engine.RunCycleForTenant(ctx, lf.tenantID, 50); len(failures) > 0 || generated != 1 {
			t.Fatalf("period close: generated=%d failures=%v, want 1", generated, failures)
		}
	}

	invs := lf.listInvoices(t, ctx)
	sort.Slice(invs, func(i, j int) bool { return invs[i].BillingPeriodEnd.Before(invs[j].BillingPeriodEnd) })
	for _, inv := range invs {
		fmt.Fprintf(&out, "invoice %s: subtotal=%d tax=%d total=%d\n", inv.BillingReason, inv.SubtotalCents, inv.TaxAmountCents, inv.TotalAmountCents)
		lines, err := lf.invStore.ListLineItems(ctx, lf.tenantID, inv.ID)
		if err != nil {
			t.Fatalf("list lines: %v", err)
		}
		for _, l := range lines {
			fmt.Fprintf(&out, "  line %s %q qty=%s unit=%d amount=%d%s rule=%s\n",
				l.LineType, l.Description, l.QuantityDecimal.String(), l.UnitAmountCents, l.AmountCents, mode(l.PricingMode), keys[l.RatingRuleVersionID])
		}
	}
	return out.String()
}

func TestPriceShapes_ExpressibleBeforeBillUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	cases := []struct {
		name   string
		price  func(t *testing.T, lf *liveFixture) map[string]string // rule id → key
		events []shapeEvent
		tax    bool
		cancel bool
	}{
		{
			name: "per-unit decimal rate beside the meter's own rate",
			price: func(t *testing.T, lf *liveFixture) map[string]string {
				a := createRule(t, lf, shapePerUnit("model_a", "0.0035"))
				seedPricingRule(t, lf.db, lf.ctx, lf.tenantID, pricing.UpsertMeterPricingRuleInput{
					MeterID: lf.meterID, RatingRuleVersionID: a.ID, DimensionMatch: map[string]any{"model": "a"}, Priority: 10,
				})
				return map[string]string{a.ID: "model_a"}
			},
			events: []shapeEvent{{"1234.5", "a"}, {"0.25", "a"}, {"999999.125", "a"}, {"33", "c"}},
		},
		{
			name: "graduated, open last tier, tiers continue across the threshold invoice",
			price: func(t *testing.T, lf *liveFixture) map[string]string {
				v := publishCallsRule(t, lf, shapeGraduated("calls_flat", []int64{100}, "5", "1.25"))
				return map[string]string{v.ID: "calls_flat"}
			},
			events: []shapeEvent{{"60", ""}, {"70", ""}},
		},
		{
			name: "package, whole packages",
			price: func(t *testing.T, lf *liveFixture) map[string]string {
				v := publishCallsRule(t, lf, shapeWholePackage("calls_flat", 50, 120))
				return map[string]string{v.ID: "calls_flat"}
			},
			events: []shapeEvent{{"50", ""}, {"100", ""}},
		},
		{
			name: "graduated customer override, immediate cancel",
			price: func(t *testing.T, lf *liveFixture) map[string]string {
				v := publishCallsRule(t, lf, shapeGraduated("calls_flat", []int64{500}, "4", "1"))
				o, err := lf.pricingSvc.CreateOverride(lf.ctx, lf.tenantID, shapeGraduatedOverride(lf.customerID, v.ID, []int64{300}, "3.25", "0.75"))
				if err != nil {
					t.Fatalf("create override: %v", err)
				}
				bypassExec(t, lf, `UPDATE customer_price_overrides SET created_at = $1 WHERE id = $2`, lf.cycleStart.Add(-time.Hour), o.ID)
				return map[string]string{v.ID: "calls_flat"}
			},
			events: []shapeEvent{{"120", ""}, {"260", ""}},
			cancel: true,
		},
		{
			name: "tax-inclusive tenant, graduated",
			price: func(t *testing.T, lf *liveFixture) map[string]string {
				v := publishCallsRule(t, lf, shapeGraduated("calls_flat", []int64{400}, "7", "2.5"))
				return map[string]string{v.ID: "calls_flat"}
			},
			events: []shapeEvent{{"150", ""}, {"180", ""}},
			tax:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lf := newLiveFixture(t, "Shapes unchanged "+tc.name, 0)
			keys := map[string]string{lf.seededRule(t, lf.ctx).ID: "calls_flat"}
			for id, k := range tc.price(t, lf) {
				keys[id] = k
			}
			if tc.tax {
				setTaxInclusive(t, lf, 18)
			}
			got := runPriceStory(t, lf, keys, tc.events, tc.cancel, false)
			want, ok := shapesUnchangedGolden[tc.name]
			if !ok {
				t.Fatalf("no recorded output for %q; got:\n%s", tc.name, got)
			}
			if got != want {
				t.Errorf("output changed:\n--- want\n%s--- got\n%s", want, got)
			}
		})
	}
}

// TestPriceShapes_EachShapeBillsItsGolden: each shape, written in the API's
// words (the JSON a POST /v1/rating-rules or /v1/price-overrides body
// carries), on every rating path. The story sends the events, fires a spend
// threshold, sends them again and ends the period, so the second half shows
// how a ladder continues past the threshold invoice. Each expected amount is
// worked out by hand below; a line rounds once, half to even.
//
// What changes on purpose, against what the same price billed before:
//
//   - a package bills a part package as a whole one (ceil). Before, a part
//     package billed only its overage rate, 0 when there was none: 130 units
//     in packages of 100 at 120.5 cents bill 241 here; before they billed
//     one package, and 120.5 could not be written at all (whole cents only);
//   - the open last tier is written "up_to": null. Its money is unchanged
//     (the graduated cases of the test above); "up_to": 0 is refused when
//     saved (pricing's TestPriceShapes_TierTableCheckedWhenSaved).
func TestPriceShapes_EachShapeBillsItsGolden(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	cases := []struct {
		name     string
		rule     string // the meter's price, as a POST /v1/rating-rules body
		override string // a price override for the customer, as a POST /v1/price-overrides body without its ids
		events   []shapeEvent
		cancel   bool
		want     string
	}{
		{
			// 1000.5 units × 0.0035 = 3.50175 → 4 cents on each half; the
			// flat window holds 2001 units: 7.0035 → 7.
			name:   "per_unit at a decimal rate",
			rule:   `{"rule_key":"calls_flat","name":"calls_flat","mode":"per_unit","currency":"USD","unit_amount_cents":"0.0035"}`,
			events: []shapeEvent{{"1000", ""}, {"0.5", ""}},
			want: `open preview:
  line usage "Calls (calls)" qty=1000.5 unit=0 amount=4 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:4}
open window preview:
  line usage "Calls (calls)" qty=1000.5 unit=0 amount=4 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:4}
open live: unbilled=4
threshold invoices: 1
after fire preview:
  line usage "Calls (calls)" qty=1000.5 unit=0 amount=4 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:4}
  warning A spend-threshold invoice already billed this period through <the threshold invoice's end>; this estimate shows only what is left. Tiered prices and max or last-value meters may differ on the actual invoice.
after fire window preview:
  line usage "Calls (calls)" qty=2001 unit=0 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:7}
after fire live: unbilled=4
invoice threshold: subtotal=4 tax=0 total=4
  line usage "Calls (calls)" qty=1000.5 unit=0 amount=4 mode=per_unit rule=calls_flat
invoice subscription_cycle: subtotal=4 tax=0 total=4
  line usage "Calls (calls)" qty=1000.5 unit=0 amount=4 mode=per_unit rule=calls_flat
`,
		},
		{
			// Tiers: 100 at 5, up to 1000 at 2.5, the rest at 1.25.
			// 1030 units: 500 + 2250 + 30 × 1.25 = 2787.5 → 2788.
			// 2060 units: 2750 + 1060 × 1.25 = 4075; the close bills
			// 4075 − 2788 = 1287, so the period bills F(2060) in all.
			name: "graduated, the last tier open, across the threshold invoice",
			rule: `{"rule_key":"calls_flat","name":"calls_flat","mode":"graduated","currency":"USD","tiers":[
				{"up_to":100,"unit_amount_cents":"5"},{"up_to":1000,"unit_amount_cents":"2.5"},{"up_to":null,"unit_amount_cents":"1.25"}]}`,
			events: []shapeEvent{{"60", ""}, {"70", ""}, {"900", ""}},
			want: `open preview:
  line usage "Calls (calls)" qty=1030 unit=3 amount=2788 mode=graduated rule=calls_flat match=
  total {Currency:USD AmountCents:2788}
open window preview:
  line usage "Calls (calls)" qty=1030 unit=3 amount=2788 mode=graduated rule=calls_flat match=
  total {Currency:USD AmountCents:2788}
open live: unbilled=2788
threshold invoices: 1
after fire preview:
  line usage "Calls (calls)" qty=1030 unit=1 amount=1287 mode=graduated rule=calls_flat match=
  total {Currency:USD AmountCents:1287}
  warning A spend-threshold invoice already billed this period through <the threshold invoice's end>; this estimate shows only what is left. Tiered prices and max or last-value meters may differ on the actual invoice.
after fire window preview:
  line usage "Calls (calls)" qty=2060 unit=2 amount=4075 mode=graduated rule=calls_flat match=
  total {Currency:USD AmountCents:4075}
after fire live: unbilled=1287
invoice threshold: subtotal=2788 tax=0 total=2788
  line usage "Calls (calls)" qty=1030 unit=3 amount=2788 mode=graduated rule=calls_flat
invoice subscription_cycle: subtotal=1287 tax=0 total=1287
  line usage "Calls (calls)" qty=1030 unit=1 amount=1287 mode=graduated rule=calls_flat
`,
		},
		{
			// Packages of 100 at 120.5 cents. 130 units: 2 packages = 241.
			// 260 units: 3 packages = 361.5 → 362; the close bills
			// 362 − 241 = 121.
			name:   "package, a part package bills as a whole one",
			rule:   `{"rule_key":"calls_flat","name":"calls_flat","mode":"package","currency":"USD","package_size":100,"package_amount_cents":"120.5"}`,
			events: []shapeEvent{{"60", ""}, {"70", ""}},
			want: `open preview:
  line usage "Calls (calls)" qty=130 unit=2 amount=241 mode=package rule=calls_flat match=
  total {Currency:USD AmountCents:241}
open window preview:
  line usage "Calls (calls)" qty=130 unit=2 amount=241 mode=package rule=calls_flat match=
  total {Currency:USD AmountCents:241}
open live: unbilled=241
threshold invoices: 1
after fire preview:
  line usage "Calls (calls)" qty=130 unit=1 amount=121 mode=package rule=calls_flat match=
  total {Currency:USD AmountCents:121}
  warning A spend-threshold invoice already billed this period through <the threshold invoice's end>; this estimate shows only what is left. Tiered prices and max or last-value meters may differ on the actual invoice.
after fire window preview:
  line usage "Calls (calls)" qty=260 unit=1 amount=362 mode=package rule=calls_flat match=
  total {Currency:USD AmountCents:362}
after fire live: unbilled=121
invoice threshold: subtotal=241 tax=0 total=241
  line usage "Calls (calls)" qty=130 unit=2 amount=241 mode=package rule=calls_flat
invoice subscription_cycle: subtotal=121 tax=0 total=121
  line usage "Calls (calls)" qty=130 unit=1 amount=121 mode=package rule=calls_flat
`,
		},
		{
			// The list price is 1 cent a unit; the customer's override is
			// packages of 50 at 33.3 cents. 60 units: 2 packages = 66.6 →
			// 67. 120 units: 3 packages = 99.9 → 100; the cancel bills
			// 100 − 67 = 33.
			name:     "package override at a decimal amount, immediate cancel",
			rule:     `{"rule_key":"calls_flat","name":"calls_flat","mode":"per_unit","currency":"USD","unit_amount_cents":"1"}`,
			override: `"mode":"package","package_size":50,"package_amount_cents":"33.3","reason":"negotiated"`,
			events:   []shapeEvent{{"20", ""}, {"40", ""}},
			cancel:   true,
			want: `open preview:
  line usage "Calls (calls)" qty=60 unit=1 amount=67 mode=package rule=calls_flat match=
  total {Currency:USD AmountCents:67}
open window preview:
  line usage "Calls (calls)" qty=60 unit=1 amount=67 mode=package rule=calls_flat match=
  total {Currency:USD AmountCents:67}
open live: unbilled=67
threshold invoices: 1
after fire preview:
  line usage "Calls (calls)" qty=60 unit=1 amount=33 mode=package rule=calls_flat match=
  total {Currency:USD AmountCents:33}
  warning A spend-threshold invoice already billed this period through <the threshold invoice's end>; this estimate shows only what is left. Tiered prices and max or last-value meters may differ on the actual invoice.
after fire window preview:
  line usage "Calls (calls)" qty=120 unit=1 amount=100 mode=package rule=calls_flat match=
  total {Currency:USD AmountCents:100}
after fire live: unbilled=33
invoice threshold: subtotal=67 tax=0 total=67
  line usage "Calls (calls)" qty=60 unit=1 amount=67 mode=package rule=calls_flat
invoice subscription_cancel: subtotal=33 tax=0 total=33
  line usage "Calls (calls) - canceled mid-period" qty=60 unit=1 amount=33 mode=package rule=calls_flat
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lf := newLiveFixture(t, "Shapes golden "+tc.name, 0)
			var in pricing.CreateRatingRuleInput
			if err := json.Unmarshal([]byte(tc.rule), &in); err != nil {
				t.Fatalf("decode rule %s: %v", tc.rule, err)
			}
			v := publishCallsRule(t, lf, in)
			keys := map[string]string{lf.seededRule(t, lf.ctx).ID: "calls_flat", v.ID: "calls_flat"}
			if tc.override != "" {
				var oin pricing.CreateOverrideInput
				body := `{"customer_id":"` + lf.customerID + `","rating_rule_version_id":"` + v.ID + `",` + tc.override + `}`
				if err := json.Unmarshal([]byte(body), &oin); err != nil {
					t.Fatalf("decode override %s: %v", body, err)
				}
				o, err := lf.pricingSvc.CreateOverride(lf.ctx, lf.tenantID, oin)
				if err != nil {
					t.Fatalf("create override: %v", err)
				}
				bypassExec(t, lf, `UPDATE customer_price_overrides SET created_at = $1 WHERE id = $2`, lf.cycleStart.Add(-time.Hour), o.ID)
			}
			if got := runPriceStory(t, lf, keys, tc.events, tc.cancel, true); got != tc.want {
				t.Errorf("output:\n--- want\n%s--- got\n%s", tc.want, got)
			}
		})
	}
}

// setTaxInclusive gives the tenant a manual tax rate, included in prices.
func setTaxInclusive(t *testing.T, lf *liveFixture, rate float64) {
	t.Helper()
	setManualTax(t, lf.ctx, lf.settings, lf.tenantID, rate)
	ts, err := lf.settings.Get(lf.ctx, lf.tenantID)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	ts.TaxInclusive = true
	if _, err := lf.settings.Upsert(lf.ctx, ts); err != nil {
		t.Fatalf("upsert settings: %v", err)
	}
}

// shapesUnchangedGolden is each case's output, recorded on main at 7b110a6c.
var shapesUnchangedGolden = map[string]string{
	"per-unit decimal rate beside the meter's own rate": `open preview:
  line usage "Calls (a)" qty=1001233.875 unit=0 amount=3504 rule=model_a match=model=a
  line usage "Calls (calls)" qty=33 unit=1 amount=33 rule=calls_flat match=
  total {Currency:USD AmountCents:3537}
open window preview:
  line usage "Calls (a)" qty=1001233.875 unit=0 amount=3504 rule=model_a match=model=a
  line usage "Calls (calls)" qty=33 unit=1 amount=33 rule=calls_flat match=
  total {Currency:USD AmountCents:3537}
open live: unbilled=3537
threshold invoices: 1
after fire preview:
  line usage "Calls (a)" qty=1001233.875 unit=0 amount=3504 rule=model_a match=model=a
  line usage "Calls (calls)" qty=33 unit=1 amount=33 rule=calls_flat match=
  total {Currency:USD AmountCents:3537}
  warning A spend-threshold invoice already billed this period through <the threshold invoice's end>; this estimate shows only what is left. Tiered prices and max or last-value meters may differ on the actual invoice.
after fire window preview:
  line usage "Calls (a)" qty=2002467.75 unit=0 amount=7009 rule=model_a match=model=a
  line usage "Calls (calls)" qty=66 unit=1 amount=66 rule=calls_flat match=
  total {Currency:USD AmountCents:7075}
after fire live: unbilled=3537
invoice threshold: subtotal=3537 tax=0 total=3537
  line usage "Calls (a)" qty=1001233.875 unit=0 amount=3504 rule=model_a
  line usage "Calls (calls)" qty=33 unit=1 amount=33 rule=calls_flat
invoice subscription_cycle: subtotal=3537 tax=0 total=3537
  line usage "Calls (a)" qty=1001233.875 unit=0 amount=3504 rule=model_a
  line usage "Calls (calls)" qty=33 unit=1 amount=33 rule=calls_flat
`,
	"graduated, open last tier, tiers continue across the threshold invoice": `open preview:
  line usage "Calls (calls)" qty=130 unit=4 amount=538 rule=calls_flat match=
  total {Currency:USD AmountCents:538}
open window preview:
  line usage "Calls (calls)" qty=130 unit=4 amount=538 rule=calls_flat match=
  total {Currency:USD AmountCents:538}
open live: unbilled=538
threshold invoices: 1
after fire preview:
  line usage "Calls (calls)" qty=130 unit=1 amount=162 rule=calls_flat match=
  total {Currency:USD AmountCents:162}
  warning A spend-threshold invoice already billed this period through <the threshold invoice's end>; this estimate shows only what is left. Tiered prices and max or last-value meters may differ on the actual invoice.
after fire window preview:
  line usage "Calls (calls)" qty=260 unit=3 amount=700 rule=calls_flat match=
  total {Currency:USD AmountCents:700}
after fire live: unbilled=162
invoice threshold: subtotal=538 tax=0 total=538
  line usage "Calls (calls)" qty=130 unit=4 amount=538 rule=calls_flat
invoice subscription_cycle: subtotal=162 tax=0 total=162
  line usage "Calls (calls)" qty=130 unit=1 amount=162 rule=calls_flat
`,
	"package, whole packages": `open preview:
  line usage "Calls (calls)" qty=150 unit=2 amount=360 rule=calls_flat match=
  total {Currency:USD AmountCents:360}
open window preview:
  line usage "Calls (calls)" qty=150 unit=2 amount=360 rule=calls_flat match=
  total {Currency:USD AmountCents:360}
open live: unbilled=360
threshold invoices: 1
after fire preview:
  line usage "Calls (calls)" qty=150 unit=2 amount=360 rule=calls_flat match=
  total {Currency:USD AmountCents:360}
  warning A spend-threshold invoice already billed this period through <the threshold invoice's end>; this estimate shows only what is left. Tiered prices and max or last-value meters may differ on the actual invoice.
after fire window preview:
  line usage "Calls (calls)" qty=300 unit=2 amount=720 rule=calls_flat match=
  total {Currency:USD AmountCents:720}
after fire live: unbilled=360
invoice threshold: subtotal=360 tax=0 total=360
  line usage "Calls (calls)" qty=150 unit=2 amount=360 rule=calls_flat
invoice subscription_cycle: subtotal=360 tax=0 total=360
  line usage "Calls (calls)" qty=150 unit=2 amount=360 rule=calls_flat
`,
	"graduated customer override, immediate cancel": `open preview:
  line usage "Calls (calls)" qty=380 unit=3 amount=1035 rule=calls_flat match=
  total {Currency:USD AmountCents:1035}
open window preview:
  line usage "Calls (calls)" qty=380 unit=3 amount=1035 rule=calls_flat match=
  total {Currency:USD AmountCents:1035}
open live: unbilled=1035
threshold invoices: 1
after fire preview:
  line usage "Calls (calls)" qty=380 unit=1 amount=285 rule=calls_flat match=
  total {Currency:USD AmountCents:285}
  warning A spend-threshold invoice already billed this period through <the threshold invoice's end>; this estimate shows only what is left. Tiered prices and max or last-value meters may differ on the actual invoice.
after fire window preview:
  line usage "Calls (calls)" qty=760 unit=2 amount=1320 rule=calls_flat match=
  total {Currency:USD AmountCents:1320}
after fire live: unbilled=285
invoice threshold: subtotal=1035 tax=0 total=1035
  line usage "Calls (calls)" qty=380 unit=3 amount=1035 rule=calls_flat
invoice subscription_cancel: subtotal=285 tax=0 total=285
  line usage "Calls (calls) - canceled mid-period" qty=380 unit=1 amount=285 rule=calls_flat
`,
	"tax-inclusive tenant, graduated": `open preview:
  line usage "Calls (calls)" qty=330 unit=7 amount=2310 rule=calls_flat match=
  total {Currency:USD AmountCents:2310}
open window preview:
  line usage "Calls (calls)" qty=330 unit=7 amount=2310 rule=calls_flat match=
  total {Currency:USD AmountCents:2310}
open live: unbilled=2310
threshold invoices: 1
after fire preview:
  line usage "Calls (calls)" qty=330 unit=3 amount=1140 rule=calls_flat match=
  total {Currency:USD AmountCents:1140}
  warning A spend-threshold invoice already billed this period through <the threshold invoice's end>; this estimate shows only what is left. Tiered prices and max or last-value meters may differ on the actual invoice.
after fire window preview:
  line usage "Calls (calls)" qty=660 unit=5 amount=3450 rule=calls_flat match=
  total {Currency:USD AmountCents:3450}
after fire live: unbilled=1140
invoice threshold: subtotal=1958 tax=352 total=2310
  line usage "Calls (calls)" qty=330 unit=7 amount=1958 rule=calls_flat
invoice subscription_cycle: subtotal=966 tax=174 total=1140
  line usage "Calls (calls)" qty=330 unit=3 amount=966 rule=calls_flat
`,
}

// The helpers below write a price that both builds can express, each in its
// build's words. They are the only part of this file that changes.

func shapePerUnit(key, cents string) pricing.CreateRatingRuleInput {
	return pricing.CreateRatingRuleInput{RuleKey: key, Name: key, Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.RequireFromString(cents)}
}

// shapeTiers is a tier table: one tier up to each bound, then an open last
// tier. rates has one more entry than bounds.
func shapeTiers(bounds []int64, rates ...string) []domain.RatingTier {
	tiers := make([]domain.RatingTier, 0, len(rates))
	for i, r := range rates {
		var upTo int64 // 0: the open last tier
		if i < len(bounds) {
			upTo = bounds[i]
		}
		tiers = append(tiers, domain.RatingTier{UpTo: upTo, UnitAmountCents: decimal.RequireFromString(r)})
	}
	return tiers
}

func shapeGraduated(key string, bounds []int64, rates ...string) pricing.CreateRatingRuleInput {
	return pricing.CreateRatingRuleInput{RuleKey: key, Name: key, Mode: domain.PricingGraduated, Currency: "USD", GraduatedTiers: shapeTiers(bounds, rates...)}
}

func shapeWholePackage(key string, size, cents int64) pricing.CreateRatingRuleInput {
	return pricing.CreateRatingRuleInput{RuleKey: key, Name: key, Mode: domain.PricingPackage, Currency: "USD", PackageSize: size, PackageAmountCents: cents}
}

func shapeGraduatedOverride(customerID, ruleID string, bounds []int64, rates ...string) pricing.CreateOverrideInput {
	return pricing.CreateOverrideInput{CustomerID: customerID, RatingRuleVersionID: ruleID, Mode: domain.PricingGraduated,
		GraduatedTiers: shapeTiers(bounds, rates...), Reason: "negotiated"}
}
