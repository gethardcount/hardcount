package billing_test

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// A5 (2026-10-21 sweep): a plan change scheduled for the period end applies
// at the start of that period's close (applyDueScheduledPlanChanges), so the
// close bills the upcoming in_advance base on the NEW plan. The preview read
// the items as they are now and showed the old plan's price.
//
// Each scenario previews mid-period, closes the period, and compares the
// preview with the issued invoice: every line (type, description, quantity,
// amount) and the header period.
func TestPreview_ScheduledPlanChangeMatchesCycleClose(t *testing.T) {
	db := testutil.SetupTestDB(t)

	inAdvance := func(t *testing.T, f *parityFixture, code string, cents int64, interval domain.BillingInterval, meters ...string) domain.Plan {
		t.Helper()
		p, err := f.pricing.CreatePlan(f.ctx, f.tenantID, domain.Plan{
			Code: code, Name: code, Currency: "USD",
			BillingInterval: interval, BaseBillTiming: domain.BillInAdvance,
			BaseAmountCents: cents, Status: domain.PlanActive, MeterIDs: meters,
		})
		if err != nil {
			t.Fatalf("create plan %s: %v", code, err)
		}
		return p
	}

	type scenario struct {
		name      string
		build     func(t *testing.T, f *parityFixture) (sub domain.Subscription, previewAt, closeAt time.Time)
		wantTotal int64
	}
	scenarios := []scenario{
		{
			// Monthly $99 → $29 at the period end. The upcoming base bills
			// $29; usage of the elapsed period stays on the old plan's meter.
			name:      "in-advance-downgrade-same-interval",
			wantTotal: 2900 + 5*10,
			build: func(t *testing.T, f *parityFixture) (domain.Subscription, time.Time, time.Time) {
				rule, err := f.pricing.CreateRatingRule(f.ctx, f.tenantID, domain.RatingRuleVersion{
					RuleKey: "a5_calls", Name: "calls", Version: 1,
					LifecycleState: domain.RatingRuleActive, Mode: domain.PricingFlat,
					Currency: "USD", FlatAmountCents: decimal.NewFromInt(10),
				})
				if err != nil {
					t.Fatalf("create rule: %v", err)
				}
				meter, err := f.pricing.CreateMeter(f.ctx, f.tenantID, domain.Meter{
					Key: "a5_calls", Name: "Calls", Unit: "calls", Aggregation: "sum", RatingRuleVersionID: rule.ID,
				})
				if err != nil {
					t.Fatalf("create meter: %v", err)
				}
				pro := inAdvance(t, f, "a5-pro", 9900, domain.BillingMonthly, meter.ID)
				basic := inAdvance(t, f, "a5-basic", 2900, domain.BillingMonthly)
				sub := f.activeSub(t, "sub-a5-dg", parityPS, domain.SubscriptionItem{PlanID: pro.ID, Quantity: 1})
				if _, err := f.subs.SetItemPendingPlan(f.ctx, f.tenantID, sub.Items[0].ID, basic.ID, parityPE); err != nil {
					t.Fatalf("schedule change: %v", err)
				}
				if _, err := usage.NewPostgresStore(f.db).Ingest(f.ctx, f.tenantID, domain.UsageEvent{
					CustomerID: f.custID, MeterID: meter.ID, Quantity: decimal.NewFromInt(5), Timestamp: parityPS.AddDate(0, 0, 9),
				}); err != nil {
					t.Fatalf("ingest: %v", err)
				}
				return sub, parityPS.AddDate(0, 0, 25), parityPE.Add(time.Nanosecond)
			},
		},
		{
			// Calendar billing, yearly $1200 from May 13 → monthly $31 at the
			// renewal. The close opens the calendar stub [May 13, Jun 1) and
			// bills 19/31 of the monthly base; the header is that stub.
			name:      "in-advance-yearly-to-monthly-at-renewal",
			wantTotal: 1900,
			build: func(t *testing.T, f *parityFixture) (domain.Subscription, time.Time, time.Time) {
				yearly := inAdvance(t, f, "a5-yearly", 120000, domain.BillingYearly)
				monthly := inAdvance(t, f, "a5-monthly", 3100, domain.BillingMonthly)
				ps := time.Date(2025, 5, 13, 0, 0, 0, 0, time.UTC)
				pe := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)
				sub, err := f.subs.Create(clock.WithEffectiveNow(f.ctx, ps), f.tenantID, domain.Subscription{
					Code: "sub-a5-ym", DisplayName: "sub-a5-ym", CustomerID: f.custID,
					Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar, BillingAnchorDay: 13,
					StartedAt: &ps, CurrentBillingPeriodStart: &ps, CurrentBillingPeriodEnd: &pe, NextBillingAt: &pe,
					Items: []domain.SubscriptionItem{{PlanID: yearly.ID, Quantity: 1}},
				})
				if err != nil {
					t.Fatalf("create sub: %v", err)
				}
				if _, err := f.subs.SetItemPendingPlan(f.ctx, f.tenantID, sub.Items[0].ID, monthly.ID, pe); err != nil {
					t.Fatalf("schedule change: %v", err)
				}
				return sub, time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC), pe.Add(time.Nanosecond)
			},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			f := newParityFixture(t, db, "a5-"+sc.name)
			built, previewAt, closeAt := sc.build(t, f)
			sub, err := f.subs.Get(f.ctx, f.tenantID, built.ID)
			if err != nil {
				t.Fatalf("reload sub: %v", err)
			}

			pv, err := f.engine(t, previewAt).Preview(f.ctx, sub)
			if err != nil {
				t.Fatalf("preview: %v", err)
			}
			var got []string
			for _, l := range pv.Lines {
				got = append(got, previewFingerprint(l.LineType, l.Description, l.Quantity, l.AmountCents))
			}
			sort.Strings(got)

			if _, errs := f.engine(t, closeAt).RunCycleForTenant(f.ctx, f.tenantID, 50); len(errs) > 0 {
				t.Fatalf("cycle close: %v", errs)
			}
			want := f.invoiceLinesNoPeriod(t, sub.ID)
			if len(want) == 0 {
				t.Fatal("cycle close billed nothing — scenario is not exercising billing")
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("preview diverges from the invoice the close issued:\npreview:\n%s\ninvoice:\n%s",
					strings.Join(got, "\n"), strings.Join(want, "\n"))
			}

			invs, _, err := f.invoices.List(f.ctx, invoice.ListFilter{TenantID: f.tenantID})
			if err != nil || len(invs) != 1 {
				t.Fatalf("list invoices: %d invoices, err %v; want one cycle invoice", len(invs), err)
			}
			inv := invs[0]
			if !pv.BillingPeriodStart.Equal(inv.BillingPeriodStart) || !pv.BillingPeriodEnd.Equal(inv.BillingPeriodEnd) {
				t.Fatalf("preview period [%v, %v), invoice period [%v, %v)",
					pv.BillingPeriodStart, pv.BillingPeriodEnd, inv.BillingPeriodStart, inv.BillingPeriodEnd)
			}
			if len(pv.Totals) != 1 || pv.Totals[0].AmountCents != sc.wantTotal || inv.SubtotalCents != sc.wantTotal {
				t.Fatalf("preview totals %+v, invoice subtotal %d, want %d", pv.Totals, inv.SubtotalCents, sc.wantTotal)
			}
		})
	}
}

