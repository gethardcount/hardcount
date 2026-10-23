package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// TestUpdateItem_Upgrade_EnrollsProrationInvoiceForAutoCharge locks the fix for
// the proration-invoice collection gap: an immediate UPGRADE produces a charge
// invoice that must be enrolled into the auto-charge sweep (auto_charge_pending),
// so it actually collects — wall-clock cron for live subs, test-clock catchup on
// advance for clock-pinned subs. Before this, proration invoices finalized but
// were never charged (no creation-site enrollment), unlike engine cycle/create
// invoices.
func TestUpdateItem_Upgrade_EnrollsProrationInvoiceForAutoCharge(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_basic", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_basic": {ID: "plan_basic", Name: "Basic", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_pro":   {ID: "plan_pro", Name: "Pro", BaseAmountCents: 6000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	// PAID source so the upgrade proceeds (ADR-050) and a charge invoice is cut.
	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{
			ID: "src_inv", PaymentStatus: domain.PaymentSucceeded,
			SubtotalCents: 2000, TotalAmountCents: 2000, AmountDueCents: 0, AmountPaidCents: 2000,
		},
	}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_pro", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if len(invoices.createdInvoices) != 1 {
		t.Fatalf("expected 1 proration charge invoice, got %d", len(invoices.createdInvoices))
	}
	createdID := invoices.createdInvoices[0].ID
	if len(invoices.autoChargeEnrolled) != 1 || invoices.autoChargeEnrolled[0] != createdID {
		t.Fatalf("auto-charge enrollment: got %v, want [%s] (the proration charge invoice must be enrolled for the sweep)",
			invoices.autoChargeEnrolled, createdID)
	}
}

// TestUpdateItem_Downgrade_DoesNotEnrollForAutoCharge proves the credit path is
// untouched: a DOWNGRADE issues a credit/credit-note, not a charge invoice, so
// nothing is enrolled for auto-charge.
func TestUpdateItem_Downgrade_DoesNotEnrollForAutoCharge(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_pro", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_pro":   {ID: "plan_pro", Name: "Pro", BaseAmountCents: 6000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_basic": {ID: "plan_basic", Name: "Basic", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{
			ID: "src_inv", PaymentStatus: domain.PaymentSucceeded,
			SubtotalCents: 6000, TotalAmountCents: 6000, AmountDueCents: 0, AmountPaidCents: 6000,
		},
	}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)
	h.SetCreditNoteIssuer(&fakeCNIssuer{})

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_basic", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if len(invoices.createdInvoices) != 0 {
		t.Errorf("downgrade must not create a charge invoice, got %d", len(invoices.createdInvoices))
	}
	if len(invoices.autoChargeEnrolled) != 0 {
		t.Errorf("downgrade must not enroll anything for auto-charge, got %v", invoices.autoChargeEnrolled)
	}
}
