package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// The worked case for the handler's in-advance proration (R5): the tenant
// moved from Asia/Kolkata to UTC, and the close rolled a 1-day period in UTC
// from IST's Feb 1 boundary on anchor 1. A seat added at its start owes one
// day of the seat. Before, the cycle was the period itself (1 day), so the
// change charged the whole month's fee, 10000. Now the cycle is one month
// from the 31st, clamped: 10000 × 1/28 = 357.
func TestCycleDays_WorkedCase_ProrationInAOneDaySeam(t *testing.T) {
	start := time.Date(2027, 1, 31, 18, 30, 0, 0, time.UTC)
	end := time.Date(2027, 2, 1, 18, 30, 0, 0, time.UTC)
	ctx := clock.WithEffectiveNow(context.Background(), start)
	tenantID := "t1"

	store := newMemStore()
	sub, err := store.CreateWithBill(context.Background(), tenantID, domain.Subscription{
		Code: "seam", DisplayName: "Seam", CustomerID: "cus_1",
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeAnniversary, BillingAnchorDay: 1,
		CurrentBillingPeriodStart: &start, CurrentBillingPeriodEnd: &end,
		Items: []domain.SubscriptionItem{{PlanID: "plan_seats", Quantity: 1}},
	}, nil)
	if err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_seats": {ID: "plan_seats", Name: "Seat", BaseAmountCents: 10000, Currency: "USD",
			BaseBillTiming: domain.BillInAdvance, BillingInterval: domain.BillingMonthly},
	}}
	invoices := &invoicesMock{sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded}}
	h := NewHandler(NewService(store, nil))
	h.SetProrationDeps(plans, invoices)

	qty := int64(2)
	body, _ := json.Marshal(UpdateItemInput{Quantity: &qty})
	rr := httptest.NewRecorder()
	h.updateItem(rr, updateItemURL(auth.WithTenantID(ctx, tenantID), sub.ID, sub.Items[0].ID, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if len(invoices.createdInvoices) != 1 {
		t.Fatalf("proration invoices = %d, want 1", len(invoices.createdInvoices))
	}
	if got := invoices.createdInvoices[0].SubtotalCents; got != 357 {
		t.Errorf("proration = %d, want 357 (10000 × 1/28)", got)
	}
}
