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

// seatIncreaseOnRealTx runs a quantity 1 → 2 change on the atomic path (real
// Postgres item write, faked invoice writer) against a paid source, with rec
// as the finalize recorder. It returns the HTTP status and the item's
// quantity read back afterwards.
func seatIncreaseOnRealTx(t *testing.T, rec *finalizeRecorderMock) (int, int64, *invoicesMock) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Proration Finalize Audit")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_pfa", DisplayName: "PFA"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "pfa-base", Name: "Base", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance,
		BaseAmountCents: 6000, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	store := NewPostgresStore(db)
	created, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-pfa", DisplayName: "PFA", CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt: &proPeriodStart, CurrentBillingPeriodStart: &proPeriodStart,
		CurrentBillingPeriodEnd: &proPeriodEnd, NextBillingAt: &proPeriodEnd,
		Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	sub, err := store.Get(ctx, tenantID, created.ID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}

	invoices := &invoicesMock{sourceInvoice: domain.Invoice{
		ID: "src_inv", PaymentStatus: domain.PaymentSucceeded,
		SubtotalCents: 6000, TotalAmountCents: 6000, CreatedAt: proPeriodStart,
	}}
	h := NewHandler(svcWithStore(store))
	h.SetDB(db)
	h.SetProrationDeps(&plansMock{plans: map[string]domain.Plan{plan.ID: plan}}, invoices)
	h.SetInvoiceFinalizeRecorder(rec)

	reqCtx := clock.WithEffectiveNow(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), proNow)
	qty := int64(2)
	body, _ := json.Marshal(UpdateItemInput{Quantity: &qty, Immediate: true})
	rr := httptest.NewRecorder()
	h.updateItem(rr, updateItemURL(reqCtx, sub.ID, sub.Items[0].ID, body))

	after, err := store.Get(ctx, tenantID, sub.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	return rr.Code, after.Items[0].Quantity, invoices
}

// TestUpdateItem_ProrationFinalizeAudit_RidesTheItemChangeTx (#1279): on the
// atomic path the finalize audit row is written on the item-change tx (the
// recorder gets that tx), and invoice.finalized goes out once after commit.
func TestUpdateItem_ProrationFinalizeAudit_RidesTheItemChangeTx(t *testing.T) {
	rec := &finalizeRecorderMock{}
	code, qty, invoices := seatIncreaseOnRealTx(t, rec)
	if code != 200 || qty != 2 {
		t.Fatalf("seat increase: status %d, quantity %d; want 200 and 2", code, qty)
	}
	if len(invoices.createdInvoices) != 1 {
		t.Fatalf("created %d proration invoices, want 1", len(invoices.createdInvoices))
	}
	id := invoices.createdInvoices[0].ID
	if len(rec.audits) != 1 || rec.audits[0].ID != id || rec.auditTxs[0] == nil {
		t.Errorf("finalize audit: %d rows (txs %v), want one for %s on the item-change tx", len(rec.audits), rec.auditTxs, id)
	}
	if len(rec.dispatched) != 1 || rec.dispatched[0].ID != id {
		t.Errorf("invoice.finalized dispatches: %v, want one for %s", rec.dispatched, id)
	}
	if got := rec.calls; len(got) != 2 || got[0] != "audit" || got[1] != "dispatch" {
		t.Errorf("call order %v, want [audit dispatch]", got)
	}
}

// TestUpdateItem_ProrationFinalizeAuditFailure_RollsBackQuantity: the row
// shares the item change's fate (ADR-090). A failed audit write rolls the
// quantity back on the real tx and sends no webhook.
func TestUpdateItem_ProrationFinalizeAuditFailure_RollsBackQuantity(t *testing.T) {
	rec := &finalizeRecorderMock{auditErr: errors.New("audit insert failed")}
	code, qty, _ := seatIncreaseOnRealTx(t, rec)
	if code >= 200 && code < 300 {
		t.Fatalf("seat increase with a failing audit write answered %d, want an error", code)
	}
	if qty != 1 {
		t.Errorf("quantity after a failed finalize audit = %d, want 1 (the change rolls back)", qty)
	}
	if len(rec.dispatched) != 0 {
		t.Errorf("invoice.finalized sent for a rolled-back change: %v", rec.dispatched)
	}
}
