package invoice

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestSendEmail_RefusesNonBareRecipient: the "email" field of
// POST /v1/invoices/{id}/send is the To address. It used to be checked
// only for being non-empty, so any text reached the email queue. It now
// gets the same check as a customer's email: one bare address, or 422 and
// nothing sent.
func TestSendEmail_RefusesNonBareRecipient(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	inv, err := store.Create(ctx, "t1", domain.Invoice{
		InvoiceNumber: "INV-TO-1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, CustomerID: "cus_to", Currency: "USD",
		TotalAmountCents: 10000, AmountDueCents: 10000,
	})
	if err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	newHandler := func() (*Handler, *ccCapturingSender) {
		sender := &ccCapturingSender{}
		h := &Handler{svc: NewService(store, nil, nil)}
		h.customers = staticCustomerGetter{cust: domain.Customer{ID: "cus_to", DisplayName: "To Co", Email: "ap@acme.test"}}
		h.emailSender = sender
		h.emailConfigured = true
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
		h, sender := newHandler()
		if rr := sendReq(t, h, inv.ID, body(to)); rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("email %q: status %d, want 422; body=%s", to, rr.Code, rr.Body.String())
		}
		if sender.called {
			t.Errorf("email %q: sent to %q, want nothing sent", to, sender.to)
		}
	}

	// A display-name CC override is refused the same way.
	h, sender := newHandler()
	if rr := sendReq(t, h, inv.ID, body("ap@acme.test", "Boss <boss@acme.test>")); rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("display-name CC: status %d, want 422", rr.Code)
	}
	if sender.called {
		t.Error("display-name CC: want nothing sent")
	}

	// Control: a bare address, sent to as parsed (trimmed).
	h, sender = newHandler()
	if rr := sendReq(t, h, inv.ID, body(" ap@acme.test ")); rr.Code != http.StatusOK {
		t.Fatalf("bare address: status %d body=%s", rr.Code, rr.Body.String())
	}
	if sender.to != "ap@acme.test" {
		t.Errorf("sent to %q, want %q", sender.to, "ap@acme.test")
	}
}
