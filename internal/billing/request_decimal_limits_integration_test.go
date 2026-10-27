package billing_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// Request decimals are validated with one helper and their limits are
// documented. These cases send rates at the edges of those limits through the
// public routes (a price edit and an override create), store them in
// Postgres, and bill them at the real
// period close: each invoice line must come out to the cents worked out by
// hand below. The same amounts billed before the limits existed, so valid
// rates bill as they did.
func TestRequestRates_ValidEdgesBillAsBefore(t *testing.T) {
	cases := []struct {
		name string
		// rule is the body fragment of the fixture's price, edited to it;
		// override, when set, is a price override fragment for the fixture
		// customer on it.
		rule, override string
		qty            []string
		wantCents      int64
		// stored is the rate as Postgres must hold it, read back from the
		// store (the price's, or the override's when one is set).
		stored func(unit decimal.Decimal, tiers []domain.RatingTier, pkg decimal.Decimal) string
		want   string
	}{
		{
			// 2.5e14 × 1e-12 = 250 cents.
			name:      "per_unit, smallest rate",
			rule:      `"mode":"per_unit","unit_amount_cents":"0.000000000001"`,
			qty:       []string{"150000000000000", "100000000000000"},
			wantCents: 250,
			stored:    func(f decimal.Decimal, _ []domain.RatingTier, _ decimal.Decimal) string { return f.String() },
			want:      "0.000000000001",
		},
		{
			// 2 × 99999999.999999999999 = 199999999.999999999998, rounds to
			// 200000000.
			name:      "per_unit, large rate with 12 places",
			rule:      `"mode":"per_unit","unit_amount_cents":"99999999.999999999999"`,
			qty:       []string{"2"},
			wantCents: 200000000,
			stored:    func(f decimal.Decimal, _ []domain.RatingTier, _ decimal.Decimal) string { return f.String() },
			want:      "99999999.999999999999",
		},
		{
			// 1000 × 2.000000000005 + 500.5 × 0.999999999999
			// = 2000.000000005 + 500.4999999994995 = 2500.5000000044995,
			// rounds to 2501. With the 12th place dropped it would be 2500.
			name:      "graduated, 12-place tiers",
			rule:      `"mode":"graduated","tiers":[{"up_to":1000,"unit_amount_cents":"2.000000000005"},{"up_to":null,"unit_amount_cents":0.999999999999}]`,
			qty:       []string{"1000", "500.5"},
			wantCents: 2501,
			stored: func(_ decimal.Decimal, t []domain.RatingTier, _ decimal.Decimal) string {
				return t[0].UnitAmountCents.String()
			},
			want: "2.000000000005",
		},
		{
			// One unit is one whole package: 0.500000000001 rounds to 1.
			// With the 12th place dropped it would be 0.5, which rounds half
			// to even: 0.
			name:      "package amount with 12 places",
			rule:      `"mode":"package","package_size":100,"package_amount_cents":"0.500000000001"`,
			qty:       []string{"1"},
			wantCents: 1,
			stored:    func(_ decimal.Decimal, _ []domain.RatingTier, p decimal.Decimal) string { return p.String() },
			want:      "0.500000000001",
		},
		{
			// The override prices it: 1e12 × 0.000000000999 = 999 cents.
			name:      "per_unit override, 12 places",
			rule:      `"mode":"per_unit","unit_amount_cents":"5"`,
			override:  `"mode":"per_unit","unit_amount_cents":"0.000000000999"`,
			qty:       []string{"1000000000000"},
			wantCents: 999,
			stored:    func(f decimal.Decimal, _ []domain.RatingTier, _ decimal.Decimal) string { return f.String() },
			want:      "0.000000000999",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lf := newLiveFixture(t, "Request Rates "+tc.name, 0)
			ctx := lf.ctx
			h := pricing.NewHandler(lf.pricingSvc)
			send := func(routes http.Handler, method, path, body string, want int) []byte {
				t.Helper()
				req := httptest.NewRequest(method, path, strings.NewReader(body)).
					WithContext(auth.WithTenantID(ctx, lf.tenantID))
				rr := httptest.NewRecorder()
				routes.ServeHTTP(rr, req)
				if rr.Code != want {
					t.Fatalf("%s %s %s: status %d, want %d; body %s", method, path, body, rr.Code, want, rr.Body.String())
				}
				return rr.Body.Bytes()
			}

			// The fixture's price, edited through the API to the case's
			// rate, in force from before the period opened.
			send(h.PlanRoutes(), http.MethodPatch, "/"+lf.planID+"/prices/"+lf.priceID, `{`+tc.rule+`}`, http.StatusOK)
			backdateLatestVersion(t, lf.db, ctx, lf.priceID, lf.cycleStart.Add(-2*time.Hour))
			listed, err := lf.pricingSvc.ListPlanPrices(ctx, lf.tenantID, lf.planID)
			if err != nil || len(listed) != 1 {
				t.Fatalf("read price: %v (%d prices)", err, len(listed))
			}
			stored := listed[0]
			got := tc.stored(stored.UnitAmountCents, stored.Tiers, stored.PackageAmountCents)
			if tc.override != "" {
				var o domain.CustomerPriceOverride
				if err := json.Unmarshal(send(h.OverrideRoutes(), http.MethodPost, "/", `{"customer_id":"`+lf.customerID+`","price_id":"`+lf.priceID+`",`+tc.override+`}`, http.StatusCreated), &o); err != nil {
					t.Fatalf("decode override: %v", err)
				}
				bypassExec(t, lf, `UPDATE customer_price_overrides SET created_at = $1 WHERE id = $2`, lf.cycleStart.Add(-time.Hour), o.ID)
				os, err := lf.pricingSvc.ListOverrides(ctx, lf.tenantID, lf.customerID, "")
				if err != nil || len(os) != 1 {
					t.Fatalf("read override: %v (%d rows)", err, len(os))
				}
				got = tc.stored(os[0].UnitAmountCents, os[0].Tiers, os[0].PackageAmountCents)
			}
			if got != tc.want {
				t.Errorf("stored rate %s, want %s", got, tc.want)
			}

			for i, q := range tc.qty {
				ts := lf.cycleStart.Add(time.Duration(i+1) * time.Minute)
				if _, err := lf.usageSvc.Ingest(ctx, lf.tenantID, usage.IngestInput{
					CustomerID: lf.customerID, MeterID: lf.meterID, Quantity: decimal.RequireFromString(q), Timestamp: &ts,
				}); err != nil {
					t.Fatalf("ingest %s: %v", q, err)
				}
			}
			if n := lf.closeEveryPeriod(t); n != 1 {
				t.Fatalf("period close made %d invoices, want 1", n)
			}
			invs := lf.listInvoices(t, ctx)
			if len(invs) != 1 {
				t.Fatalf("%d invoices, want 1", len(invs))
			}
			lines, err := lf.invStore.ListLineItems(ctx, lf.tenantID, invs[0].ID)
			if err != nil {
				t.Fatalf("list lines: %v", err)
			}
			var usageCents int64
			for _, l := range lines {
				if l.LineType == domain.LineTypeUsage {
					usageCents += l.AmountCents
				}
			}
			if usageCents != tc.wantCents {
				t.Errorf("usage billed %d cents, want %d", usageCents, tc.wantCents)
			}
		})
	}
}

