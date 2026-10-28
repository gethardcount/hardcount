package subscription

import (
	"context"
	"errors"
	"net/http"
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

// TestRemoveItem_ClawbackDraftCreateFailure_RealTxRollsBackItemDelete is the
// real-Postgres proof of the ADR-056/ADR-057 atomicity guarantee on the
// downgrade/removal CLAWBACK path: createClawbackDraftsTx creates the
// tax-reversing credit note as a DRAFT inside the SAME tx as the item delete,
// so a draft-create failure must roll the item delete back. (The manual analog is
// MANUAL_TEST FLOW B3's "REVOKE INSERT ON credit_notes" injection; this test is the
// automated counterpart.)
//
// The sibling cross_interval_swap_integration_test.go proves the rollback when
// the in-tx new-period BILL fails (a different in-tx side effect); this is the
// closest automated analog for the clawback-draft seam, where the failure is in
// h.creditNotes.CreateAdjustmentDraftTx. The item delete runs on the caller's
// real tx via h.svc.RemoveItemTx -> store.RemoveItemTx, so only a real tx can
// prove the DELETE is undone (the in-memory store can't model tx rollback).
func TestRemoveItem_ClawbackDraftCreateFailure_RealTxRollsBackItemDelete(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clawback Draft Rollback")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_clawback_rb", DisplayName: "Clawback RB",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	// Real plan row — subscription_items has a FK to plans, so the item insert
	// needs a persisted plan even though the proration logic reads the mock below.
	ps := pricing.NewPostgresStore(db)
	realPlan, err := ps.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "pro-monthly-adv", Name: "Pro", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance,
		BaseAmountCents: 6000, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	// A distinct second plan: an active sub refuses to drop its last item AND
	// refuses duplicate plans in items, so the keeper item needs its own plan.
	keeperPlan, err := ps.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "keeper-monthly-adv", Name: "Keeper", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance,
		BaseAmountCents: 3000, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create keeper plan: %v", err)
	}

	// A 30-day in_advance period straddling proNow (15/30 days remain) so the
	// removal claws back the unused prebill — the branch that creates a clawback
	// draft in-tx. The handler reads clock.Now(ctx) for the remaining-days ratio,
	// so bind effective-now to a mid-period instant.
	store := NewPostgresStore(db)
	created, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-clawback-rb", DisplayName: "Clawback RB", CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &proPeriodStart,
		CurrentBillingPeriodStart: &proPeriodStart,
		CurrentBillingPeriodEnd:   &proPeriodEnd,
		NextBillingAt:             &proPeriodEnd,
		// Two items so RemoveItemTx is allowed (an active sub refuses to drop its
		// last item). We remove the first; the second keeps the sub valid.
		Items: []domain.SubscriptionItem{
			{PlanID: realPlan.ID, Quantity: 1},
			{PlanID: keeperPlan.ID, Quantity: 1},
		},
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	sub, err := store.Get(ctx, tenantID, created.ID)
	if err != nil {
		t.Fatalf("get created sub: %v", err)
	}
	if len(sub.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(sub.Items))
	}
	removeItemID := sub.Items[0].ID

	// Proration deps: an in_advance plan + a PAID funding invoice so the removal
	// resolves a clawback (net + tax) and reaches createClawbackDraftsTx. The
	// invoice rows are mock-resolved — the property under test is the item-delete
	// rollback, not invoice resolution.
	plans := &plansMock{plans: map[string]domain.Plan{
		realPlan.ID:   realPlan,
		keeperPlan.ID: keeperPlan,
	}}
	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{
			ID: "src_inv", PaymentStatus: domain.PaymentSucceeded,
			SubtotalCents: 6000, TaxFacts: domain.TaxFacts{TaxAmountCents: 600}, TotalAmountCents: 6600,
			CreatedAt: proPeriodStart,
		},
	}
	// fakeCNIssuer.err fails CreateAdjustmentDraftTx in-tx — the exact failure
	// the atomic remove path must roll the item delete back from. CreditedCents
	// stays 0 (full headroom) so the clawback resolves a non-empty piece first.
	cn := &fakeCNIssuer{err: errors.New("simulated in-tx clawback draft create failure")}

	h := NewHandler(svcWithStore(store))
	h.SetDB(db)
	h.SetProrationDeps(plans, invoices)
	h.SetCreditNoteIssuer(cn)

	// Mid-period instant so remainingPeriodRatio > 0 → atomic clawback path taken.
	reqCtx := clock.WithEffectiveNow(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), proNow)
	req := removeItemURL(reqCtx, sub.ID, removeItemID)
	rr := httptest.NewRecorder()
	h.removeItem(rr, req)

	// The in-tx draft-create failure must surface as an error (NOT 204) — the
	// remove did not succeed.
	if rr.Code == http.StatusNoContent || (rr.Code >= 200 && rr.Code < 300) {
		t.Fatalf("expected removeItem to fail when the in-tx clawback draft create errors, got %d: %s", rr.Code, rr.Body.String())
	}
	// Sanity: the failing seam was actually reached (the draft-create was attempted).
	if len(cn.calls) == 0 {
		t.Fatalf("CreateAdjustmentDraftTx was never called — the clawback seam was not exercised (test is vacuous)")
	}

	// The real assertion: the item DELETE on the caller's tx MUST have rolled
	// back. A fresh read from a new connection still sees BOTH items, the removed
	// one unchanged. Without atomicity the item would be gone (deleted on the tx)
	// while the customer was never credited (draft create failed) — a silent loss.
	after, err := store.Get(ctx, tenantID, sub.ID)
	if err != nil {
		t.Fatalf("get after rollback: %v", err)
	}
	if len(after.Items) != 2 {
		t.Fatalf("item delete must roll back on in-tx clawback draft failure: got %d items, want 2", len(after.Items))
	}
	var found bool
	for _, it := range after.Items {
		if it.ID == removeItemID {
			found = true
		}
	}
	if !found {
		t.Errorf("the removed item (%s) must still exist after rollback", removeItemID)
	}
}

// svcWithStore builds a Service over the real Postgres store so the handler's
// atomic RemoveItemTx runs the DELETE on the caller's tx (the rollback target).
func svcWithStore(store Store) *Service { return NewService(store, nil) }
