package creditnote

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// cnCapturingSender records the SendCreditNote call.
type cnCapturingSender struct {
	called   bool
	to       string
	cc       []string
	cnNumber string
	invoice  string
	amount   int64
	pdf      []byte
}

func (c *cnCapturingSender) SendCreditNote(_ context.Context, _, to string, cc []string, _, cnNumber, invoiceNumber string, amountCents int64, _ string, pdf []byte) error {
	c.called = true
	c.to, c.cc, c.cnNumber, c.invoice, c.amount, c.pdf = to, cc, cnNumber, invoiceNumber, amountCents, pdf
	return nil
}

type cnStaticCustomers struct{ cust domain.Customer }

func (s cnStaticCustomers) Get(context.Context, string, string) (domain.Customer, error) {
	return s.cust, nil
}
func (s cnStaticCustomers) GetBillingProfile(context.Context, string, string) (domain.CustomerBillingProfile, error) {
	return domain.CustomerBillingProfile{}, nil
}

type cnStaticInvoices struct{ inv domain.Invoice }

func (s cnStaticInvoices) Get(context.Context, string, string) (domain.Invoice, error) {
	return s.inv, nil
}

func cnSendReq(t *testing.T, h *Handler, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/credit-notes/"+id+"/send", bytes.NewBufferString(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	reqCtx := context.WithValue(req.Context(), auth.TestTenantIDKey(), "t1")
	reqCtx = context.WithValue(reqCtx, chi.RouteCtxKey, rctx)
	req = req.WithContext(reqCtx)
	rr := httptest.NewRecorder()
	h.sendEmail(rr, req)
	return rr
}

// TestCreditNoteSendEmail (ADR-082 rider): issued-only guard, PDF
// attached, applied-invoice number + amount threaded, stored-CC
// default, and the no-sender 409. CNs previously had NO send surface —
// engine-issued clawback CNs moved money with no document reaching the
// customer.
func TestCreditNoteSendEmail(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()

	mkCN := func(num string, status domain.CreditNoteStatus) domain.CreditNote {
		cn, err := store.Create(ctx, "t1", domain.CreditNote{
			InvoiceID: "inv_1", CustomerID: "cus_1", CreditNoteNumber: num,
			Status: status, Reason: "duplicate", SubtotalCents: 5400,
			TotalCents: 5400, Currency: "USD", RefundStatus: domain.RefundNone,
		})
		if err != nil {
			t.Fatalf("seed CN: %v", err)
		}
		return cn
	}

	newH := func() (*Handler, *cnCapturingSender) {
		sender := &cnCapturingSender{}
		h := NewHandler(NewService(store, nil, nil), HandlerDeps{
			Customers: cnStaticCustomers{cust: domain.Customer{
				ID: "cus_1", DisplayName: "Acme", Email: "ap@acme.test",
				AdditionalEmails: []string{"finance@acme.test"},
			}},
			Invoices: cnStaticInvoices{inv: domain.Invoice{InvoiceNumber: "INV-9", Currency: "USD"}},
		})
		h.SetEmailSender(sender)
		h.SetEmailConfigured(true)
		return h, sender
	}

	issued := mkCN("CN-1", domain.CreditNoteIssued)

	// Golden: issued CN sends with PDF + stored CC + applied invoice.
	h, sender := newH()
	if rr := cnSendReq(t, h, issued.ID, `{"email":"ap@acme.test"}`); rr.Code != http.StatusOK {
		t.Fatalf("issued send: status %d body=%s", rr.Code, rr.Body.String())
	}
	if !sender.called || sender.cnNumber != "CN-1" || sender.invoice != "INV-9" || sender.amount != 5400 {
		t.Errorf("send args: %+v", sender)
	}
	if len(sender.pdf) == 0 {
		t.Error("credit-note email must attach the rendered PDF")
	}
	if !reflect.DeepEqual(sender.cc, []string{"finance@acme.test"}) {
		t.Errorf("stored CC default: got %v", sender.cc)
	}

	// Override [] → primary only.
	h, sender = newH()
	if rr := cnSendReq(t, h, issued.ID, `{"email":"ap@acme.test","additional_emails":[]}`); rr.Code != http.StatusOK {
		t.Fatalf("override []: status %d", rr.Code)
	}
	if len(sender.cc) != 0 {
		t.Errorf("explicit [] must clear cc, got %v", sender.cc)
	}

	// Draft → 422 naming the fix; voided → 422; nothing sent.
	draft := mkCN("CN-2", domain.CreditNoteDraft)
	h, sender = newH()
	if rr := cnSendReq(t, h, draft.ID, `{"email":"ap@acme.test"}`); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("draft send: status %d, want 422", rr.Code)
	}
	voided := mkCN("CN-3", domain.CreditNoteVoided)
	if rr := cnSendReq(t, h, voided.ID, `{"email":"ap@acme.test"}`); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("voided send: status %d, want 422", rr.Code)
	}
	if sender.called {
		t.Error("draft/voided sends must not reach the sender")
	}

	// Unknown id → 404; missing email → 400; no sender wired → 409.
	h, _ = newH()
	if rr := cnSendReq(t, h, "vlx_cn_missing", `{"email":"a@b.co"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown id: status %d, want 404", rr.Code)
	}
	if rr := cnSendReq(t, h, issued.ID, `{}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("missing email: status %d, want 400", rr.Code)
	}
	bare := NewHandler(NewService(store, nil, nil))
	if rr := cnSendReq(t, bare, issued.ID, `{"email":"a@b.co"}`); rr.Code != http.StatusConflict {
		t.Fatalf("no sender wired: status %d, want 409", rr.Code)
	}

	// Server without SMTP → 409 email_not_configured, nothing queued. The
	// send is queued, so it used to answer 200 and fail out of sight.
	h, sender = newH()
	h.SetEmailConfigured(false)
	rr := cnSendReq(t, h, issued.ID, `{"email":"ap@acme.test"}`)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), `"code":"email_not_configured"`) {
		t.Fatalf("no SMTP: status %d body=%s, want 409 email_not_configured", rr.Code, rr.Body.String())
	}
	if sender.called {
		t.Error("an email was queued on a server that cannot send it")
	}
}
