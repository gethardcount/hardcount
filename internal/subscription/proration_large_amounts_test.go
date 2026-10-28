package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// A mid-period change prorates base × quantity by the days left (here 15
// of 30). Amounts this large (near 9.2 × 10^18 cents) are not realistic;
// they are tested because the products used to be worked in int64 and
// wrapped. The proration is exact whenever each base × quantity fits int64
// cents, and the change is refused when one does not.

// changeSeats changes the seeded item's quantity from `from` to `to` on a
// paid, in_advance plan of `base` cents a seat at proNow, and returns the
// status, the proration invoices written and the credit notes issued.
func changeSeats(t *testing.T, base, from, to int64) (int, *invoicesMock, *fakeCNIssuer) {
	t.Helper()
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	tenantID := "t1"
	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_seats", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)
	if from != 1 {
		if _, err := svc.UpdateItem(ctx, tenantID, subID, itemID, UpdateItemInput{Quantity: &from}); err != nil {
			t.Fatalf("seed quantity %d: %v", from, err)
		}
	}
	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_seats": {ID: "plan_seats", Name: "Seat", BaseAmountCents: base, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	// The period's base invoice charged `from` seats in full.
	paid := base * from
	invoices := &invoicesMock{sourceInvoice: domain.Invoice{
		ID: "src_inv", PaymentStatus: domain.PaymentSucceeded, SubtotalCents: paid, TotalAmountCents: paid,
	}}
	cn := &fakeCNIssuer{}
	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)
	h.SetCreditNoteIssuer(cn)

	body, _ := json.Marshal(UpdateItemInput{Quantity: &to})
	rr := httptest.NewRecorder()
	h.updateItem(rr, updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body))
	return rr.Code, invoices, cn
}

func TestUpdateItem_LargeSeatIncreaseProratedExactly(t *testing.T) {
	// 1 → 9 seats of 1e18: the delta 8e18 × 15/30 = 4e18. The credit for the
	// old seat's unused half is 5e17, the charge for the new seats' 4.5e18.
	code, invoices, _ := changeSeats(t, 1_000_000_000_000_000_000, 1, 9)
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	if len(invoices.createdInvoices) != 1 {
		t.Fatalf("%d proration invoices, want 1", len(invoices.createdInvoices))
	}
	inv := invoices.createdInvoices[0]
	if inv.SubtotalCents != 4_000_000_000_000_000_000 || inv.TotalAmountCents != 4_000_000_000_000_000_000 {
		t.Errorf("subtotal/total = %d/%d, want 4000000000000000000", inv.SubtotalCents, inv.TotalAmountCents)
	}
	lines := invoices.createdLineItems[0]
	if len(lines) != 2 {
		t.Fatalf("%d lines, want the credit and the charge", len(lines))
	}
	if lines[0].AmountCents != -500_000_000_000_000_000 || lines[1].AmountCents != 4_500_000_000_000_000_000 {
		t.Errorf("credit/charge = %d/%d, want -500000000000000000/4500000000000000000", lines[0].AmountCents, lines[1].AmountCents)
	}
}

func TestUpdateItem_LargeSeatDecreaseCreditedExactly(t *testing.T) {
	// 9 → 1 seats of 1e18: 8e18 × 15/30 = 4e18 back, all on the paid base
	// invoice (9e18, no tax).
	code, invoices, cn := changeSeats(t, 1_000_000_000_000_000_000, 9, 1)
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	if len(invoices.createdInvoices) != 0 {
		t.Errorf("%d proration invoices, want none (a decrease credits)", len(invoices.createdInvoices))
	}
	if len(cn.calls) != 1 || cn.calls[0].gross != 4_000_000_000_000_000_000 {
		t.Errorf("credit notes %+v, want one of 4000000000000000000", cn.calls)
	}
}

func TestUpdateItem_SeatChangePastInt64Refused(t *testing.T) {
	// 1 → 10 or 18 seats of 1e18: the new base × quantity (1e19, 1.8e19)
	// does not fit. (1.8e19 wrapped to a small negative amount, so its
	// delta fit and an increase was credited.)
	for _, to := range []int64{10, 18} {
		code, invoices, cn := changeSeats(t, 1_000_000_000_000_000_000, 1, to)
		if code < 400 {
			t.Errorf("1 → %d seats: status %d, want an error", to, code)
		}
		if len(invoices.createdInvoices) != 0 || len(cn.calls) != 0 {
			t.Errorf("1 → %d seats: wrote %d invoices and %d credit notes, want none", to, len(invoices.createdInvoices), len(cn.calls))
		}
	}
}