// usage_gte is stored exactly as sent at the edges of its limits, and a value
// outside them is refused with nothing written (the column is NUMERIC(38,12)
// with CHECK usage_gte > 0: before, a 13th decimal place was rounded away, and
// 0 or a value of 10^26 or more failed in Postgres).
func TestRequestUsageGTE_StoredExactlyInPostgres(t *testing.T) {
	lf := newLiveFixture(t, "Request Usage GTE", 0)
	ctx := lf.ctx
	sub, err := lf.subStore.Get(ctx, lf.tenantID, lf.subID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	itemID := sub.Items[0].ID
	set := func(raw string) error {
		_, err := lf.subSvc.SetBillingThresholds(ctx, lf.tenantID, lf.subID, subscription.BillingThresholdsInput{
			ItemThresholds: []subscription.ItemThresholdInput{{SubscriptionItemID: itemID, UsageGTE: raw}},
		})
		return err
	}
	read := func() string {
		got, err := lf.subStore.Get(ctx, lf.tenantID, lf.subID)
		if err != nil {
			t.Fatalf("get sub: %v", err)
		}
		if got.BillingThresholds == nil || len(got.BillingThresholds.ItemThresholds) != 1 {
			t.Fatalf("thresholds = %+v, want one item threshold", got.BillingThresholds)
		}
		return got.BillingThresholds.ItemThresholds[0].UsageGTE.String()
	}
	for _, v := range []string{"0.000000000001", "99999999999999999999999999.999999999999", "1000.5"} {
		if err := set(v); err != nil {
			t.Fatalf("%s refused: %v", v, err)
		}
		if got := read(); got != v {
			t.Errorf("stored %s, want %s", got, v)
		}
	}
	for _, v := range []string{"0", "0.0000000000001", "1.0000000000001", "100000000000000000000000000"} {
		if err := set(v); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("%s: err = %v, want a validation error", v, err)
		}
		if got := read(); got != "1000.5" {
			t.Errorf("after refusing %s the stored threshold is %s, want 1000.5", v, got)
		}
	}
}

