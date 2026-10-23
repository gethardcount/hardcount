package billing_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// TestRateShapes_EveryPathComparedByTheDifferential feeds the same-build
// differential of the one rater (legacy_rater_test.go, ADR-130 PR-1) the
// pricing shapes the other fixtures do not reach on every path: package
// prices, decimal rates with fractional quantities, a multi-dimension meter
// that mixes package, graduated and the meter's own rate, a graduated
// customer override, a tax-inclusive tenant whose tiers continue across a
// threshold invoice, and a test-clock subscription. Each case runs every
// rating entry point through its public caller: the preview (close mirror
// and flat window), the live balance, the spend-threshold evaluation and
// fire, then the preview and live balance after the fire, and the period
// close or an immediate cancel. The differential compares each call and fails
// the test binary on any difference; this test checks it saw them.
func TestRateShapes_EveryPathComparedByTheDifferential(t *testing.T) {
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
				publishCallsRule(t, lf, packageRule("calls_flat", 100, 250, "1.5"))
			},
			events: []event{{qty: "130"}, {qty: "75"}, {qty: "260"}},
		},
		{
			name:   "decimal flat rate, fractional quantities",
			price:  func(t *testing.T, lf *liveFixture) { publishCallsRule(t, lf, flatRule("calls_flat", "0.0035")) },
			events: []event{{qty: "1234.5"}, {qty: "0.25"}, {qty: "999999.125"}},
			cancel: true,
		},
		{
			name: "multi-dimension: package, graduated and the meter's own rate",
			price: func(t *testing.T, lf *liveFixture) {
				pkg := createRule(t, lf, packageRule("model_a_pkg", 50, 120, "0.75"))
				grad := createRule(t, lf, gradRule("model_b_grad", []domain.RatingTier{
					{UpTo: 200, UnitAmountCents: decimal.RequireFromString("2.5")},
					{UpTo: 0, UnitAmountCents: decimal.RequireFromString("0.5")},
				}))
				seedPricingRule(t, lf.db, lf.ctx, lf.tenantID, pricing.UpsertMeterPricingRuleInput{
					MeterID: lf.meterID, RatingRuleVersionID: pkg.ID, DimensionMatch: map[string]any{"model": "a"}, AggregationMode: domain.AggSum, Priority: 10,
				})
				seedPricingRule(t, lf.db, lf.ctx, lf.tenantID, pricing.UpsertMeterPricingRuleInput{
					MeterID: lf.meterID, RatingRuleVersionID: grad.ID, DimensionMatch: map[string]any{"model": "b"}, AggregationMode: domain.AggSum, Priority: 10,
				})
			},
			events: []event{
				{qty: "70", dims: map[string]any{"model": "a"}},
				{qty: "150", dims: map[string]any{"model": "b"}},
				{qty: "33", dims: map[string]any{"model": "c"}}, // no rule: the meter's own flat 1¢
			},
		},
		{
			name: "graduated customer override",
			price: func(t *testing.T, lf *liveFixture) {
				v2 := publishCallsRule(t, lf, gradRule("calls_flat", []domain.RatingTier{
					{UpTo: 500, UnitAmountCents: decimal.NewFromInt(4)},
					{UpTo: 0, UnitAmountCents: decimal.NewFromInt(1)},
				}))
				o, err := lf.pricingSvc.CreateOverride(lf.ctx, lf.tenantID, pricing.CreateOverrideInput{
					CustomerID: lf.customerID, RatingRuleVersionID: v2.ID, Mode: domain.PricingGraduated,
					GraduatedTiers: []domain.RatingTier{
						{UpTo: 300, UnitAmountCents: decimal.RequireFromString("3.25")},
						{UpTo: 0, UnitAmountCents: decimal.RequireFromString("0.75")},
					},
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
					{UpTo: 400, UnitAmountCents: decimal.NewFromInt(7)},
					{UpTo: 0, UnitAmountCents: decimal.RequireFromString("2.5")},
				}))
			},
			events:       []event{{qty: "150"}, {qty: "180"}},
			taxInclusive: true,
		},
		{
			name: "test clock",
			price: func(t *testing.T, lf *liveFixture) {
				publishCallsRule(t, lf, packageRule("calls_flat", 10, 35, "4"))
			},
			events:    []event{{qty: "27"}, {qty: "0.5"}},
			testClock: true,
		},
	}
	entries := []string{"preview, close mirror", "preview, flat window", "live balance", "spend-threshold evaluation", "period close", "immediate cancel"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := map[string]int{}
			for _, e := range entries {
				before[e] = billing.RateDifferentialCompared(e)
			}
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
				clockID := postgres.NewID("vlx_tclk")
				bypassExec(t, lf, `INSERT INTO test_clocks (id, tenant_id, livemode, name, frozen_time, status) VALUES ($1, $2, false, 'Rate Shapes Clock', $3, 'ready')`,
					clockID, lf.tenantID, time.Now().UTC())
				bypassExec(t, lf, `UPDATE subscriptions SET test_clock_id = $1 WHERE id = $2`, clockID, lf.subID)
			}

			previews := billing.NewPreviewService(lf.engine, customer.NewPostgresStore(lf.db), lf.subStore)
			readAll := func() {
				t.Helper()
				if _, err := previews.CreatePreview(ctx, lf.tenantID, billing.CreatePreviewRequest{CustomerID: lf.customerID, SubscriptionID: lf.subID}); err != nil {
					t.Fatalf("preview: %v", err)
				}
				if _, err := previews.CreatePreview(ctx, lf.tenantID, billing.CreatePreviewRequest{
					CustomerID: lf.customerID, SubscriptionID: lf.subID,
					Period: billing.CreatePreviewPeriod{From: lf.cycleStart, To: time.Now().UTC()},
				}); err != nil {
					t.Fatalf("preview, explicit window: %v", err)
				}
				lf.liveAll(t)
			}
			readAll()

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
				readAll()

				periodEnd := fireEnd.Add(2 * time.Second).Truncate(time.Microsecond)
				if wait := time.Until(periodEnd.Add(500 * time.Millisecond)); wait > 0 {
					time.Sleep(wait)
				}
				if tc.cancel {
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
			}

			want := []string{"preview, close mirror", "preview, flat window", "live balance"}
			if !tc.testClock {
				want = append(want, "spend-threshold evaluation")
				if tc.cancel {
					want = append(want, "immediate cancel")
				} else {
					want = append(want, "period close")
				}
			}
			for _, e := range want {
				if billing.RateDifferentialCompared(e) == before[e] {
					t.Errorf("the differential compared no %s call", e)
				}
			}
		})
	}
}

