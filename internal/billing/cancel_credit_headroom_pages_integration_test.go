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

// A cancel credits the unused time to the invoice that charged it, up to
// what that invoice can still be credited. The room left is the invoice
// total less every credit note on it, and it must count every note: the
// count used to stop at the first page of 50 (the newest), so an invoice
// whose oldest note was large read as having room it did not have. The
// credit note then went over the invoice total, its cap refused it, and the
// cancel failed on every retry.
//
// Paid prebill of 49.00. Its oldest note credits 40.00, then 100 notes of
// 0.01: 41.00 credited, 8.00 left. A page of 100 notes holds only the small
// ones. The cancel owes 24.50 of unused time; 8.00 fits and the rest is not
// owed.
func TestCancelCredit_HeadroomCountsEveryCreditNote(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	pricingStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)
	creditSvc := credit.NewService(credit.NewPostgresStore(db))
	settingsStore := tenant.NewSettingsStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "Cancel Credit Headroom Pages")
	plan, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "pro-pages", Name: "Pro Pages", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
		BaseAmountCents: 4900, BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	creditNoteSvc := creditnote.NewService(creditnote.NewPostgresStore(db), invoiceStore, nil, &cnGrantAdapter{creditSvc})
	creditNoteSvc.SetNumberGenerator(settingsStore)
	e := newRealEngine(db, clock.NewFake(periodStart.Add(time.Hour)))
	e.SetInvoiceVoider(invoice.NewService(invoiceStore, clock.Real(), settingsStore))
	e.SetCreditNoteAdjuster(creditNoteSvc)
	e.SetCreditHeadroomReader(creditNoteSvc)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_pages", DisplayName: "Pages"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-pages", DisplayName: "Pages", CustomerID: cust.ID,
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
	if _, err := invoiceStore.MarkPaid(ctx, tenantID, inv.ID, "pi_pages", periodStart); err != nil {
		t.Fatalf("mark prebill paid: %v", err)
	}

	note := func(cents int64) {
		t.Helper()
		if _, err := creditNoteSvc.Create(ctx, tenantID, creditnote.CreateInput{
			InvoiceID: inv.ID, Reason: "goodwill",
			Lines: []creditnote.CreditLineInput{{Description: "goodwill", Quantity: 1, UnitAmountCents: cents}},
		}); err != nil {
			t.Fatalf("credit note of %d: %v", cents, err)
		}
	}
	note(4000)
	for range 100 {
		note(1)
	}

	const room = 4900 - 4000 - 100
	const owed = 2450 // 15 of 30 days unused

	cancelAt := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	sub.Status = domain.SubscriptionCanceled
	sub.CanceledAt = &cancelAt

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	r, err := e.BillOnCancelDraftsTx(ctx, tx, sub)
	if err != nil {
		t.Fatalf("cancel credit: %v", err)
	}
	if len(r.DraftIDs) != 1 || r.CreditedCents != room || r.UncreditedCents != owed-room {
		t.Fatalf("drafts %v credited %d uncredited %d, want 1 draft crediting %d with %d not owed",
			r.DraftIDs, r.CreditedCents, r.UncreditedCents, room, owed-room)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit cancel: %v", err)
	}
	credited, err := creditNoteSvc.CreditedCents(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("CreditedCents: %v", err)
	}
	if credited != inv.TotalAmountCents {
		t.Errorf("credited %d of the invoice's %d after the cancel, want all of it and no more", credited, inv.TotalAmountCents)
	}
}
