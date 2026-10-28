package subscription

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// TestRemoveItem_ProrationGate pins when removing an item mid-period credits
// the unused time. The credit is a credit note (CreditNoteIssuer); the gate
// in removeItem decides whether the proration runs at all:
//   - proration deps wired → it runs, and a credit note credits the removal;
//   - proration deps wired but no credit-note issuer → it runs and fails the
//     request, so a credit owed is never dropped silently;
//   - no proration deps → it does not run.
func TestRemoveItem_ProrationGate(t *testing.T) {
	const tenantID = "t1"
	ctx := clock.WithEffectiveNow(context.Background(), proNow)

	setup := func(t *testing.T) (*Handler, string, string) {
		t.Helper()
		store := newMemStore()
		subID, firstItemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_a", proPeriodStart, proPeriodEnd)
		svc := NewService(store, nil)
		if _, err := svc.AddItem(ctx, tenantID, subID, AddItemInput{PlanID: "plan_b", Quantity: 1}); err != nil {
			t.Fatalf("seed second item: %v", err)
		}
		return NewHandler(svc), subID, firstItemID
	}
	wireProration := func(h *Handler) {
		plans := &plansMock{plans: map[string]domain.Plan{
			"plan_a": {ID: "plan_a", Name: "Base A", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
			"plan_b": {ID: "plan_b", Name: "Base B", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		}}
		invoices := &invoicesMock{
			sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded, SubtotalCents: 3000, TotalAmountCents: 3000},
		}
		h.SetProrationDeps(plans, invoices)
	}
	remove := func(h *Handler, subID, itemID string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.removeItem(rr, removeItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID))
		return rr
	}

	t.Run("credit notes wired: the removal is credited", func(t *testing.T) {
		h, subID, itemID := setup(t)
		wireProration(h)
		cn := &fakeCNIssuer{}
		h.SetCreditNoteIssuer(cn)

		rr := remove(h, subID, itemID)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("status: got %d, want 204. body=%s", rr.Code, rr.Body.String())
		}
		if len(cn.calls) != 1 {
			t.Fatalf("credit notes: got %d, want 1 — the proration must run and credit the removed item", len(cn.calls))
		}
		if c := cn.calls[0]; c.reason != "subscription_item_removed" || c.invoiceID != "src_inv" || c.gross != 1000 {
			t.Errorf("credit note: got %+v, want 1000 cents (2000 × 15/30) on src_inv for subscription_item_removed", c)
		}
	})

	t.Run("credit-note issuer not wired: the removal fails instead of dropping the credit", func(t *testing.T) {
		h, subID, itemID := setup(t)
		wireProration(h)

		rr := remove(h, subID, itemID)
		if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "proration_failed") {
			t.Fatalf("got %d %s, want 500 proration_failed — the proration must run and refuse, not skip", rr.Code, rr.Body.String())
		}
	})

	t.Run("no proration deps: no proration runs", func(t *testing.T) {
		h, subID, itemID := setup(t)
		cn := &fakeCNIssuer{}
		h.SetCreditNoteIssuer(cn)

		rr := remove(h, subID, itemID)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("status: got %d, want 204. body=%s", rr.Code, rr.Body.String())
		}
		if len(cn.calls) != 0 {
			t.Errorf("credit notes: got %d, want 0 — no proration runs without its deps", len(cn.calls))
		}
	})
}
