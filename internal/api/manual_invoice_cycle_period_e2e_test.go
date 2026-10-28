package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// #1331: a manual invoice (POST /v1/invoices) that names the subscription and
// exactly the window its next cycle invoice will carry must not stand in for
// that cycle invoice. The billing run still bills the period, and the manual
// invoice is left as it was. A draft and a finalized manual invoice are both
// covered, under both base timings: an in-arrears close's invoice carries the
// period it closes, an in-advance close's the period it opens.
//
// Real server over HTTP, real Postgres; the billing run is POST
// /v1/billing/run.
func TestManualInvoiceWithCycleWindow_CycleStillBills(t *testing.T) {
	skipShort(t)
	for _, tc := range []struct {
		name       string
		timing     string
		start, end time.Time // the window the cycle invoice closed at lagB carries
		finalize   bool
	}{
		{"in arrears, draft manual invoice", "in_arrears", lagP0, lagB, false},
		{"in arrears, finalized manual invoice", "in_arrears", lagP0, lagB, true},
		{"in advance, draft manual invoice", "in_advance", lagB, lagP2, false},
		{"in advance, finalized manual invoice", "in_advance", lagB, lagP2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLagRig(t, tc.timing, false)
			r.ingest(lagP0.AddDate(0, 0, 5), 40)

			at := lagP0.AddDate(0, 0, 10)
			manual := r.must(http.MethodPost, "/v1/invoices", at, map[string]any{
				"customer_id":          r.custID,
				"subscription_id":      r.subID,
				"billing_period_start": tc.start.Format(time.RFC3339),
				"billing_period_end":   tc.end.Format(time.RFC3339),
				"line_items":           []map[string]any{{"description": "Onboarding", "quantity": 1, "unit_amount_cents": 500}},
			}, http.StatusCreated)
			manualID := manual["id"].(string)
			wantManualStatus := string(domain.InvoiceDraft)
			if tc.finalize {
				r.must(http.MethodPost, "/v1/invoices/"+manualID+"/finalize", at, nil, http.StatusOK)
				wantManualStatus = string(domain.InvoiceFinalized)
			}

			r.run(lagB.Add(time.Hour))

			invs := r.invoices()
			cycle := r.cycleInvoice(invs, tc.start)
			if !cycle.end.Equal(tc.end) {
				t.Errorf("cycle invoice window [%v, %v), want [%v, %v)", cycle.start, cycle.end, tc.start, tc.end)
			}
			if cycle.sum("base_fee") != 3000 || cycle.sum("usage") != 40 {
				r.logInvoices(invs)
				t.Errorf("cycle invoice base=%dc usage=%dc, want 3000c and April's 40c usage", cycle.sum("base_fee"), cycle.sum("usage"))
			}
			var manualRow *lagInvoice
			for i := range invs {
				if invs[i].id == manualID {
					manualRow = &invs[i]
				}
			}
			if manualRow == nil || manualRow.reason != string(domain.BillingReasonManual) ||
				manualRow.status != wantManualStatus || manualRow.subtotal != 500 {
				r.logInvoices(invs)
				t.Errorf("manual invoice %s = %+v, want it unchanged: manual, %s, 500c", manualID, manualRow, wantManualStatus)
			}
			if s := r.sub(); !s.CurrentBillingPeriodStart.Equal(lagB) {
				t.Errorf("period start after the run = %v, want %v", s.CurrentBillingPeriodStart, lagB)
			}
		})
	}
}