// A rate inside its limits can price a line whose tax, or whose total with
// tax, is past what an int64 product holds. The manual tax provider works
// those products in 128 bits, so the tax is exact; an invoice whose total
// does not fit int64 cents fails the close with ErrAmountOverflow and no
// invoice is written. The price is edited through the route and billed at
// the real period close.
func TestRequestRates_LargeAmountsTaxedExactly(t *testing.T) {
	cases := []struct {
		name      string
		rate      string
		qty       string
		taxRate   float64
		inclusive bool
		// want subtotal, tax, total; overflow: the close fails instead.
		subtotal, tax, total int64
		overflow             bool
	}{
		{name: "exclusive 1e14 at 20%", rate: "100000000000000", qty: "1", taxRate: 20,
			subtotal: 100_000_000_000_000, tax: 20_000_000_000_000, total: 120_000_000_000_000},
		// Gross 1e14 at 20% inclusive: net 83333333333333, tax 16666666666667.
		{name: "inclusive 1e14 at 20%", rate: "100000000000000", qty: "1", taxRate: 20, inclusive: true,
			subtotal: 83_333_333_333_333, tax: 16_666_666_666_667, total: 100_000_000_000_000},
		// 5 × 999999999999999999 = 4999999999999999995; 20% = 999999999999999999.
		{name: "exclusive near 5e18 at 20%", rate: "999999999999999999", qty: "5", taxRate: 20,
			subtotal: 4_999_999_999_999_999_995, tax: 999_999_999_999_999_999, total: 5_999_999_999_999_999_994},
		// At 100% the total, 9999999999999999990, does not fit int64.
		{name: "total past int64 at 100%", rate: "999999999999999999", qty: "5", taxRate: 100, overflow: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lf := newLiveFixture(t, "Request Rates Tax "+tc.name, 0)
			ctx := lf.ctx
			setManualTax(t, ctx, lf.settings, lf.tenantID, tc.taxRate)
			if tc.inclusive {
				ts, err := lf.settings.Get(ctx, lf.tenantID)
				if err != nil {
					t.Fatalf("get settings: %v", err)
				}
				ts.TaxInclusive = true
				if _, err := lf.settings.Upsert(ctx, ts); err != nil {
					t.Fatalf("upsert settings: %v", err)
				}
			}
			h := pricing.NewHandler(lf.pricingSvc)
			req := httptest.NewRequest(http.MethodPatch, "/"+lf.planID+"/prices/"+lf.priceID, strings.NewReader(
				`{"mode":"per_unit","unit_amount_cents":"`+tc.rate+`"}`)).
				WithContext(auth.WithTenantID(ctx, lf.tenantID))
			rr := httptest.NewRecorder()
			h.PlanRoutes().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("PATCH price: status %d; body %s", rr.Code, rr.Body.String())
			}
			backdateLatestVersion(t, lf.db, ctx, lf.priceID, lf.cycleStart.Add(-2*time.Hour))

			ts := lf.cycleStart.Add(time.Minute)
			if _, err := lf.usageSvc.Ingest(ctx, lf.tenantID, usage.IngestInput{
				CustomerID: lf.customerID, MeterID: lf.meterID, Quantity: decimal.RequireFromString(tc.qty), Timestamp: &ts,
			}); err != nil {
				t.Fatalf("ingest: %v", err)
			}

			newEnd := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
			lf.exec(t, `UPDATE subscriptions SET current_billing_period_end = $1, next_billing_at = $1 WHERE tenant_id = $2`, newEnd, lf.tenantID)
			time.Sleep(time.Until(newEnd.Add(500 * time.Millisecond)))
			generated, failures := lf.engine.RunCycleForTenant(ctx, lf.tenantID, 50)
			invs := lf.listInvoices(t, ctx)

			if tc.overflow {
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
			inv := invs[0]
			if inv.Status != domain.InvoiceFinalized || inv.TaxStatus != domain.InvoiceTaxOK {
				t.Errorf("status/tax_status = %s/%s, want finalized/ok", inv.Status, inv.TaxStatus)
			}
			if inv.SubtotalCents != tc.subtotal || inv.TaxAmountCents != tc.tax || inv.TotalAmountCents != tc.total {
				t.Errorf("subtotal/tax/total = %d/%d/%d, want %d/%d/%d",
					inv.SubtotalCents, inv.TaxAmountCents, inv.TotalAmountCents, tc.subtotal, tc.tax, tc.total)
			}
		})
	}
}
