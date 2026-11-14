package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/tax"
)

// A stored invoice's tax is worked out in the terms it was priced in, not the
// tenant's setting at the time of the computation: a switch of tax-inclusive
// pricing applies to invoices priced after it. Plan A's base is 3000 at 20%:
// with tax included that is subtotal 2500, tax 500, total 3000; before tax it
// is 3000, 600, 3600.

// wantTerms is the invoice's amounts in the terms it was priced in.
func wantTerms(t *testing.T, r *lagRig, id string, inclusive bool) {
	t.Helper()
	inv := r.invoice(id)
	sub, taxAmt, total := flowInt(inv, "subtotal_cents"), flowInt(inv, "tax_amount_cents"), flowInt(inv, "total_amount_cents")
	want := [3]int{3000, 600, 3600}
	if inclusive {
		want = [3]int{2500, 500, 3000}
	}
	if got := [3]int{int(sub), int(taxAmt), int(total)}; got != want {
		t.Errorf("subtotal, tax, total = %v, want %v: the terms the invoice was priced in (tax included: %v)", got, want, inclusive)
	}
}

// An invoice whose tax was left pending is retried after the tenant switched
// the setting. The retry works out its tax in the terms it was priced in.
func TestTaxRetry_KeepsTheTermsTheInvoiceWasPricedIn(t *testing.T) {
	for _, inclusive := range []bool{true, false} {
		t.Run(fmt.Sprintf("priced with tax included=%v", inclusive), func(t *testing.T) {
			r := newTaxFlagRig(t, "in_advance", inclusive)
			resolver := &refusingTaxResolver{inner: tax.NewResolver(nil), refuse: true}
			r.srv.BillingEngine.SetTaxProviderResolver(resolver)
			r.createSub(lagP0, nil)
			id := r.onlyInvoice(domain.BillingReasonSubscriptionCreate)

			r.setTaxInclusive(!inclusive)
			resolver.refuse = false
			r.must(http.MethodPost, "/v1/invoices/"+id+"/retry-tax", lagP0.Add(time.Hour), nil, http.StatusOK)
			if got := r.invoice(id)["tax_status"]; got == string(domain.InvoiceTaxPending) {
				t.Fatalf("tax_status still pending after the retry; the retry did not run")
			}
			wantTerms(t, r, id, inclusive)
		})
	}
}

// A one-off invoice is taxed in the terms it was created in: a draft created
// before the tenant switched the setting, and finalized after, keeps them.
func TestManualInvoice_TaxedInTheTermsItWasCreatedIn(t *testing.T) {
	for _, inclusive := range []bool{true, false} {
		t.Run(fmt.Sprintf("created with tax included=%v", inclusive), func(t *testing.T) {
			r := newTaxFlagRig(t, "in_advance", inclusive)
			inv := r.must(http.MethodPost, "/v1/invoices", lagP0, map[string]any{
				"customer_id": r.custID,
				"line_items":  []map[string]any{{"description": "setup", "quantity": 1, "unit_amount_cents": 3000}},
			}, http.StatusCreated)
			id := inv["id"].(string)

			r.setTaxInclusive(!inclusive)
			r.must(http.MethodPost, "/v1/invoices/"+id+"/finalize", lagP0, nil, http.StatusOK)
			wantTerms(t, r, id, inclusive)
		})
	}
}
