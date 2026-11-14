package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
)

// The credit for unused prepaid time on a quantity decrease and a downgrade,
// at 20% manual tax, driven over the real server and Postgres. Ten seats for
// the month [Sep 1, Oct 1) cost subtotal 10000, tax 2000, total 12000: 1200 a
// seat with tax-inclusive prices, 1000 without. The base invoice is paid, and
// the change comes on Sep 16 with 15 of 30 days left. Half of five seats'
// month is owed back: 3000 of what was paid.
//
// With tax-inclusive prices the unused amount comes from prices with tax in
// them, and it was taken as an amount before tax and grossed up a second
// time by the invoice's 12000/10000, crediting 3600.

// newTaxSeatRig is the seat rig at 20% manual tax with ten seats, the base
// invoice paid, and a plan at half the seat price for the downgrade.
func newTaxSeatRig(t *testing.T, inclusive bool) (r *seatRig, halfPlan string) {
	t.Helper()
	r = &seatRig{lagRig: newLagRigBare(t, "in_advance", 0)}
	if _, err := tenant.NewSettingsStore(r.db).Update(postgres.WithLivemode(context.Background(), false), r.tenantID, func(ts *domain.TenantSettings) error {
		ts.TaxProvider, ts.TaxRate, ts.TaxName, ts.TaxInclusive = "manual", 20, "VAT", inclusive
		return nil
	}); err != nil {
		t.Fatalf("set the tax: %v", err)
	}
	seat := int64(1000)
	if inclusive {
		seat = 1200
	}
	r.planTeam = r.newPlan("seat", "in_advance", seat)
	halfPlan = r.newPlan("seat_half", "in_advance", seat/2)
	sub := r.must(http.MethodPost, "/v1/subscriptions", ocP0, map[string]any{
		"code": "tax-seat-sub", "display_name": "Tax Seat Sub", "customer_id": r.custID, "billing_time": "calendar",
		"items": []map[string]any{{"plan_id": r.planTeam, "quantity": 10}},
	}, http.StatusCreated)
	r.subID = sub["id"].(string)
	r.itemA = r.sub().Items[0].ID
	r.baseID = r.firstInvoiceID()
	base := r.invoice(r.baseID)
	if flowInt(base, "subtotal_cents") != 10000 || flowInt(base, "tax_amount_cents") != 2000 || flowInt(base, "total_amount_cents") != 12000 {
		t.Fatalf("base invoice = subtotal %d, tax %d, total %d; want 10000, 2000, 12000",
			flowInt(base, "subtotal_cents"), flowInt(base, "tax_amount_cents"), flowInt(base, "total_amount_cents"))
	}
	r.pay(r.baseID, ocP0.Add(1))
	return r, halfPlan
}

func TestE2E_TaxInclusive_SeatDecreaseCreditsWhatWasPaid(t *testing.T) {
	r, _ := newTaxSeatRig(t, true)
	r.setSeats(5, ocUp)
	r.wantNotes("10 → 5 seats with half the month left", r.baseID, 3000)
}

func TestE2E_TaxExclusive_SeatDecreaseCreditsWhatWasPaid(t *testing.T) {
	r, _ := newTaxSeatRig(t, false)
	r.setSeats(5, ocUp)
	r.wantNotes("10 → 5 seats with half the month left", r.baseID, 3000)
}

func TestE2E_TaxInclusive_DowngradeCreditsWhatWasPaid(t *testing.T) {
	r, half := newTaxSeatRig(t, true)
	r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, ocUp,
		map[string]any{"new_plan_id": half, "immediate": true}, http.StatusOK)
	r.wantNotes("a downgrade to half the price with half the month left", r.baseID, 3000)
}

func TestE2E_TaxExclusive_DowngradeCreditsWhatWasPaid(t *testing.T) {
	r, half := newTaxSeatRig(t, false)
	r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, ocUp,
		map[string]any{"new_plan_id": half, "immediate": true}, http.StatusOK)
	r.wantNotes("a downgrade to half the price with half the month left", r.baseID, 3000)
}
