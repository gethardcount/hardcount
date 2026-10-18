package billing_test

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// M-SUB-1 (2026-10-16): the subscription page's "Upcoming invoice" must be
// the invoice the cycle close issues. Each scenario previews the sub
// mid-period, then closes the period and compares the preview's lines with
// the invoice's, line for line (type, description, quantity, amount).
//
// Before the fix the preview billed the FULL base fee for a calendar stub
// (19/31 days → $120 shown, $73.55 billed), for a mid-period add and for each
// side of a quantity change, and rated usage of a meter added mid-period over
// the whole period.

func previewFingerprint(lineType, desc string, qty decimal.Decimal, amount int64) string {
	return fmt.Sprintf("%s|%s|%s|%d", lineType, desc, qty.String(), amount)
}

func (f *parityFixture) invoiceLinesNoPeriod(t *testing.T, subID string) []string {
	t.Helper()
	full := f.invoiceLines(t, subID)
	out := make([]string, 0, len(full))
	for _, fp := range full {
		parts := strings.Split(fp, "|")
		// type|desc|qty|amount|ps|pe — drop the periods (preview lines
		// carry none) and print qty in decimal form like the preview.
		out = append(out, strings.Join(parts[:4], "|"))
	}
	sort.Strings(out)
	return out
}

type previewParityScenario struct {
	name  string
	build func(t *testing.T, f *parityFixture) domain.Subscription
	// wantTotal pins the money so a scenario can't pass by both sides
	// silently producing nothing.
	wantTotal int64
}

func previewParityScenarios() []previewParityScenario {
	day := func(n int) time.Time { return parityPS.AddDate(0, 0, n) }
	var out []previewParityScenario
	// Every cycle-close shape of the ADR-101 corpus (mid-period add,
	// quantity change, plan swap, removal, scheduled swap, same-instant
	// add+swap) — the cancel shape bills through BillOnCancel, not a close.
	totals := map[string]int64{
		"full-period": 6200, "creation-day-clamp": 2900, "mid-period-add": 3100 + 4200,
		"quantity-change": 800 + 11500, "plan-swap-immediate": 1200 + 5700,
		"scheduled-swap-at-boundary": 3100, "remove-mid-period": 3100 + 3000,
		"same-instant-add-and-swap": 3100 + 7500,
	}
	for _, sc := range intervalCorpus() {
		if sc.cancelDay > 0 {
			continue
		}
		want, ok := totals[sc.name]
		if !ok {
			panic("preview parity: no pinned total for corpus shape " + sc.name)
		}
		out = append(out, previewParityScenario{name: sc.name, build: sc.build, wantTotal: want})
	}

	out = append(out,
		previewParityScenario{
			// Calendar billing, created on the 13th: the first period is a
			// stub to the 1st, billed 19/31 of the monthly base.
			name:      "calendar-stub-first-period",
			wantTotal: 7355, // 12000 × 19/31
			build: func(t *testing.T, f *parityFixture) domain.Subscription {
				p := f.plan(t, "stub", 12000)
				start := day(12) // May 13
				sub, err := f.subs.Create(clock.WithEffectiveNow(f.ctx, start), f.tenantID, domain.Subscription{
					Code: "sub-stub", DisplayName: "sub-stub", CustomerID: f.custID,
					Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
					StartedAt:                 &start,
					CurrentBillingPeriodStart: &start,
					CurrentBillingPeriodEnd:   &parityPE,
					NextBillingAt:             &parityPE,
					Items:                     []domain.SubscriptionItem{{PlanID: p.ID, Quantity: 1}},
				})
				if err != nil {
					t.Fatalf("create stub sub: %v", err)
				}
				return sub
			},
		},
		previewParityScenario{
			// A metered item added on day 10: usage before the add belongs
			// to no plan on this sub and is not billed; usage after is.
			name:      "mid-period-add-with-usage",
			wantTotal: 3100 + 4200 + 7*10,
			build: func(t *testing.T, f *parityFixture) domain.Subscription {
				rule, err := f.pricing.CreateRatingRule(f.ctx, f.tenantID, domain.RatingRuleVersion{
					RuleKey: "pp_calls", Name: "calls", Version: 1,
					LifecycleState: domain.RatingRuleActive, Mode: domain.PricingFlat,
					Currency: "USD", FlatAmountCents: decimal.NewFromInt(10),
				})
				if err != nil {
					t.Fatalf("create rule: %v", err)
				}
				meter, err := f.pricing.CreateMeter(f.ctx, f.tenantID, domain.Meter{
					Key: "pp_calls", Name: "Calls", Unit: "calls", Aggregation: "sum", RatingRuleVersionID: rule.ID,
				})
				if err != nil {
					t.Fatalf("create meter: %v", err)
				}
				pA := f.plan(t, "mu-a", 3100)
				pB, err := f.pricing.CreatePlan(f.ctx, f.tenantID, domain.Plan{
					Code: "mu-b", Name: "mu-b", Currency: "USD",
					BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears,
					BaseAmountCents: 6200, Status: domain.PlanActive, MeterIDs: []string{meter.ID},
				})
				if err != nil {
					t.Fatalf("create metered plan: %v", err)
				}
				sub := f.activeSub(t, "sub-mu", parityPS, domain.SubscriptionItem{PlanID: pA.ID, Quantity: 1})
				if _, err := f.subs.AddItem(clock.WithEffectiveNow(f.ctx, day(10)), f.tenantID, domain.SubscriptionItem{SubscriptionID: sub.ID, PlanID: pB.ID, Quantity: 1}); err != nil {
					t.Fatalf("add item: %v", err)
				}
				us := usage.NewPostgresStore(f.db)
				for _, ev := range []struct {
					at  time.Time
					qty int64
				}{{day(2).Add(time.Hour), 5}, {day(15).Add(time.Hour), 7}} {
					if _, err := us.Ingest(f.ctx, f.tenantID, domain.UsageEvent{
						CustomerID: f.custID, MeterID: meter.ID, Quantity: decimal.NewFromInt(ev.qty), Timestamp: ev.at,
					}); err != nil {
						t.Fatalf("ingest: %v", err)
					}
				}
				return sub
			},
		},
	)
	return out
}

