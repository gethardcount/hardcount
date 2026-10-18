package invoice

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// ccCapturingSender records the cc list handed to SendInvoice.
type ccCapturingSender struct {
	called bool
	to     string
	cc     []string
}

func (c *ccCapturingSender) SendInvoice(_ context.Context, _, to string, cc []string, _, _ string, _ int64, _ string, _ []byte, _ string) error {
	c.called = true
	c.to = to
	c.cc = cc
	return nil
}

// staticCustomerGetter serves one customer with a stored CC list.
type staticCustomerGetter struct{ cust domain.Customer }

func (s staticCustomerGetter) Get(context.Context, string, string) (domain.Customer, error) {
	return s.cust, nil
}
func (s staticCustomerGetter) GetBillingProfile(context.Context, string, string) (domain.CustomerBillingProfile, error) {
	return domain.CustomerBillingProfile{}, nil
}

func sendReq(t *testing.T, h *Handler, invoiceID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/invoices/"+invoiceID+"/send", bytes.NewBufferString(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", invoiceID)
	reqCtx := context.WithValue(req.Context(), auth.TestTenantIDKey(), "t1")
	reqCtx = context.WithValue(reqCtx, chi.RouteCtxKey, rctx)
	req = req.WithContext(reqCtx)
	rr := httptest.NewRecorder()
	h.sendEmail(rr, req)
	return rr
}

// TestSendEmail_CCTriState pins the ADR-082 override contract on
// POST /v1/invoices/{id}/send:
//   - absent additional_emails → the customer's STORED list is CC'd
//     (legacy {email} bodies now CC by default — the Orb-parity change),
//   - explicit [] → primary only,
//   - explicit list → validated exact override (bad entry → 422).
func TestSendEmail_CCTriState(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	inv, err := store.Create(ctx, "t1", domain.Invoice{
		InvoiceNumber: "INV-CC-1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, CustomerID: "cus_cc", Currency: "USD",
		TotalAmountCents: 10000, AmountDueCents: 10000,
	})
	if err != nil {
		t.Fatalf("seed invoice: %v", err)
	}

	newHandler := func() (*Handler, *ccCapturingSender) {
		sender := &ccCapturingSender{}
		h := &Handler{svc: NewService(store, nil, nil)}
		h.customers = staticCustomerGetter{cust: domain.Customer{
			ID: "cus_cc", DisplayName: "CC Co", Email: "ap@acme.test",
			AdditionalEmails: []string{"finance@acme.test", "eng@acme.test"},
		}}
		h.emailSender = sender
		h.emailConfigured = true
		return h, sender
	}

	// Legacy body → stored list CC'd.
	h, sender := newHandler()
	if rr := sendReq(t, h, inv.ID, `{"email":"ap@acme.test"}`); rr.Code != http.StatusOK {
		t.Fatalf("legacy body: status %d body=%s", rr.Code, rr.Body.String())
	}
	if !reflect.DeepEqual(sender.cc, []string{"finance@acme.test", "eng@acme.test"}) {
		t.Errorf("legacy body must CC the stored list, got %v", sender.cc)
	}

	// To address equal to a stored CC entry → that entry dropped from cc.
	h, sender = newHandler()
	if rr := sendReq(t, h, inv.ID, `{"email":"finance@acme.test"}`); rr.Code != http.StatusOK {
		t.Fatalf("to==stored-cc: status %d", rr.Code)
	}
	if !reflect.DeepEqual(sender.cc, []string{"eng@acme.test"}) {
		t.Errorf("stored entry equal to To must be dropped, got %v", sender.cc)
	}

	// Explicit [] → primary only.
	h, sender = newHandler()
	if rr := sendReq(t, h, inv.ID, `{"email":"ap@acme.test","additional_emails":[]}`); rr.Code != http.StatusOK {
		t.Fatalf("explicit []: status %d", rr.Code)
	}
	if len(sender.cc) != 0 {
		t.Errorf("explicit [] must send primary-only, got cc=%v", sender.cc)
	}

	// Explicit override → exact validated list (normalized). A display-name
	// entry is refused (send_recipient_test.go).
	h, sender = newHandler()
	if rr := sendReq(t, h, inv.ID, `{"email":"ap@acme.test","additional_emails":[" Boss@Acme.Test "]}`); rr.Code != http.StatusOK {
		t.Fatalf("override: status %d", rr.Code)
	}
	if !reflect.DeepEqual(sender.cc, []string{"boss@acme.test"}) {
		t.Errorf("override must be the normalized exact list, got %v", sender.cc)
	}

	// Override entry equal to the To address → 422, nothing sent.
	h, sender = newHandler()
	if rr := sendReq(t, h, inv.ID, `{"email":"ap@acme.test","additional_emails":["ap@acme.test"]}`); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("override==to: status %d, want 422", rr.Code)
	}
	if sender.called {
		t.Error("nothing may be sent on a validation failure")
	}
}

// TestSendEmail_RefusesWhenEmailNotConfigured: the send is queued, so on a
// server with no SMTP it used to answer 200 {"status":"sent"} — the dashboard
// toasted "Invoice sent" while the queued row failed with "SMTP not
// configured". The handler now refuses with a stable code and queues nothing.
func TestSendEmail_RefusesWhenEmailNotConfigured(t *testing.T) {
	store := newMemStore()
	inv, err := store.Create(context.Background(), "t1", domain.Invoice{
		InvoiceNumber: "INV-NOSMTP", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, CustomerID: "cus_cc", Currency: "USD",
		TotalAmountCents: 10000, AmountDueCents: 10000,
	})
	if err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	sender := &ccCapturingSender{}
	h := &Handler{svc: NewService(store, nil, nil), emailSender: sender} // emailConfigured: false

	rr := sendReq(t, h, inv.ID, `{"email":"ap@acme.test"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte(`"code":"email_not_configured"`)) {
		t.Errorf("want code email_not_configured, got %s", rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("Email isn't set up on this server, so nothing was sent.")) {
		t.Errorf("want the operator message, got %s", rr.Body.String())
	}
	if sender.called {
		t.Error("an email was queued on a server that cannot send it")
	}
}
