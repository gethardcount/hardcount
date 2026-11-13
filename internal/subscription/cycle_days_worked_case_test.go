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

// A trialing subscription's period starts at its trial end, after now, so
// the days to its end can exceed the period. An item change on it is refused
// (only an active subscription's items change), and the update path does not
// work out days left for it, as the add and remove paths do not.
func TestUpdateItem_TrialingSubscription_RefusedWithNoProration(t *testing.T) {
	now := time.Date(2027, 1, 10, 0, 0, 0, 0, time.UTC)
	start := time.Date(2027, 1, 20, 0, 0, 0, 0, time.UTC) // the trial ends on Jan 20
	end := time.Date(2027, 2, 20, 0, 0, 0, 0, time.UTC)
	ctx := clock.WithEffectiveNow(context.Background(), now)
	store := newMemStore()
	sub, err := store.CreateWithBill(context.Background(), "t1", domain.Subscription{
		Code: "trial", DisplayName: "Trial", CustomerID: "cus_1",
		Status: domain.SubscriptionTrialing, BillingTime: domain.BillingTimeAnniversary, BillingAnchorDay: 20,
		TrialEndAt: &start, CurrentBillingPeriodStart: &start, CurrentBillingPeriodEnd: &end,
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
	h.updateItem(rr, updateItemURL(auth.WithTenantID(ctx, "t1"), sub.ID, sub.Items[0].ID, body))
	if rr.Code < 400 {
		t.Fatalf("status %d, want a refusal: only an active subscription's items change. body=%s", rr.Code, rr.Body.String())
	}
	if len(invoices.createdInvoices) != 0 {
		t.Errorf("proration invoices = %d, want 0", len(invoices.createdInvoices))
	}
}

// The period's end decides for the handler too: Feb 28 to Mar 31 on anchor
// 31 is a full 31-day cycle, so a seat added on Mar 16 owes 15/31 of the fee,
// 4839. With any other end the period would read as cut short and count 28
// days from the 28th (5357).
func TestCycleDays_WorkedCase_ProrationOverAClampedFullCycle(t *testing.T) {
	start := time.Date(2027, 2, 28, 0, 0, 0, 0, time.UTC)
	end := time.Date(2027, 3, 31, 0, 0, 0, 0, time.UTC)
	ctx := clock.WithEffectiveNow(context.Background(), time.Date(2027, 3, 16, 0, 0, 0, 0, time.UTC))
	store := newMemStore()
	sub, err := store.CreateWithBill(context.Background(), "t1", domain.Subscription{
		Code: "clamped", DisplayName: "Clamped", CustomerID: "cus_1",
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeAnniversary, BillingAnchorDay: 31,
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
	h.updateItem(rr, updateItemURL(auth.WithTenantID(ctx, "t1"), sub.ID, sub.Items[0].ID, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if len(invoices.createdInvoices) != 1 || invoices.createdInvoices[0].SubtotalCents != 4839 {
		var got []int64
		for _, inv := range invoices.createdInvoices {
			got = append(got, inv.SubtotalCents)
		}
		t.Errorf("proration invoices %v, want one of 4839 (10000 × 15/31)", got)
	}
}
