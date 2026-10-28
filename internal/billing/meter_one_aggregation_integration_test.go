package billing_test

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/subscription"
)

// TestOneAggregationPerMeter_EveryPathUnchanged: a meter has one aggregation
// (ADR-130 §2, PR-2). Before it, each pricing rule carried its own, and a
// meter whose rules all used the meter's aggregation is the only shape left.
// For that shape every rating path must bill exactly what it billed before:
// the preview (close mirror and flat window), the live balance, the spend
// threshold invoice and its evaluation, and the period close or an immediate
// cancel. Each case records every output of those paths as text; the
// expected text was recorded on main before the change (1717f4a8), so the
// same test passing on both builds is the proof. Since ADR-130 PR-4 the
// prices are plan prices: the two pricing rules are two prices for model=a
// and model=b, and the meter's own price is the plan's catch-all price. A
// line names its price as it named its rule's key before.
//
// The events are at distinct instants: a timestamp tie on a `last` meter is
// the one place the change bills differently on purpose (the id breaks it on
// every path; TestAggregateLast_TimestampTieBreaksByID).
func TestOneAggregationPerMeter_EveryPathUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	type event struct {
		qty   string
		model string
	}
	events := []event{{"4", "a"}, {"6", "b"}, {"3", "c"}, {"2", "a"}, {"5", "b"}, {"7", "c"}}

	for _, agg := range []string{"sum", "count", "max", "last"} {
		for _, shape := range []string{"two pricing rules and the meter's own price", "the meter's own price only"} {
			for _, end := range []string{"period close", "immediate cancel"} {
				if end == "immediate cancel" && (shape != "two pricing rules and the meter's own price" || agg != "max") {
					continue
				}
				name := agg + ", " + shape + ", " + end
				t.Run(name, func(t *testing.T) {
					lf := newLiveFixture(t, "One aggregation "+name, 0)
					ctx := lf.ctx
					bypassExec(t, lf, `UPDATE meters SET aggregation = $1 WHERE id = $2`, agg, lf.meterID)
					keys := map[string]string{lf.priceID: "calls_flat"}
					if shape == "two pricing rules and the meter's own price" {
						a := createRule(t, lf, map[string]string{"model": "a"}, flatRule("model_a", "2"))
						b := createRule(t, lf, map[string]string{"model": "b"}, gradRule("model_b", []domain.RatingTier{
							{UpTo: new(int64(3)), UnitAmountCents: decimal.NewFromInt(5)},
							{UpTo: nil, UnitAmountCents: decimal.NewFromInt(1)},
						}))
						keys[a.ID], keys[b.ID] = "model_a", "model_b"
					}
					for i, ev := range events {
						insertUsageAt(t, lf, lf.cycleStart.Add(time.Duration(i+1)*time.Minute), ev.qty, map[string]any{"model": ev.model})
					}

					var out strings.Builder
					previews := billing.NewPreviewService(lf.engine, customer.NewPostgresStore(lf.db), lf.subStore)
					readAll := func(stage string) {
						t.Helper()
						p, err := previews.CreatePreview(ctx, lf.tenantID, billing.CreatePreviewRequest{CustomerID: lf.customerID, SubscriptionID: lf.subID})
						if err != nil {
							t.Fatalf("preview: %v", err)
						}
						writePreview(&out, stage+" preview", p, keys)
						p, err = previews.CreatePreview(ctx, lf.tenantID, billing.CreatePreviewRequest{
							CustomerID: lf.customerID, SubscriptionID: lf.subID,
							Period: billing.CreatePreviewPeriod{From: lf.cycleStart, To: time.Now().UTC()},
						})
						if err != nil {
							t.Fatalf("preview, explicit window: %v", err)
						}
						writePreview(&out, stage+" window preview", p, keys)
						fmt.Fprintf(&out, "%s live: unbilled=%d\n", stage, lf.live(t).UnbilledUsageCents)
					}
					readAll("open")

					if _, err := lf.subSvc.SetBillingThresholds(ctx, lf.tenantID, lf.subID, subscription.BillingThresholdsInput{AmountGTE: 1}); err != nil {
						t.Fatalf("set thresholds: %v", err)
					}
					// A max or last meter's buckets are not additive, so the
					// threshold invoice defers them to the close and, with no
					// other line, is not issued (ADR-066 §4).
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
						insertUsageAt(t, lf, fireEnd.Add(time.Duration(i+1)*100*time.Millisecond), ev.qty, map[string]any{"model": ev.model})
					}
					// Every read below reads up to now: wait until every event is
					// in the past, so each read sees all of them.
					if wait := time.Until(fireEnd.Add(time.Duration(len(events)+1) * 100 * time.Millisecond)); wait > 0 {
						time.Sleep(wait)
					}
					readAll("after fire")

					periodEnd := fireEnd.Add(2 * time.Second).Truncate(time.Microsecond)
					if wait := time.Until(periodEnd.Add(500 * time.Millisecond)); wait > 0 {
						time.Sleep(wait)
					}
					if end == "immediate cancel" {
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
						fmt.Fprintf(&out, "invoice %s: subtotal=%d total=%d\n", inv.BillingReason, inv.SubtotalCents, inv.TotalAmountCents)
						lines, err := lf.invStore.ListLineItems(ctx, lf.tenantID, inv.ID)
						if err != nil {
							t.Fatalf("list lines: %v", err)
						}
						for _, l := range lines {
							fmt.Fprintf(&out, "  line %s %q qty=%s unit=%d amount=%d mode=%s rule=%s\n",
								l.LineType, l.Description, l.QuantityDecimal.String(), l.UnitAmountCents, l.AmountCents, l.PricingMode, keys[l.PriceID])
						}
					}

					got := out.String()
					want, ok := oneAggregationGolden[name]
					if !ok {
						t.Fatalf("no recorded output for %q; got:\n%s", name, got)
					}
					if got != want {
						t.Errorf("output changed:\n--- want\n%s--- got\n%s", want, got)
					}
				})
			}
		}
	}
}

