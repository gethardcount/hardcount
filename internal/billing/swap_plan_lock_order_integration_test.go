package billing_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/subscription/subscriptiontest"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// C10, the cross-interval swap (D08 §4.21). The swap writes its refund credit
// note, which takes a number from the tenant's sequence row, so it must take
// its plans before that write. Otherwise three requests can wait on each
// other:
//
//   - W, a draft activation on plan hi, holds hi and is about to number its
//     day-1 invoice;
//   - a writer of plan content that takes both plans in ascending id, in one
//     statement (ADR-130 §8 step 2), holds lo and waits for hi;
//   - the swap off lo holds the sequence row and waits for lo;
//   - W waits for the sequence row.
//
// Postgres aborts one of them with a deadlock error (40P01). The plan writer
// is a test transaction: D05's meter edit took plans this way until ADR-130
// deleted it, and the order is the rule for any writer that takes several.
func TestCrossIntervalSwap_PlansLockedBeforeTheNumberRow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	customerStore := customer.NewPostgresStore(db)
	pricingStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)
	settingsStore := tenant.NewSettingsStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "Swap Plan Lock Order")
	pricingSvc := pricing.NewService(pricingStore)
	meter, err := pricingSvc.CreateMeter(ctx, tenantID, pricing.CreateMeterInput{
		EventName: "swap-lockord", Name: "swap-lockord", Unit: "tokens", Aggregation: "sum",
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	// Two monthly plans that price the meter; lo is the lower id.
	var monthly []domain.Plan
	for _, code := range []string{"swap-lockord-one", "swap-lockord-two"} {
		p, _ := createPlanWithPrices(t, db, ctx, tenantID, domain.Plan{
			Code: code, Name: code, Currency: "USD",
			BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
			BaseAmountCents: 4900, BaseBillTiming: domain.BillInAdvance,
		}, priceOn{meterID: meter.ID, shape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(1)}})
		monthly = append(monthly, p)
	}
	lo, hi := monthly[0], monthly[1]
	var first string
	if err := admin.QueryRowContext(ctx, `SELECT id FROM plans WHERE id IN ($1, $2) ORDER BY id LIMIT 1`, lo.ID, hi.ID).Scan(&first); err != nil {
		t.Fatalf("order plans: %v", err)
	}
	if first != lo.ID {
		lo, hi = hi, lo
	}
	yearly, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "swap-lockord-yearly", Name: "swap-lockord-yearly", Currency: "USD",
		BillingInterval: domain.BillingYearly, Status: domain.PlanActive,
		BaseAmountCents: 49000, BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("create yearly plan: %v", err)
	}

	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	swapAt := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)

	cnSvc := creditnote.NewService(creditnote.NewPostgresStore(db), invoiceStore, nil)
	cnSvc.SetNumberGenerator(settingsStore)
	e := newRealEngine(db, clock.NewFake(periodStart.Add(time.Hour)))
	e.SetCreditNoteAdjuster(cnSvc)
	e.SetCreditHeadroomReader(cnSvc)

	// The swapped subscription: on lo, its period paid, so the swap writes a
	// refund credit note.
	custA, err := customerStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_swap_lockord_a", DisplayName: "Swap A"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-swap-lockord", DisplayName: "Swap", CustomerID: custA.ID,
		Items:  []domain.SubscriptionItem{{PlanID: lo.ID, Quantity: 1}},
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &periodStart,
		CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)
	sub, err = subStore.Get(ctx, tenantID, sub.ID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	funding, err := e.BillOnCreate(ctx, sub)
	if err != nil {
		t.Fatalf("BillOnCreate: %v", err)
	}
	if _, err := invoiceStore.MarkPaid(ctx, tenantID, funding.ID, "pi_swap_lockord", periodStart); err != nil {
		t.Fatalf("mark paid: %v", err)
	}

	// W's subscription: a draft on hi, activated with a day-1 invoice.
	custB, err := customerStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_swap_lockord_b", DisplayName: "Swap B"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	draft, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-swap-lockord-draft", DisplayName: "Draft", CustomerID: custB.ID,
		Items:  []domain.SubscriptionItem{{PlanID: hi.ID, Quantity: 1}},
		Status: domain.SubscriptionDraft, BillingTime: domain.BillingTimeCalendar,
	})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}

	subSvc := subscription.NewService(subStore, clock.NewFake(swapAt))
	subSvc.SetPlanReader(&swapLockPlanReader{plans: map[string]domain.Plan{lo.ID: lo, hi.ID: hi, yearly.ID: yearly}})
	subSvc.SetBiller(e)
	opCtx := clock.WithEffectiveNow(ctx, swapAt)

	// 1. W activates the draft: it holds hi and stops before its invoice.
	wHolds, wGo := make(chan struct{}), make(chan struct{})
	wDone := make(chan error, 1)
	go func() {
		_, err := subStore.ActivateDraftWithBill(opCtx, tenantID, draft.ID, swapAt, swapAt, periodEnd, 1, func(tx *sql.Tx, a domain.Subscription) error {
			close(wHolds)
			<-wGo
			_, _, err := e.BillOnCreateTx(opCtx, tx, a, swapAt)
			return err
		})
		wDone <- err
	}()
	select {
	case <-wHolds:
	case err := <-wDone:
		t.Fatalf("activation finished before its invoice: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("the activation never reached its invoice")
	}

	// 2. The plan writer locks lo and waits for hi.
	editDone := make(chan error, 1)
	go func() {
		editDone <- func() error {
			tx, err := admin.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(ctx, `SELECT 1 FROM plans WHERE id = ANY($1) ORDER BY id FOR UPDATE /* plan writer */`, []string{lo.ID, hi.ID}); err != nil {
				return err
			}
			return tx.Commit()
		}()
	}()
	testutil.WaitForLockWaiter(t, ctx, admin, "/* plan writer */")

	// 3. The swap off lo waits for lo.
	swapDone := make(chan error, 1)
	go func() {
		swapDone <- func() error {
			tx, err := db.BeginTx(opCtx, postgres.TxTenant, tenantID)
			if err != nil {
				return err
			}
			defer postgres.Rollback(tx)
			if _, err := subSvc.UpdateItemTx(opCtx, tx, tenantID, sub.ID, sub.Items[0].ID, subscription.UpdateItemInput{
				NewPlanID: yearly.ID, Immediate: true,
			}); err != nil {
				return err
			}
			return tx.Commit()
		}()
	}()
	testutil.WaitForLockWaiter(t, ctx, admin, "ORDER BY id FOR KEY SHARE")

	// 4. W numbers its invoice.
	close(wGo)

	var wErr, editErr, swapErr error
	for range 3 {
		select {
		case wErr = <-wDone:
		case editErr = <-editDone:
		case swapErr = <-swapDone:
		case <-time.After(20 * time.Second):
			t.Fatal("the activation, the plan writer or the swap never finished")
		}
	}
	if postgres.IsDeadlock(wErr) || postgres.IsDeadlock(editErr) || postgres.IsDeadlock(swapErr) {
		t.Fatalf("deadlock (40P01): activation err=%v, plan writer err=%v, swap err=%v", wErr, editErr, swapErr)
	}
	if wErr != nil {
		t.Fatalf("activation: %v", wErr)
	}
	if swapErr != nil {
		t.Fatalf("swap: %v", swapErr)
	}
	if editErr != nil {
		t.Fatalf("plan writer: %v", editErr)
	}
	notes, err := cnSvc.List(ctx, creditnote.ListFilter{TenantID: tenantID, InvoiceID: funding.ID})
	if err != nil {
		t.Fatalf("list credit notes: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("refund credit notes on the old period's invoice = %d, want 1", len(notes))
	}
}
