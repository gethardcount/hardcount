package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/subscription/subscriptiontest"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// Scenario B of the ADR-132 review: the engine's cancel relief on a PAID
// prebill that credit partly paid. A 4900 prebill paid with 4000 of
// promotional credit and 900 by card; a cancel halfway through the period
// owes 2450, issued as a credit note on the paid invoice. The customer's
// balance rises by exactly 2450, whether the promotional grant is still live
// (its pro-rata share, 2450 × 4000/4900 = 2000, goes back to it) or already
// expired (all 2450 is new credit). Drawn first, the expired case gave 0: all
// 2450 went back to the expired grant and expired at once.
//
// Mutation-verify (CI, PR #1498): treat every block as spendable → "grant
// expired" puts 2000 of live credit back on the expired grant (promotional
// remaining red).
func TestCancelRelief_PaidPrebillWithCredit_GivesTheWholeRelief(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expire    bool
		wantPromo int64
	}{
		{"grant live", false, 2000},
		{"grant expired", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			ctx := postgres.WithLivemode(context.Background(), false)

			pricingStore := pricing.NewPostgresStore(db)
			subStore := subscription.NewPostgresStore(db)
			invoiceStore := invoice.NewPostgresStore(db)
			creditStore := credit.NewPostgresStore(db)
			creditSvc := credit.NewService(creditStore)
			creditNoteStore := creditnote.NewPostgresStore(db)
			settingsStore := tenant.NewSettingsStore(db)
			tenantID := testutil.CreateTestTenant(t, db, "Relief Paid Credit")

			plan, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
				Code: "pro-adv", Name: "Pro Advance", Currency: "USD",
				BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
				BaseAmountCents: 4900, BaseBillTiming: domain.BillInAdvance,
			})
			if err != nil {
				t.Fatalf("create plan: %v", err)
			}
			periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
			periodEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

			creditNoteSvc := creditnote.NewService(creditNoteStore, invoiceStore, nil, &cnGrantAdapter{creditSvc})
			creditNoteSvc.SetNumberGenerator(settingsStore)
			e := newRealEngine(db, clock.NewFake(periodStart.Add(time.Hour)))
			e.SetInvoiceVoider(invoice.NewService(invoiceStore, clock.Real(), settingsStore))
			e.SetCreditNoteAdjuster(creditNoteSvc)
			e.SetCreditHeadroomReader(creditNoteSvc)

			cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_rpc", DisplayName: "RPC"})
			if err != nil {
				t.Fatalf("create customer: %v", err)
			}
			sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
				Code: "sub-rpc", DisplayName: "RPC", CustomerID: cust.ID,
				Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				StartedAt:                 &periodStart,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
			})
			if err != nil {
				t.Fatalf("create sub: %v", err)
			}
			subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)
			sub.CurrentBillingPeriodStart = &periodStart
			sub.CurrentBillingPeriodEnd = &periodEnd

			inv, err := e.BillOnCreate(ctx, sub)
			if err != nil {
				t.Fatalf("BillOnCreate: %v", err)
			}
			// 4000 of promotional credit pays part of the prebill; the card
			// pays the other 900.
			later := time.Now().UTC().Add(time.Hour)
			promo, err := creditSvc.Grant(ctx, tenantID, credit.GrantInput{
				Currency: "USD", CustomerID: cust.ID, AmountCents: 4000, Description: "promo",
				ExpiresAt: &later, GrantKind: domain.GrantKindPromotional,
			})
			if err != nil {
				t.Fatalf("grant: %v", err)
			}
			if n, err := creditStore.ApplyToInvoiceAtomic(ctx, tenantID, cust.ID, inv.ID, "prebill", inv.AmountDueCents, time.Time{}); err != nil || n != 4000 {
				t.Fatalf("apply credit to the prebill: %d, %v; want 4000", n, err)
			}
			if _, err := invoiceStore.MarkPaid(ctx, tenantID, inv.ID, "pi_rpc", periodStart); err != nil {
				t.Fatalf("mark prebill paid: %v", err)
			}
			if tc.expire {
				tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
				if err != nil {
					t.Fatalf("begin: %v", err)
				}
				if _, err := tx.ExecContext(ctx, `UPDATE customer_credit_ledger SET expires_at = $1 WHERE id = $2`, time.Now().UTC().Add(-time.Hour), promo.ID); err != nil {
					t.Fatalf("expire grant: %v", err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatalf("commit: %v", err)
				}
			}

			cancelAt := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
			sub.Status = domain.SubscriptionCanceled
			sub.CanceledAt = &cancelAt
			const wantCredit = 2450 // 4900 × 15/30 unused

			tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
			if err != nil {
				t.Fatalf("begin tx: %v", err)
			}
			r, err := e.BillOnCancelDraftsTx(ctx, tx, sub)
			if err != nil {
				postgres.Rollback(tx)
				t.Fatalf("BillOnCancelDraftsTx: %v", err)
			}
			if r.CreditedCents != wantCredit || len(r.DraftIDs) != 1 {
				postgres.Rollback(tx)
				t.Fatalf("credited=%d ids=%v, want %d / 1 id", r.CreditedCents, r.DraftIDs, wantCredit)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit cancel: %v", err)
			}
			if issued, errs := creditNoteSvc.RetryPendingClawbackIssue(ctx, 100); len(errs) != 0 || issued != 1 {
				t.Fatalf("issue relief: issued %d, errs %v", issued, errs)
			}

			bal, err := testutil.BalanceUSD(creditSvc.GetBalance(ctx, tenantID, cust.ID))
			if err != nil {
				t.Fatalf("balance: %v", err)
			}
			if bal.BalanceCents != wantCredit {
				t.Errorf("balance %d, want %d: the relief note's whole credit", bal.BalanceCents, wantCredit)
			}
			if got := bal.TotalUsed + bal.TotalExpired + bal.TotalRetired + bal.TotalDeducted + bal.BalanceCents; got != bal.TotalGranted {
				t.Errorf("totals do not add up: %+v", bal)
			}
			resp, err := creditSvc.ListGrants(ctx, tenantID, cust.ID, true)
			if err != nil {
				t.Fatalf("list grants: %v", err)
			}
			var granted int64
			for _, g := range resp.Grants {
				granted += g.AmountCents
				if g.ID == promo.ID && g.RemainingCents != tc.wantPromo {
					t.Errorf("promotional grant remaining %d, want %d", g.RemainingCents, tc.wantPromo)
				}
			}
			if granted != bal.TotalGranted {
				t.Errorf("grant list adds to %d, total_granted %d", granted, bal.TotalGranted)
			}
		})
	}
}
