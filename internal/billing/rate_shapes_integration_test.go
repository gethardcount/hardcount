package billing_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/usage"
)

// TestRateShapes_PreviewEqualsTheInvoice: every path that prices a period's
// usage prices it through one rater (ADR-130 §4, PR-1), so the preview of a
// period equals what its invoice bills, and the live balance counts the same
// amount. Each case is a pricing shape the other fixtures do not reach on
// every path: package prices, decimal rates with fractional quantities, a
// multi-dimension meter that mixes package, graduated and a catch-all price,
// a graduated customer override, a tax-inclusive tenant whose tiers continue
// across a threshold invoice, and a test-clock subscription. Each case
// previews the open period (close mirror), reads the live balance, fires a
// spend threshold, previews again, and ends the period or cancels: the
// close-mirror preview just before the end must equal the final invoice's
// usage, and the live balance must equal that preview.
func TestRateShapes_PreviewEqualsTheInvoice(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	type event struct {
		qty  string
		dims map[string]any
	}
	cases := []struct {
		name         string
		price        func(t *testing.T, lf *liveFixture)
		events       []event // before the threshold invoice, and again after it
		taxInclusive bool
		testClock    bool // previews and the live balance only: clock-pinned subscriptions are closed by the clock's advance
		cancel       bool // end with an immediate cancel instead of the period close
	}{
		{
			name: "package",
			price: func(t *testing.T, lf *liveFixture) {
				publishCallsRule(t, lf, packageRule("calls_flat", 100, "250"))
			},
			events: []event{{qty: "130"}, {qty: "75"}, {qty: "260"}},
		},
		{
			name:   "decimal per-unit rate, fractional quantities",
			price:  func(t *testing.T, lf *liveFixture) { publishCallsRule(t, lf, flatRule("calls_flat", "0.0035")) },
			events: []event{{qty: "1234.5"}, {qty: "0.25"}, {qty: "999999.125"}},
			cancel: true,
		},
		{
			name: "multi-dimension: package, graduated and a catch-all price",
			price: func(t *testing.T, lf *liveFixture) {
				createRule(t, lf, map[string]string{"model": "a"}, packageRule("model_a_pkg", 50, "120.5"))
				createRule(t, lf, map[string]string{"model": "b"}, gradRule("model_b_grad", []domain.RatingTier{
					{UpTo: new(int64(200)), UnitAmountCents: decimal.RequireFromString("2.5")},
					{UpTo: nil, UnitAmountCents: decimal.RequireFromString("0.5")},
				}))
			},
			events: []event{
				{qty: "70", dims: map[string]any{"model": "a"}},
				{qty: "150", dims: map[string]any{"model": "b"}},
				{qty: "33", dims: map[string]any{"model": "c"}}, // the catch-all price: 1¢
			},
		},
		{
			name: "graduated customer override",
			price: func(t *testing.T, lf *liveFixture) {
				v2 := publishCallsRule(t, lf, gradRule("calls_flat", []domain.RatingTier{
					{UpTo: new(int64(500)), UnitAmountCents: decimal.NewFromInt(4)},
					{UpTo: nil, UnitAmountCents: decimal.NewFromInt(1)},
				}))
				o, err := lf.pricingSvc.CreateOverride(lf.ctx, lf.tenantID, pricing.CreateOverrideInput{
					CustomerID: lf.customerID, PriceID: v2.ID,
					PriceShape: domain.PriceShape{Mode: domain.PricingGraduated, Tiers: []domain.RatingTier{
						{UpTo: new(int64(300)), UnitAmountCents: decimal.RequireFromString("3.25")},
						{UpTo: nil, UnitAmountCents: decimal.RequireFromString("0.75")},
					}},
					Reason: "negotiated",
				})
				if err != nil {
					t.Fatalf("create override: %v", err)
				}
				bypassExec(t, lf, `UPDATE customer_price_overrides SET created_at = $1 WHERE id = $2`, lf.cycleStart.Add(-time.Hour), o.ID)
			},
			events: []event{{qty: "120"}, {qty: "260"}},
			cancel: true,
		},
		{
			name: "tax-inclusive tenant, graduated tiers continue across the threshold invoice",
			price: func(t *testing.T, lf *liveFixture) {
				publishCallsRule(t, lf, gradRule("calls_flat", []domain.RatingTier{
					{UpTo: new(int64(400)), UnitAmountCents: decimal.NewFromInt(7)},
					{UpTo: nil, UnitAmountCents: decimal.RequireFromString("2.5")},
				}))
			},
			events:       []event{{qty: "150"}, {qty: "180"}},
			taxInclusive: true,
		},
		{
			name: "test clock",
			price: func(t *testing.T, lf *liveFixture) {
				publishCallsRule(t, lf, packageRule("calls_flat", 10, "0.35"))
			},
			events:    []event{{qty: "27"}, {qty: "0.5"}},
			testClock: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lf := newLiveFixture(t, "Rate Shapes "+tc.name, 0)
			ctx := lf.ctx
			tc.price(t, lf)
			if tc.taxInclusive {
				setManualTax(t, ctx, lf.settings, lf.tenantID, 18)
				ts, err := lf.settings.Get(ctx, lf.tenantID)
				if err != nil {
					t.Fatalf("get settings: %v", err)
				}
				ts.TaxInclusive = true
				if _, err := lf.settings.Upsert(ctx, ts); err != nil {
					t.Fatalf("upsert settings: %v", err)
				}
			}
			for i, ev := range tc.events {
				ts := lf.cycleStart.Add(time.Duration(i+1) * time.Minute)
				if _, err := lf.usageSvc.Ingest(ctx, lf.tenantID, usage.IngestInput{
					CustomerID: lf.customerID, MeterID: lf.meterID,
					Quantity: decimal.RequireFromString(ev.qty), Dimensions: ev.dims, Timestamp: &ts,
				}); err != nil {
					t.Fatalf("ingest: %v", err)
				}
			}
			if tc.testClock {
				clockID := postgres.NewID("hc_tclk")
				bypassExec(t, lf, `INSERT INTO test_clocks (id, tenant_id, livemode, name, frozen_time, status) VALUES ($1, $2, false, 'Rate Shapes Clock', $3, 'ready')`,
					clockID, lf.tenantID, time.Now().UTC())
				bypassExec(t, lf, `UPDATE subscriptions SET test_clock_id = $1 WHERE id = $2`, clockID, lf.subID)
			}

			previews := billing.NewPreviewService(lf.engine, customer.NewPostgresStore(lf.db), lf.subStore)
			// readAll previews the period as its close will bill it and reads
			// the live balance; the two must agree. It returns the preview's
			// usage amount.
			readAll := func(stage string) int64 {
				t.Helper()
				p, err := previews.CreatePreview(ctx, lf.tenantID, billing.CreatePreviewRequest{CustomerID: lf.customerID, SubscriptionID: lf.subID})
				if err != nil {
					t.Fatalf("%s preview: %v", stage, err)
				}
				var usageCents int64
				for _, l := range p.Lines {
					if l.LineType == "usage" {
						usageCents += l.AmountCents
					}
				}
				if _, err := previews.CreatePreview(ctx, lf.tenantID, billing.CreatePreviewRequest{
					CustomerID: lf.customerID, SubscriptionID: lf.subID,
					Period: billing.CreatePreviewPeriod{From: lf.cycleStart, To: time.Now().UTC()},
				}); err != nil {
					t.Fatalf("%s preview, explicit window: %v", stage, err)
				}
				if live := lf.live(t).UnbilledUsageCents; live != usageCents {
					t.Errorf("%s: live balance counts %d unbilled, the preview %d: one rater, one number", stage, live, usageCents)
				}
				return usageCents
			}
			readAll("open")

			if !tc.testClock {
				if _, err := lf.subSvc.SetBillingThresholds(ctx, lf.tenantID, lf.subID, subscription.BillingThresholdsInput{
					AmountGTE: 1,
				}); err != nil {
					t.Fatalf("set thresholds: %v", err)
				}
				if fired, errs := lf.engine.ScanThresholds(ctx, 50); len(errs) > 0 || fired != 1 {
					t.Fatalf("threshold scan: fired=%d errs=%v, want 1", fired, errs)
				}
				invs := lf.listInvoices(t, ctx)
				if len(invs) != 1 {
					t.Fatalf("invoices after the fire = %d, want 1", len(invs))
				}
				fireEnd := invs[0].BillingPeriodEnd.UTC()
				for i, ev := range tc.events {
					insertUsageAt(t, lf, fireEnd.Add(time.Duration(i+1)*100*time.Millisecond), ev.qty, ev.dims)
				}
				if wait := time.Until(fireEnd.Add(time.Duration(len(tc.events)+1) * 100 * time.Millisecond)); wait > 0 {
					time.Sleep(wait)
				}
				previewed := readAll("after fire")

				periodEnd := fireEnd.Add(2 * time.Second).Truncate(time.Microsecond)
				if wait := time.Until(periodEnd.Add(500 * time.Millisecond)); wait > 0 {
					time.Sleep(wait)
				}
				// usageBilled is what an invoice billed for usage: the gross
				// line amount, which a tax-inclusive tenant's preview shows.
				usageBilled := func(inv domain.Invoice) int64 {
					t.Helper()
					lines, err := lf.invStore.ListLineItems(ctx, lf.tenantID, inv.ID)
					if err != nil {
						t.Fatalf("list lines: %v", err)
					}
					var n int64
					for _, l := range lines {
						if l.LineType != domain.LineTypeUsage {
							continue
						}
						if tc.taxInclusive {
							n += l.TotalAmountCents
						} else {
							n += l.AmountCents
						}
					}
					return n
				}
				if tc.cancel {
					canceledAt := time.Now().UTC().Truncate(time.Microsecond)
					bypassExec(t, lf, `UPDATE subscriptions SET status = 'canceled', canceled_at = $1 WHERE id = $2`, canceledAt, lf.subID)
					sub, err := lf.subStore.Get(ctx, lf.tenantID, lf.subID)
					if err != nil {
						t.Fatalf("get sub: %v", err)
					}
					inv, err := lf.engine.BillFinalOnImmediateCancel(ctx, sub)
					if err != nil || inv.ID == "" {
						t.Fatalf("cancel final invoice: inv=%q err=%v", inv.ID, err)
					}
					if got := usageBilled(inv); got != previewed {
						t.Errorf("the cancel billed %d for usage, the preview said %d", got, previewed)
					}
				} else {
					bypassExec(t, lf, `UPDATE subscriptions SET current_billing_period_end = $1, next_billing_at = $1 WHERE id = $2`, periodEnd, lf.subID)
					if generated, failures := lf.engine.RunCycleForTenant(ctx, lf.tenantID, 50); len(failures) > 0 || generated != 1 {
						t.Fatalf("period close: generated=%d failures=%v, want 1", generated, failures)
					}
					var closed domain.Invoice
					for _, inv := range lf.listInvoices(t, ctx) {
						if inv.BillingReason == domain.BillingReasonSubscriptionCycle {
							closed = inv
						}
					}
					if got := usageBilled(closed); got != previewed {
						t.Errorf("the close billed %d for usage, the preview said %d", got, previewed)
					}
				}
			}
		})
	}
}

func bypassExec(t *testing.T, lf *liveFixture, query string, args ...any) {
	t.Helper()
	tx, err := lf.db.BeginTx(lf.ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(`SET LOCAL app.livemode = 'off'`); err != nil {
		t.Fatalf("set local livemode: %v", err)
	}
	if _, err := tx.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// insertUsageAt writes one usage event at an exact instant, just after the
// threshold invoice's end, as the tier-continuity fixture does.
func insertUsageAt(t *testing.T, lf *liveFixture, at time.Time, qty string, dims map[string]any) {
	t.Helper()
	props := []byte("{}")
	if dims != nil {
		var err error
		if props, err = json.Marshal(dims); err != nil {
			t.Fatalf("marshal dims: %v", err)
		}
	}
	bypassExec(t, lf, `
		INSERT INTO usage_events (id, tenant_id, customer_id, meter_id, quantity, properties, timestamp, livemode)
		VALUES ($1, $2, $3, $4, $5, $6, $7, false)
	`, postgres.NewID("hc_ue"), lf.tenantID, lf.customerID, lf.meterID, qty, string(props), at.Truncate(time.Microsecond))
}
