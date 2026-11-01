package invoice

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// TestAddLineItem_RefusedOnBillingDraft: POST /v1/invoices/{id}/line-items
// adds lines only to an invoice the operator made. A draft billing made for a
// subscription (a period closed while collection was paused, or while tax
// could not be worked out) is finalized with the tax billing gave it, so a
// line added to it went out untaxed: 5000 + 10% tax, plus a 2000 line,
// finalized at tax 500 instead of 700. Now the add answers 409
// invoice_not_manual and the draft is unchanged; one-off drafts still take
// lines.
//
// Mutation: drop the billing-reason check from AddLineItemAtomicAudited, and
// every billing draft takes the line (201) and its subtotal moves.
func TestAddLineItem_RefusedOnBillingDraft(t *testing.T) {
	r := newSnapRig(t, "Add Line Billing Draft")
	routes := NewHandler(r.svc, r.customers, r.settings).Routes()
	add := func(t *testing.T, id string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(AddLineItemInput{Description: "Extra seat", Quantity: 1, UnitAmountCents: 2000})
		req := httptest.NewRequest(http.MethodPost, "/"+id+"/line-items", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		ctx := context.WithValue(postgres.WithLivemode(req.Context(), false), auth.TestTenantIDKey(), r.tenantID)
		rr := httptest.NewRecorder()
		routes.ServeHTTP(rr, req.WithContext(ctx))
		return rr
	}
	draft := func(t *testing.T, num string, reason domain.InvoiceBillingReason) domain.Invoice {
		t.Helper()
		p := r.protoInvoice(num, domain.InvoiceDraft, reason)
		p.TaxRate = 10
		p.TaxAmountCents = 500
		p.TotalAmountCents = 5500
		p.AmountDueCents = 5500
		d, err := r.store.CreateWithLineItems(r.ctx, r.tenantID, p, []domain.InvoiceLineItem{{
			LineType: domain.LineTypeBaseFee, Description: "Pro - base fee", Quantity: 1, UnitAmountCents: 5000,
			AmountCents: 5000, TaxRate: 10, TaxAmountCents: 500, TotalAmountCents: 5500, Currency: "EUR",
		}})
		if err != nil {
			t.Fatalf("create %s draft: %v", reason, err)
		}
		return d
	}

	for _, reason := range []domain.InvoiceBillingReason{
		domain.BillingReasonSubscriptionCycle, domain.BillingReasonSubscriptionCreate,
		domain.BillingReasonSubscriptionUpdate, domain.BillingReasonSubscriptionCancel,
		domain.BillingReasonThreshold,
	} {
		t.Run(string(reason)+" draft: refused", func(t *testing.T) {
			d := draft(t, "ALB-"+string(reason), reason)
			rr := add(t, d.ID)
			if rr.Code != http.StatusConflict {
				t.Fatalf("status = %d, body=%s; want 409", rr.Code, rr.Body.String())
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.Error.Code != "invoice_not_manual" {
				t.Errorf("error code = %q (decode err %v), body=%s; want invoice_not_manual", body.Error.Code, err, rr.Body.String())
			}
			after, err := r.store.Get(r.ctx, r.tenantID, d.ID)
			if err != nil {
				t.Fatal(err)
			}
			lines, err := r.store.ListLineItems(r.ctx, r.tenantID, d.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(lines) != 1 || after.SubtotalCents != 5000 || after.TotalAmountCents != 5500 {
				t.Errorf("draft changed: %d lines, subtotal %d, total %d; want 1, 5000, 5500", len(lines), after.SubtotalCents, after.TotalAmountCents)
			}
		})
	}

	for _, reason := range []domain.InvoiceBillingReason{domain.BillingReasonManual, ""} {
		t.Run("one-off draft ("+string(reason)+"): accepted", func(t *testing.T) {
			d := draft(t, "ALB-OK-"+string(reason), reason)
			if rr := add(t, d.ID); rr.Code != http.StatusCreated {
				t.Fatalf("status = %d, body=%s; want 201", rr.Code, rr.Body.String())
			}
			after, err := r.store.Get(r.ctx, r.tenantID, d.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.SubtotalCents != 7000 {
				t.Errorf("subtotal = %d, want 7000", after.SubtotalCents)
			}
		})
	}
}
