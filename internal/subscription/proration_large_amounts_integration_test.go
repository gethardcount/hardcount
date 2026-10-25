package subscription

import (
	"context"
	"encoding/json"
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

// An item change whose base × quantity does not fit int64 cents is refused
// on the path production takes: the change and its proration run in one
// transaction (SetDB), and the refusal rolls the change back. Here 18 seats
// of 1e18 cents (1.8e19) do not fit. Amounts this large are not realistic.
func TestItemChange_PastInt64RolledBack(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Item Change Past Int64")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_big", DisplayName: "Big"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	const base = 1_000_000_000_000_000_000
	ps := pricing.NewPostgresStore(db)
	seatPlan, err := ps.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "big-seat", Name: "Seat", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance,
		BaseAmountCents: base, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create seat plan: %v", err)
	}
	addPlan, err := ps.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "big-add", Name: "Add-on", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance,
		BaseAmountCents: base, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create add-on plan: %v", err)
	}

	store := NewPostgresStore(db)
	created, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-big", DisplayName: "Big", CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &proPeriodStart,
		CurrentBillingPeriodStart: &proPeriodStart,
		CurrentBillingPeriodEnd:   &proPeriodEnd,
		NextBillingAt:             &proPeriodEnd,
		Items:                     []domain.SubscriptionItem{{PlanID: seatPlan.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	sub, err := store.Get(ctx, tenantID, created.ID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	itemID := sub.Items[0].ID

	plans := &plansMock{plans: map[string]domain.Plan{seatPlan.ID: seatPlan, addPlan.ID: addPlan}}
	// The period's base invoice charged the one seat in full and is paid.
	invoices := &invoicesMock{sourceInvoice: domain.Invoice{
		ID: "src_inv", PaymentStatus: domain.PaymentSucceeded,
		SubtotalCents: base, TotalAmountCents: base, CreatedAt: proPeriodStart,
	}}
	cn := &fakeCNIssuer{}
	h := NewHandler(svcWithStore(store))
	h.SetDB(db)
	h.SetProrationDeps(plans, invoices)
	h.SetCreditNoteIssuer(cn)
	reqCtx := clock.WithEffectiveNow(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), proNow)

	// 1 → 18 seats.
	to := int64(18)
	body, _ := json.Marshal(UpdateItemInput{Quantity: &to, Immediate: true})
	rr := httptest.NewRecorder()
	h.updateItem(rr, updateItemURL(reqCtx, sub.ID, itemID, body))
	if rr.Code < 400 {
		t.Errorf("update to 18 seats: status %d, want an error: %s", rr.Code, rr.Body.String())
	}

	// A new item of 18 add-on seats.
	body, _ = json.Marshal(AddItemInput{PlanID: addPlan.ID, Quantity: 18})
	rr = httptest.NewRecorder()
	h.addItem(rr, addItemURL(reqCtx, sub.ID, body))
	if rr.Code < 400 {
		t.Errorf("add 18 add-on seats: status %d, want an error: %s", rr.Code, rr.Body.String())
	}

	after, err := store.Get(ctx, tenantID, sub.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if len(after.Items) != 1 || after.Items[0].ID != itemID || after.Items[0].Quantity != 1 {
		t.Errorf("items after = %+v, want only the one seat item at quantity 1", after.Items)
	}
	if len(invoices.createdInvoices) != 0 || len(cn.calls) != 0 {
		t.Errorf("wrote %d invoices and %d credit notes, want none", len(invoices.createdInvoices), len(cn.calls))
	}
}