func TestPreview_MatchesCycleClose(t *testing.T) {
	db := testutil.SetupTestDB(t)
	for _, sc := range previewParityScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			f := newParityFixture(t, db, "pv-"+sc.name)
			built := sc.build(t, f)
			sub, err := f.subs.Get(f.ctx, f.tenantID, built.ID)
			if err != nil {
				t.Fatalf("reload sub: %v", err)
			}

			// Preview mid-period, after every change has landed.
			pv, err := f.engine(t, parityPS.AddDate(0, 0, 25)).Preview(f.ctx, sub)
			if err != nil {
				t.Fatalf("preview: %v", err)
			}
			var got []string
			for _, l := range pv.Lines {
				got = append(got, previewFingerprint(l.LineType, l.Description, l.Quantity, l.AmountCents))
			}
			sort.Strings(got)
			var pvTotal int64
			if len(pv.Totals) == 1 {
				pvTotal = pv.Totals[0].AmountCents
			}

			// Close the period.
			eng := f.engine(t, parityPE.Add(time.Nanosecond))
			if _, errs := eng.RunCycleForTenant(f.ctx, f.tenantID, 50); len(errs) > 0 {
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
			if pvTotal != sc.wantTotal {
				t.Fatalf("preview total = %d, want %d", pvTotal, sc.wantTotal)
			}
			for _, w := range pv.Warnings {
				if strings.Contains(w, "excludes mid-period proration") {
					t.Fatalf("preview still disclaims proration it now applies: %q", w)
				}
			}
		})
	}
}

