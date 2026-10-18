package creditnote

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestCreditNoteSend_RefusesNonBareRecipient: the "email" field of
// POST /v1/credit-notes/{id}/send gets the same check as a customer's
// email: one bare address, or 422 and nothing sent. It used to be checked
// only for being non-empty.
func TestCreditNoteSend_RefusesNonBareRecipient(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	cn, err := store.Create(ctx, "t1", domain.CreditNote{
		InvoiceID: "inv_1", CustomerID: "cus_1", CreditNoteNumber: "CN-TO-1",
		Status: domain.CreditNoteIssued, Reason: "duplicate", SubtotalCents: 5400,
		TotalCents: 5400, Currency: "USD", RefundStatus: domain.RefundNone,
	})
	if err != nil {
		t.Fatalf("seed CN: %v", err)
	}
	newH := func() (*Handler, *cnCapturingSender) {
		sender := &cnCapturingSender{}
		h := NewHandler(NewService(store, nil, nil), HandlerDeps{
			Customers: cnStaticCustomers{cust: domain.Customer{ID: "cus_1", DisplayName: "Acme", Email: "ap@acme.test"}},
			Invoices:  cnStaticInvoices{inv: domain.Invoice{InvoiceNumber: "INV-9", Currency: "USD"}},
		})
		h.SetEmailSender(sender)
		h.SetEmailConfigured(true)
		return h, sender
	}
	body := func(to string, cc ...string) string {
		m := map[string]any{"email": to}
		if cc != nil {
			m["additional_emails"] = cc
		}
		b, _ := json.Marshal(m)
		return string(b)
	}

	for _, to := range []string{
		`"x@evil.example> NOTIFY=NEVER"@example.com`,
		`"bob"@acme.test`,
		`Bob <bob@acme.test>`,
		`<bob@acme.test>`,
		`bob@acme.test (Bob)`,
		`bob@acme.test, eve@acme.test`,
		`bob@localhost`,
	} {
		h, sender := newH()
		if rr := cnSendReq(t, h, cn.ID, body(to)); rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("email %q: status %d, want 422; body=%s", to, rr.Code, rr.Body.String())
		}
		if sender.called {
			t.Errorf("email %q: sent to %q, want nothing sent", to, sender.to)
		}
	}

	// A display-name CC override is refused the same way.
	h, sender := newH()
	if rr := cnSendReq(t, h, cn.ID, body("ap@acme.test", "Boss <boss@acme.test>")); rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("display-name CC: status %d, want 422", rr.Code)
	}
	if sender.called {
		t.Error("display-name CC: want nothing sent")
	}

	// Control: a bare address, sent to as parsed (trimmed).
	h, sender = newH()
	if rr := cnSendReq(t, h, cn.ID, body(" ap@acme.test ")); rr.Code != http.StatusOK {
		t.Fatalf("bare address: status %d body=%s", rr.Code, rr.Body.String())
	}
	if sender.to != "ap@acme.test" {
		t.Errorf("sent to %q, want %q", sender.to, "ap@acme.test")
	}
}