// A threshold fire bills the current period so far, on the plan the item
// holds now: a change scheduled for the period end is not due yet. Guards
// the other side of A5 — the scheduled plan applies only in the estimate of
// the close (previewCycle), never in the flat window the fire persists.
func TestThresholdScan_IgnoresPlanChangeScheduledForPeriodEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold Pending Plan")
	ctx := postgres.WithLivemode(context.Background(), false)

	next, err := f.pricingSvc.CreatePlan(ctx, f.tenantID, pricing.CreatePlanInput{
		Code: "pln_thresh_next", Name: "Next Plan", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseAmountCents: 5000,
	})
	if err != nil {
		t.Fatalf("create next plan: %v", err)
	}
	if _, err := f.subStore.SetItemPendingPlan(ctx, f.tenantID, f.itemID, next.ID, f.cycleEnd); err != nil {
		t.Fatalf("schedule change: %v", err)
	}
	f.ingestUsage(t, ctx, 100, 10) // 1000c on the current plan's meter
	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{AmountGTE: 500}); err != nil {
		t.Fatalf("set threshold: %v", err)
	}
	if fired, errs := f.engine.ScanThresholds(ctx, 50); len(errs) > 0 || fired != 1 {
		t.Fatalf("scan: fired %d, errs %v; want one fire", fired, errs)
	}
	invs := f.listInvoices(t, ctx)
	if len(invs) != 1 {
		t.Fatalf("%d invoices, want one threshold invoice", len(invs))
	}
	if invs[0].SubtotalCents != 1000 {
		t.Fatalf("threshold invoice subtotal %d, want 1000 (the current plan's usage, no base of the scheduled plan)", invs[0].SubtotalCents)
	}
}