func flatRule(key, cents string) pricing.CreateRatingRuleInput {
	return pricing.CreateRatingRuleInput{RuleKey: key, Name: key, Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.RequireFromString(cents)}
}

func gradRule(key string, tiers []domain.RatingTier) pricing.CreateRatingRuleInput {
	return pricing.CreateRatingRuleInput{RuleKey: key, Name: key, Mode: domain.PricingGraduated, Currency: "USD", GraduatedTiers: tiers}
}

func packageRule(key string, size, cents int64, overage string) pricing.CreateRatingRuleInput {
	return pricing.CreateRatingRuleInput{RuleKey: key, Name: key, Mode: domain.PricingPackage, Currency: "USD",
		PackageSize: size, PackageAmountCents: cents, OverageUnitAmountCents: decimal.RequireFromString(overage)}
}

func createRule(t *testing.T, lf *liveFixture, in pricing.CreateRatingRuleInput) domain.RatingRuleVersion {
	t.Helper()
	v, err := lf.pricingSvc.CreateRatingRule(lf.ctx, lf.tenantID, in)
	if err != nil {
		t.Fatalf("create rating rule %s: %v", in.RuleKey, err)
	}
	backdateRuleVersion(t, lf.thresholdFixture, lf.ctx, v.ID, lf.cycleStart.Add(-2*time.Hour))
	return v
}

// publishCallsRule publishes a new version of the fixture meter's rule key,
// in force from before the period opened (the old version is moved earlier).
func publishCallsRule(t *testing.T, lf *liveFixture, in pricing.CreateRatingRuleInput) domain.RatingRuleVersion {
	t.Helper()
	v1 := lf.seededRule(t, lf.ctx)
	backdateRuleVersion(t, lf.thresholdFixture, lf.ctx, v1.ID, lf.cycleStart.Add(-3*time.Hour))
	return createRule(t, lf, in)
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
	`, postgres.NewID("vlx_ue"), lf.tenantID, lf.customerID, lf.meterID, qty, string(props), at.Truncate(time.Microsecond))
}
