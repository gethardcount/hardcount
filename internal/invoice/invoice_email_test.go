package invoice

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
)

// emailerFixture is a handler wired as the operator's "Email invoice" is,
// around one finalized invoice whose customer has an email address and a
// stored CC list.
func emailerFixture(t *testing.T, status domain.InvoiceStatus, cust domain.Customer) (*Handler, *ccCapturingSender, *capturingInvoiceAudit, domain.Invoice) {
	t.Helper()
	store := newMemStore()
	inv, err := store.Create(context.Background(), "t1", domain.Invoice{
		InvoiceNumber: "INV-AUTO-1", Status: status, PaymentStatus: domain.PaymentPending,
		CustomerID: "cus_auto", Currency: "USD", TotalAmountCents: 4900, AmountDueCents: 4900,
	})
	if err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	sender := &ccCapturingSender{}
	aud := &capturingInvoiceAudit{}
	h := &Handler{svc: NewService(store, nil, nil), customers: staticCustomerGetter{cust: cust},
		emailSender: sender, auditLogger: aud, emailConfigured: true}
	return h, sender, aud, inv
}

// The collector's invoice email (ADR-060 amendment 2026-10-30) is the
// operator's "Email invoice" with no override, through the same body: To is
// the customer's address, CC their stored list, and the audit row is the
// same send row, marked with the automatic trigger.
func TestEmailer_SendsTheOperatorsEmailToTheCustomer(t *testing.T) {
	h, sender, aud, inv := emailerFixture(t, domain.InvoiceFinalized, domain.Customer{
		ID: "cus_auto", Email: "ap@acme.test", AdditionalEmails: []string{"finance@acme.test"},
	})

	outcome, err := h.Emailer().EmailInvoice(context.Background(), "t1", inv.ID)
	if err != nil || outcome != domain.NotifySent {
		t.Fatalf("EmailInvoice = %q, %v; want sent", outcome, err)
	}
	if !sender.called || sender.to != "ap@acme.test" || !reflect.DeepEqual(sender.cc, []string{"finance@acme.test"}) {
		t.Errorf("sent to %q cc %v (called %v), want ap@acme.test cc [finance@acme.test]", sender.to, sender.cc, sender.called)
	}
	row, ok := aud.firstOf(domain.AuditActionSend)
	if !ok || row.label != "INV-AUTO-1" || row.meta["trigger"] != TriggerAutoCollectNoStripe || row.meta["customer_id"] != "cus_auto" {
		t.Errorf("audit rows %+v, want one send row for INV-AUTO-1 with trigger %q", aud.calls, TriggerAutoCollectNoStripe)
	}
}

// The operator's send writes the same row with no trigger: its metadata is
// what it was before the body was shared.
func TestSendEmail_AuditRowHasNoTrigger(t *testing.T) {
	h, _, aud, inv := emailerFixture(t, domain.InvoiceFinalized, domain.Customer{ID: "cus_auto", Email: "ap@acme.test"})
	if rr := sendReq(t, h, inv.ID, `{"email":"ap@acme.test"}`); rr.Code != 200 {
		t.Fatalf("send: status %d body=%s", rr.Code, rr.Body.String())
	}
	row, ok := aud.firstOf(domain.AuditActionSend)
	if !ok {
		t.Fatalf("no send row, got %v", aud.actions())
	}
	if want := map[string]any{"invoice_number": "INV-AUTO-1", "customer_id": "cus_auto"}; !reflect.DeepEqual(row.meta, want) {
		t.Errorf("operator send row metadata %v, want %v", row.meta, want)
	}
}

// A customer with no email address is a typed skip, not a send; nothing
// without SMTP, and nothing for a draft or a void, each an error that sent
// nothing, so the collector leaves the invoice unstamped and tries again.
func TestEmailer_RefusesAndSkipsLikeTheOperatorsSend(t *testing.T) {
	t.Run("no customer email", func(t *testing.T) {
		h, sender, _, inv := emailerFixture(t, domain.InvoiceFinalized, domain.Customer{ID: "cus_auto"})
		outcome, err := h.Emailer().EmailInvoice(context.Background(), "t1", inv.ID)
		if err != nil || outcome != domain.NotifySkippedNoEmail || sender.called {
			t.Fatalf("EmailInvoice = %q, %v (sent %v); want skipped_no_email, nothing sent", outcome, err, sender.called)
		}
	})
	t.Run("no SMTP", func(t *testing.T) {
		h, sender, _, inv := emailerFixture(t, domain.InvoiceFinalized, domain.Customer{ID: "cus_auto", Email: "ap@acme.test"})
		h.emailConfigured = false
		_, err := h.Emailer().EmailInvoice(context.Background(), "t1", inv.ID)
		if errs.Code(err) != errs.CodeEmailNotConfigured || sender.called {
			t.Fatalf("EmailInvoice err %v (sent %v), want email_not_configured and nothing sent", err, sender.called)
		}
	})
	for _, status := range []domain.InvoiceStatus{domain.InvoiceDraft, domain.InvoiceVoided} {
		t.Run(string(status), func(t *testing.T) {
			h, sender, _, inv := emailerFixture(t, status, domain.Customer{ID: "cus_auto", Email: "ap@acme.test"})
			_, err := h.Emailer().EmailInvoice(context.Background(), "t1", inv.ID)
			if errs.Code(err) != "invoice_not_sendable" || !errors.Is(err, errs.ErrInvalidState) || sender.called {
				t.Fatalf("EmailInvoice err %v (sent %v), want invoice_not_sendable and nothing sent", err, sender.called)
			}
		})
	}
}