// A trialing subscription's preview is the first invoice after the trial.
// Calendar billing, trial ending Oct 30: the first paid period is the stub
// [Oct 30, Nov 1), billed 2/31 of the $49 base — on the trial-end invoice
// for an in_advance plan (BillOnCreateTx), on the Nov 1 close for an
// in_arrears one (emitBaseSegmentLine). Before, the preview of a trialing
// sub showed the full $49 either way. Each case previews mid-trial, then
// produces the real invoice (the trial-end flip as the expiry sweep runs it,
// plus the close for in_arrears) and compares line for line.
//
// Mutation-verify: send trialing subs back to the flat window in
// previewCycle → both cases red (4900 shown, 316 billed).
func TestPreview_TrialEndingOn30thMatchesFirstInvoice(t *testing.T) {
	db := testutil.SetupTestDB(t)
	trialStart := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	trialEnd := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	firstEnd := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	for _, timing := range []domain.BillTiming{domain.BillInArrears, domain.BillInAdvance} {
		t.Run(string(timing), func(t *testing.T) {
			f := newParityFixture(t, db, "trial30-"+string(timing))
			p, err := f.pricing.CreatePlan(f.ctx, f.tenantID, domain.Plan{
				Code: "trial30", Name: "trial30", Currency: "USD",
				BillingInterval: domain.BillingMonthly, BaseBillTiming: timing,
				BaseAmountCents: 4900, Status: domain.PlanActive,
			})
			if err != nil {
				t.Fatalf("create plan: %v", err)
			}
			// Created the way the API creates a trial: subscription.Service.Create
			// with 29 trial days from Oct 1 stamps trial_end Oct 30 and, through
			// firstPeriodAfterTrial, the first PAID period [Oct 30, Nov 1) as
			// the current period.
			created, err := subscription.NewService(f.subs, nil).Create(clock.WithEffectiveNow(f.ctx, trialStart), f.tenantID, subscription.CreateInput{
				Code: "sub-trial30", DisplayName: "sub-trial30", CustomerID: f.custID,
				BillingTime: domain.BillingTimeCalendar, TrialDays: 29,
				Items: []subscription.CreateItemInput{{PlanID: p.ID, Quantity: 1}},
			})
			if err != nil {
				t.Fatalf("create trialing sub: %v", err)
			}
			if created.Status != domain.SubscriptionTrialing || created.TrialEndAt == nil || !created.TrialEndAt.Equal(trialEnd) ||
				created.CurrentBillingPeriodStart == nil || !created.CurrentBillingPeriodStart.Equal(trialEnd) ||
				created.CurrentBillingPeriodEnd == nil || !created.CurrentBillingPeriodEnd.Equal(firstEnd) {
				t.Fatalf("created sub status=%s trial_end=%v period=[%v, %v), want trialing, trial_end %v, period [%v, %v)",
					created.Status, created.TrialEndAt, created.CurrentBillingPeriodStart, created.CurrentBillingPeriodEnd, trialEnd, trialEnd, firstEnd)
			}
			sub, err := f.subs.Get(f.ctx, f.tenantID, created.ID)
			if err != nil {
				t.Fatalf("reload sub: %v", err)
			}

			pv, err := f.engine(t, time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)).Preview(f.ctx, sub)
			if err != nil {
				t.Fatalf("preview: %v", err)
			}
			var got []string
			for _, l := range pv.Lines {
				got = append(got, previewFingerprint(l.LineType, l.Description, l.Quantity, l.AmountCents))
			}
			sort.Strings(got)
			var pvTotal int64
			if len(pv.Totals) == 1 {
				pvTotal = pv.Totals[0].AmountCents
			}

			// The trial ends: flip + day-1 invoice in one tx, as the expiry
			// sweep (ProcessExpiredTrials) does.
			endEng := f.engine(t, trialEnd)
			if _, err := f.subs.ActivateAfterTrialWithBill(f.ctx, f.tenantID, sub.ID, trialEnd, func(tx *sql.Tx, activated domain.Subscription) error {
				_, _, err := endEng.BillOnCreateTx(f.ctx, tx, activated, trialEnd)
				return err
			}); err != nil {
				t.Fatalf("trial end: %v", err)
			}
			if timing == domain.BillInArrears {
				// The first invoice is the close of the first paid period.
				if _, errs := f.engine(t, firstEnd.Add(time.Nanosecond)).RunCycleForTenant(f.ctx, f.tenantID, 50); len(errs) > 0 {
					t.Fatalf("cycle close: %v", errs)
				}
			}
			want := f.invoiceLinesNoPeriod(t, sub.ID)
			if len(want) == 0 {
				t.Fatal("no invoice was issued — the scenario is not exercising billing")
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("preview diverges from the first invoice after the trial:\npreview:\n%s\ninvoice:\n%s",
					strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			if pvTotal != 316 { // 4900 × 2/31
				t.Fatalf("preview total = %d, want 316 (2/31 of $49)", pvTotal)
			}
		})
	}
}
