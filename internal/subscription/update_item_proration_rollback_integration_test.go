package subscription

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestUpdateItem_ProrationInvoiceCreateFailure_RollsBackQuantity is FLOW B3 item #5
// (UpdateItem half — same ADR-030 atomic-proration guarantee as the #3 add path):
// a same-interval quantity INCREASE cuts a partial-period charge proration in the
// SAME tx as the quantity write, so a proration-invoice write failure must roll the
// quantity back to its pre-change value — never leave the item upgraded with no
// charge cut. (The cross-interval-swap half of #5 is covered by
// TestUpdateItemTx_CrossIntervalSwap_RealTxRollsBackOnBillFailure.)
func TestUpdateItem_ProrationInvoiceCreateFailure_RollsBackQuantity(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "UpdateItem Proration Rollback")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_upd_rb", DisplayName: "Upd RB",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	basePlan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "upd-base-adv", Name: "Base", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance,
		BaseAmountCents: 6000, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	// Active in_advance sub, one item at quantity 1, mid-period.
	store := NewPostgresStore(db)
	created, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-upd-rb", DisplayName: "Upd RB", CustomerID: cust.ID,
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
	itemID := sub.Items[0].ID

	plans := &plansMock{plans: map[string]domain.Plan{basePlan.ID: basePlan}}
	// PAID source so the qty-increase proceeds to the proration CHARGE invoice
	// creation — the seam we fail.
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
	newQty := int64(2)
	body, _ := json.Marshal(UpdateItemInput{Quantity: &newQty, Immediate: true})
	req := updateItemURL(reqCtx, sub.ID, itemID, body)
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code >= 200 && rr.Code < 300 {
		t.Fatalf("expected updateItem to fail when the in-tx proration invoice write errors, got %d: %s", rr.Code, rr.Body.String())
	}

	// The real assertion: the quantity write on the caller's tx MUST have rolled
	// back to its pre-change value.
	after, err := store.Get(ctx, tenantID, sub.ID)
	if err != nil {
		t.Fatalf("get after rollback: %v", err)
	}
	if len(after.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(after.Items))
	}
	if after.Items[0].Quantity != 1 {
		t.Errorf("item quantity must roll back to 1 on in-tx proration-write failure, got %d", after.Items[0].Quantity)
	}
}
