package invoice

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
)

// errInvoicePDF marks a failure to render the invoice PDF, which the
// operator's "Email invoice" answers with a plain 500 (the render error's
// text never reaches the operator).
var errInvoicePDF = errors.New("render invoice PDF")

// TriggerAutoCollectNoStripe labels the invoice email the auto-charge
// collector sends when Stripe is not connected for the invoice's mode (ADR-060
// amendment 2026-10-30). It is recorded on that send's audit row; the
// operator's "Email invoice" row carries no trigger.
const TriggerAutoCollectNoStripe = "auto_collect_no_stripe"

// invoiceEmailRefusal is why the invoice email cannot be sent for inv, or nil.
// Both senders refuse the same way: the operator's "Email invoice" answers
// with the error (a 409), and the collector's automatic send logs it and
// tries again on its next visit.
func (h *Handler) invoiceEmailRefusal(inv domain.Invoice) error {
	// This email carries a "View & pay invoice" button. Sending it for an
	// invoice whose payment is unresolved asks the customer to pay something we
	// may already have taken from them — and for a parked invoice (ADR-107) the
	// hosted page will not even offer a Pay button when they arrive.
	if b := domain.PaymentBlocksAction(inv, domain.ActionEmailInvoice); b.Blocked {
		return errs.InvalidState(b.Message).WithCode(b.Code)
	}
	// The email says "Your invoice is ready" over the amount due and a pay
	// button. A draft is not issued yet and its lines can still change; a
	// voided invoice is cancelled. Either way the customer would be asked for
	// money they do not owe.
	switch inv.Status {
	case domain.InvoiceDraft:
		return errs.InvalidState("a draft invoice can't be emailed because it isn't issued yet. Finalize it first, then email it").
			WithCode("invoice_not_sendable")
	case domain.InvoiceVoided:
		return errs.InvalidState("a voided invoice can't be emailed because it is cancelled and nothing is owed. To bill the customer, create a new invoice").
			WithCode("invoice_not_sendable")
	}
	// Sends are queued, so a queued email on a server with no SMTP would fail
	// later, out of sight. Refuse up front instead.
	if !h.emailConfigured {
		return errs.EmailNotConfigured()
	}
	return nil
}

// emailInvoice is the invoice email's one body, shared by the operator's
// "Email invoice" (sendEmail) and the collector's automatic send (Emailer):
// the refusals, the CC list (ADR-082: nil ccOverride means the customer's
// stored additional_emails), the PDF from the shared context builder, the
// send, and the audit row. ctx is the caller's, not yet bound to the
// invoice's clock; trigger is empty for the operator's send.
func (h *Handler) emailInvoice(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem, to string, ccOverride *[]string, trigger string) error {
	if err := h.invoiceEmailRefusal(inv); err != nil {
		return err
	}
	// Bind once here so the enqueued email carries the invoice's billing
	// anchor and lands on the invoice's own calendar (ADR-030, ADR-104).
	// Without this the row enqueues unanchored and sorts by real send time —
	// the exact gap the 2026-07-29 walk caught after the anchor shipped.
	bound := h.svc.bindForInvoice(ctx, tenantID, inv.ID)

	cc, err := resolveSendCC(bound, h.customers, tenantID, inv.CustomerID, to, ccOverride)
	if err != nil {
		return err
	}

	// One shared context builder across emailed/downloaded/hosted PDFs —
	// this path previously hand-rolled a THINNER context (no buyer
	// address/tax id, no credit notes), so the emailed document diverged
	// from the downloaded one.
	bt, ci, cnInfos := BuildPDFContext(bound, h.customers, h.settings, h.pdfCreditNotes, tenantID, &inv)

	pdfBytes, err := RenderPDF(bound, inv, items, bt, cnInfos, ci)
	if err != nil {
		return fmt.Errorf("%w: %w", errInvoicePDF, err)
	}

	// AmountDueCents, not Total: the email template labels this figure
	// "Amount due", and credits/partial payments make the two differ —
	// telling a customer they owe the pre-credit total is wrong.
	if err := h.emailSender.SendInvoice(bound, tenantID, to, cc, bt.Name, inv.InvoiceNumber, inv.AmountDueCents, inv.Currency, pdfBytes, inv.PublicToken, inv.Status == domain.InvoiceUncollectible); err != nil {
		return err
	}

	// Explicit audit row so a send is recorded as "Emailed INV-NNN". (It used
	// to also displace the catch-all middleware's generic "create" row; that
	// middleware is deleted — ADR-090 — so this row is now the only record.)
	// No recipient address in the append-only row (GDPR erasure) — the email
	// outbox is the delivery record; the row links the invoice + customer.
	if h.auditLogger != nil {
		meta := map[string]any{
			"invoice_number": inv.InvoiceNumber,
			"customer_id":    inv.CustomerID,
		}
		if trigger != "" {
			meta["trigger"] = trigger
		}
		_ = h.auditLogger.Log(h.svc.AuditCtx(ctx, inv), tenantID, domain.AuditActionSend, "invoice", inv.ID, inv.InvoiceNumber, meta)
	}
	return nil
}

// Emailer sends the invoice email on the auto-charge collector's behalf
// (billing.InvoiceEmailer): when Stripe is not connected for an owed
// invoice's mode, the customer is emailed the invoice itself, once, instead
// of a card-setup link (ADR-060 amendment 2026-10-30). It is the operator's
// "Email invoice" with no override: To is the customer's email address and CC
// their stored list. It reads the handler's collaborators at send time, so it
// sends exactly what the operator's action sends.
type Emailer struct{ h *Handler }

// Emailer returns the handler's invoice email for the collector.
func (h *Handler) Emailer() *Emailer { return &Emailer{h: h} }

// EmailInvoice emails the invoice to its customer. NotifySkippedNoEmail: the
// customer has no email address, nothing was sent. An error sent nothing: a
// refusal (no SMTP on this server, a draft, a void, a payment in flight) or a
// failed read, render or enqueue.
func (m *Emailer) EmailInvoice(ctx context.Context, tenantID, invoiceID string) (domain.NotifyOutcome, error) {
	h := m.h
	inv, items, err := h.svc.GetWithLineItems(ctx, tenantID, invoiceID)
	if err != nil {
		return "", fmt.Errorf("load invoice %s: %w", invoiceID, err)
	}
	if h.customers == nil {
		return "", errors.New("invoice email: no customer reader wired")
	}
	cust, err := h.customers.Get(ctx, tenantID, inv.CustomerID)
	if err != nil {
		return "", fmt.Errorf("read the customer's email address: %w", err)
	}
	to := strings.TrimSpace(cust.Email)
	if to == "" {
		return domain.NotifySkippedNoEmail, nil
	}
	if err := h.emailInvoice(ctx, tenantID, inv, items, to, nil, TriggerAutoCollectNoStripe); err != nil {
		return "", err
	}
	return domain.NotifySent, nil
}
