package subscription

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestAddItem_ProrationInvoiceCreateFailure_RollsBackItemAdd is FLOW B3 item #3
// (the ADD path of the ADR-030 atomic-proration guarantee): atomicAddItemWithProration
// writes the item add AND the proration charge invoice in ONE tx, so a failure
// creating the proration invoice must roll the item add back — never leave the
// item on the subscription with no charge cut. Mirrors the manual flow's
// "REVOKE INSERT ON invoices" by failing the in-tx CreateInvoiceWithLineItems(Tx)
// seam; the item add runs on the caller's real tx via store.AddItemTx, so a real
// DB is required to prove the DELETE/rollback. (Sibling: #7 clawback-draft and
// the cross-interval-swap bill-fail tests prove the same pattern at other seams.)
func TestAddItem_ProrationInvoiceCreateFailure_RollsBackItemAdd(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "AddItem Proration Rollback")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_add_rb", DisplayName: "Add RB",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	ps := pricing.NewPostgresStore(db)
	basePlan, err := ps.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "base-adv", Name: "Base", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance,
		BaseAmountCents: 6000, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create base plan: %v", err)
	}
	addPlan, err := ps.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "add-adv", Name: "Add", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance,
		BaseAmountCents: 3000, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create add plan: %v", err)
	}

	// Active in_advance sub, one item, mid-period (proNow = 15/30 days remain) so
	// adding a second item cuts a partial-period CHARGE proration.
	store := NewPostgresStore(db)
	created, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-add-rb", DisplayName: "Add RB", CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &proPeriodStart,
		CurrentBillingPeriodStart: &proPeriodStart,
		CurrentBillingPeriodEnd:   &proPeriodEnd,
		NextBillingAt:             &proPeriodEnd,
		Items:                     []domain.SubscriptionItem{{PlanID: basePlan.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	sub, err := store.Get(ctx, tenantID, created.ID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	if len(sub.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(sub.Items))
	}

	plans := &plansMock{plans: map[string]domain.Plan{basePlan.ID: basePlan, addPlan.ID: addPlan}}
	// PAID source so the add proceeds past the ADR-050 unpaid-prebill gate and
	// reaches the proration CHARGE invoice creation — the seam we fail.
	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{
			ID: "src_inv", PaymentStatus: domain.PaymentSucceeded,
			SubtotalCents: 6000, TaxFacts: domain.TaxFacts{}, TotalAmountCents: 6000, CreatedAt: proPeriodStart,
		},
		createInvoiceErr: errors.New("simulated proration invoice write failure"),
	}

	h := NewHandler(svcWithStore(store))
	h.SetDB(db)
	h.SetProrationDeps(plans, invoices)

	reqCtx := clock.WithEffectiveNow(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), proNow)
	body, _ := json.Marshal(AddItemInput{PlanID: addPlan.ID, Quantity: 1})
	req := addItemURL(reqCtx, sub.ID, body)
	rr := httptest.NewRecorder()
	h.addItem(rr, req)

	// The in-tx proration invoice-write failure must surface as an error, not a success.
	if rr.Code >= 200 && rr.Code < 300 {
		t.Fatalf("expected addItem to fail when the in-tx proration invoice write errors, got %d: %s", rr.Code, rr.Body.String())
	}

	// The real assertion: the item ADD on the caller's tx MUST have rolled back.
	// A fresh read still sees ONLY the original item — no row for the added plan,
	// i.e. no item-on-the-sub-with-no-charge.
	after, err := store.Get(ctx, tenantID, sub.ID)
	if err != nil {
		t.Fatalf("get after rollback: %v", err)
	}
	if len(after.Items) != 1 {
		t.Fatalf("item add must roll back on in-tx proration-write failure: got %d items, want 1", len(after.Items))
	}
	for _, it := range after.Items {
		if it.PlanID == addPlan.ID {
			t.Errorf("added item (plan %s) must NOT exist after rollback", addPlan.ID)
		}
	}
}

// TestAddItem_EnrollAutoChargeFailure_RollsBackItemAdd guards the enrollAutoCharge
// fix: auto-charge enrollment now runs INSIDE the item-change tx, so an enrollment
// failure rolls the item add back (clean error, operator retries) instead of leaving
// a committed-but-unenrolled charge invoice to sit unpaid. The proration invoice
// itself is created fine here — only the in-tx SetAutoChargePendingTx fails.
func TestAddItem_EnrollAutoChargeFailure_RollsBackItemAdd(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "AddItem Enroll Rollback")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_enr_rb", DisplayName: "Enr RB"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	ps := pricing.NewPostgresStore(db)
	basePlan, err := ps.CreatePlan(ctx, tenantID, domain.Plan{Code: "enr-base", Name: "Base", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 6000, Status: domain.PlanActive})
	if err != nil {
		t.Fatalf("create base plan: %v", err)
	}
	addPlan, err := ps.CreatePlan(ctx, tenantID, domain.Plan{Code: "enr-add", Name: "Add", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 3000, Status: domain.PlanActive})
	if err != nil {
		t.Fatalf("create add plan: %v", err)
	}

	store := NewPostgresStore(db)
	created, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-enr-rb", DisplayName: "Enr RB", CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt: &proPeriodStart, CurrentBillingPeriodStart: &proPeriodStart,
		CurrentBillingPeriodEnd: &proPeriodEnd, NextBillingAt: &proPeriodEnd,
		Items: []domain.SubscriptionItem{{PlanID: basePlan.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	sub, err := store.Get(ctx, tenantID, created.ID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}

	plans := &plansMock{plans: map[string]domain.Plan{basePlan.ID: basePlan, addPlan.ID: addPlan}}
	// The proration invoice is created OK; the in-tx enrollment is what fails.
	invoices := &invoicesMock{
		sourceInvoice:    domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded, SubtotalCents: 6000, TotalAmountCents: 6000, CreatedAt: proPeriodStart},
		setAutoChargeErr: errors.New("simulated in-tx auto-charge enrollment failure"),
	}
	h := NewHandler(svcWithStore(store))
	h.SetDB(db)
	h.SetProrationDeps(plans, invoices)

	reqCtx := clock.WithEffectiveNow(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), proNow)
	body, _ := json.Marshal(AddItemInput{PlanID: addPlan.ID, Quantity: 1})
	rr := httptest.NewRecorder()
	h.addItem(rr, addItemURL(reqCtx, sub.ID, body))

	if rr.Code >= 200 && rr.Code < 300 {
		t.Fatalf("expected addItem to fail when in-tx enrollment errors, got %d: %s", rr.Code, rr.Body.String())
	}
	after, err := store.Get(ctx, tenantID, sub.ID)
	if err != nil {
		t.Fatalf("get after rollback: %v", err)
	}
	if len(after.Items) != 1 {
		t.Fatalf("item add must roll back on in-tx enrollment failure: got %d items, want 1", len(after.Items))
	}
}