func writePreview(out *strings.Builder, stage string, p billing.PreviewResult, keys map[string]string) {
	fmt.Fprintf(out, "%s:\n", stage)
	for _, l := range p.Lines {
		var dm []string
		for k, v := range l.DimensionMatch {
			dm = append(dm, fmt.Sprintf("%s=%v", k, v))
		}
		sort.Strings(dm)
		fmt.Fprintf(out, "  line %s %q qty=%s unit=%d amount=%d mode=%s rule=%s match=%s\n",
			l.LineType, l.Description, l.Quantity.String(), l.UnitAmountCents, l.AmountCents, l.PricingMode, keys[l.PriceID], strings.Join(dm, ","))
	}
	for _, tot := range p.Totals {
		fmt.Fprintf(out, "  total %+v\n", tot)
	}
	for _, w := range p.Warnings {
		fmt.Fprintf(out, "  warning %s\n", firedThrough.ReplaceAllString(w, "through <the threshold invoice's end>"))
	}
}

// firedThrough is the instant a preview warning names, which differs per run.
var firedThrough = regexp.MustCompile(`through [A-Z][a-z]{2} \d{1,2}, \d{4} \d{2}:\d{2} UTC`)

// oneAggregationGolden is each case's output, recorded on main at 1717f4a8.
// ADR-130 §3 (PR-3) renamed the flat shape per_unit; the line label
// mode=flat → mode=per_unit is the only edit to this text since.
var oneAggregationGolden = map[string]string{
	"sum, two pricing rules and the meter's own price, period close": `open preview:
  line usage "Calls (a)" qty=6 unit=2 amount=12 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=11 unit=2 amount=23 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=10 unit=1 amount=10 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:45}
open window preview:
  line usage "Calls (a)" qty=6 unit=2 amount=12 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=11 unit=2 amount=23 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=10 unit=1 amount=10 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:45}
open live: unbilled=45
threshold invoices: 1
after fire preview:
  line usage "Calls (a)" qty=6 unit=2 amount=12 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=11 unit=1 amount=11 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=10 unit=1 amount=10 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:33}
  warning A spend-threshold invoice already billed this period through <the threshold invoice's end>; this estimate shows only what is left. Tiered prices and max or last-value meters may differ on the actual invoice.
after fire window preview:
  line usage "Calls (a)" qty=12 unit=2 amount=24 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=22 unit=2 amount=34 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=20 unit=1 amount=20 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:78}
after fire live: unbilled=33
invoice threshold: subtotal=45 total=45
  line usage "Calls (a)" qty=6 unit=2 amount=12 mode=per_unit rule=model_a
  line usage "Calls (b)" qty=11 unit=2 amount=23 mode=graduated rule=model_b
  line usage "Calls (calls)" qty=10 unit=1 amount=10 mode=per_unit rule=calls_flat
invoice subscription_cycle: subtotal=33 total=33
  line usage "Calls (a)" qty=6 unit=2 amount=12 mode=per_unit rule=model_a
  line usage "Calls (b)" qty=11 unit=1 amount=11 mode=graduated rule=model_b
  line usage "Calls (calls)" qty=10 unit=1 amount=10 mode=per_unit rule=calls_flat
`,
	"sum, the meter's own price only, period close": `open preview:
  line usage "Calls (calls)" qty=27 unit=1 amount=27 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:27}
open window preview:
  line usage "Calls (calls)" qty=27 unit=1 amount=27 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:27}
open live: unbilled=27
threshold invoices: 1
after fire preview:
  line usage "Calls (calls)" qty=27 unit=1 amount=27 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:27}
  warning A spend-threshold invoice already billed this period through <the threshold invoice's end>; this estimate shows only what is left. Tiered prices and max or last-value meters may differ on the actual invoice.
after fire window preview:
  line usage "Calls (calls)" qty=54 unit=1 amount=54 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:54}
after fire live: unbilled=27
invoice threshold: subtotal=27 total=27
  line usage "Calls (calls)" qty=27 unit=1 amount=27 mode=per_unit rule=calls_flat
invoice subscription_cycle: subtotal=27 total=27
  line usage "Calls (calls)" qty=27 unit=1 amount=27 mode=per_unit rule=calls_flat
`,
	"count, two pricing rules and the meter's own price, period close": `open preview:
  line usage "Calls (a)" qty=2 unit=2 amount=4 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=2 unit=5 amount=10 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=2 unit=1 amount=2 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:16}
open window preview:
  line usage "Calls (a)" qty=2 unit=2 amount=4 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=2 unit=5 amount=10 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=2 unit=1 amount=2 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:16}
open live: unbilled=16
threshold invoices: 1
after fire preview:
  line usage "Calls (a)" qty=2 unit=2 amount=4 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=2 unit=3 amount=6 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=2 unit=1 amount=2 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:12}
  warning A spend-threshold invoice already billed this period through <the threshold invoice's end>; this estimate shows only what is left. Tiered prices and max or last-value meters may differ on the actual invoice.
after fire window preview:
  line usage "Calls (a)" qty=4 unit=2 amount=8 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=4 unit=4 amount=16 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=4 unit=1 amount=4 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:28}
after fire live: unbilled=12
invoice threshold: subtotal=16 total=16
  line usage "Calls (a)" qty=2 unit=2 amount=4 mode=per_unit rule=model_a
  line usage "Calls (b)" qty=2 unit=5 amount=10 mode=graduated rule=model_b
  line usage "Calls (calls)" qty=2 unit=1 amount=2 mode=per_unit rule=calls_flat
invoice subscription_cycle: subtotal=12 total=12
  line usage "Calls (a)" qty=2 unit=2 amount=4 mode=per_unit rule=model_a
  line usage "Calls (b)" qty=2 unit=3 amount=6 mode=graduated rule=model_b
  line usage "Calls (calls)" qty=2 unit=1 amount=2 mode=per_unit rule=calls_flat
`,
	"count, the meter's own price only, period close": `open preview:
  line usage "Calls (calls)" qty=6 unit=1 amount=6 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:6}
open window preview:
  line usage "Calls (calls)" qty=6 unit=1 amount=6 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:6}
open live: unbilled=6
threshold invoices: 1
after fire preview:
  line usage "Calls (calls)" qty=6 unit=1 amount=6 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:6}
  warning A spend-threshold invoice already billed this period through <the threshold invoice's end>; this estimate shows only what is left. Tiered prices and max or last-value meters may differ on the actual invoice.
after fire window preview:
  line usage "Calls (calls)" qty=12 unit=1 amount=12 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:12}
after fire live: unbilled=6
invoice threshold: subtotal=6 total=6
  line usage "Calls (calls)" qty=6 unit=1 amount=6 mode=per_unit rule=calls_flat
invoice subscription_cycle: subtotal=6 total=6
  line usage "Calls (calls)" qty=6 unit=1 amount=6 mode=per_unit rule=calls_flat
`,
	"max, two pricing rules and the meter's own price, period close": `open preview:
  line usage "Calls (a)" qty=4 unit=2 amount=8 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=6 unit=3 amount=18 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:33}
open window preview:
  line usage "Calls (a)" qty=4 unit=2 amount=8 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=6 unit=3 amount=18 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:33}
open live: unbilled=33
threshold invoices: 0
after fire preview:
  line usage "Calls (a)" qty=4 unit=2 amount=8 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=6 unit=3 amount=18 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:33}
after fire window preview:
  line usage "Calls (a)" qty=4 unit=2 amount=8 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=6 unit=3 amount=18 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:33}
after fire live: unbilled=33
invoice subscription_cycle: subtotal=33 total=33
  line usage "Calls (a)" qty=4 unit=2 amount=8 mode=per_unit rule=model_a
  line usage "Calls (b)" qty=6 unit=3 amount=18 mode=graduated rule=model_b
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat
`,
	"max, two pricing rules and the meter's own price, immediate cancel": `open preview:
  line usage "Calls (a)" qty=4 unit=2 amount=8 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=6 unit=3 amount=18 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:33}
open window preview:
  line usage "Calls (a)" qty=4 unit=2 amount=8 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=6 unit=3 amount=18 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:33}
open live: unbilled=33
threshold invoices: 0
after fire preview:
  line usage "Calls (a)" qty=4 unit=2 amount=8 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=6 unit=3 amount=18 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:33}
after fire window preview:
  line usage "Calls (a)" qty=4 unit=2 amount=8 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=6 unit=3 amount=18 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:33}
after fire live: unbilled=33
invoice subscription_cancel: subtotal=33 total=33
  line usage "Calls (a) - canceled mid-period" qty=4 unit=2 amount=8 mode=per_unit rule=model_a
  line usage "Calls (b) - canceled mid-period" qty=6 unit=3 amount=18 mode=graduated rule=model_b
  line usage "Calls (calls) - canceled mid-period" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat
`,
	"max, the meter's own price only, period close": `open preview:
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:7}
open window preview:
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:7}
open live: unbilled=7
threshold invoices: 0
after fire preview:
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:7}
after fire window preview:
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:7}
after fire live: unbilled=7
invoice subscription_cycle: subtotal=7 total=7
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat
`,
	"last, two pricing rules and the meter's own price, period close": `open preview:
  line usage "Calls (a)" qty=2 unit=2 amount=4 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=5 unit=3 amount=17 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:28}
open window preview:
  line usage "Calls (a)" qty=2 unit=2 amount=4 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=5 unit=3 amount=17 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:28}
open live: unbilled=28
threshold invoices: 0
after fire preview:
  line usage "Calls (a)" qty=2 unit=2 amount=4 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=5 unit=3 amount=17 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:28}
after fire window preview:
  line usage "Calls (a)" qty=2 unit=2 amount=4 mode=per_unit rule=model_a match=model=a
  line usage "Calls (b)" qty=5 unit=3 amount=17 mode=graduated rule=model_b match=model=b
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:28}
after fire live: unbilled=28
invoice subscription_cycle: subtotal=28 total=28
  line usage "Calls (a)" qty=2 unit=2 amount=4 mode=per_unit rule=model_a
  line usage "Calls (b)" qty=5 unit=3 amount=17 mode=graduated rule=model_b
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat
`,
	"last, the meter's own price only, period close": `open preview:
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:7}
open window preview:
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:7}
open live: unbilled=7
threshold invoices: 0
after fire preview:
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:7}
after fire window preview:
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat match=
  total {Currency:USD AmountCents:7}
after fire live: unbilled=7
invoice subscription_cycle: subtotal=7 total=7
  line usage "Calls (calls)" qty=7 unit=1 amount=7 mode=per_unit rule=calls_flat
`,
}
