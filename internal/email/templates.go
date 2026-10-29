package email

import (
	"bytes"
	"html"
	"html/template"
	"strings"

	"github.com/gethardcount/hardcount/internal/domain"
)

// Branding is the per-tenant visual identity a sender pulls from
// TenantSettings before rendering an outbound email. Every field is
// optional, and an unset field stays empty: no logo means no <img>, no
// company name means no seller name in the header or the From name. The
// email never fills in "Hardcount" as the seller, because Hardcount is the
// software, not the business billing the customer.
type Branding struct {
	CompanyName string
	LogoURL     string
	BrandColor  string // 7-char hex, e.g. #1f6feb. Empty = fall back to neutral.
	SupportURL  string
	FromName    string // Appears as "From: FromName <from@address>" when set.
}

// layoutTemplate is the shared chrome every customer-facing HTML email
// wraps around its specific content. Mirrors the hosted invoice page
// aesthetic (T0-17.5) so a customer landing on the page from an email
// CTA sees visual continuity. Inline styles throughout because email
// clients (Outlook especially) strip <style> blocks inconsistently —
// industry best practice is inline everything.
//
// Structure:
//   - optional 3px brand_color accent bar at the very top
//   - header: tenant logo + company name, each only when set
//   - content: per-email HTML (injected as template.HTML, pre-rendered)
//   - optional CTA button
//   - footer: support link + "Powered by Hardcount Billing" micro-credit
const layoutHTML = `<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Subject}}</title>
</head>
<body style="margin:0;padding:0;background-color:#f5f6f8;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:#1f2937;">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background-color:#f5f6f8;">
    <tr>
      <td align="center" style="padding:32px 16px;">
        <table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:600px;background:#ffffff;border-radius:8px;overflow:hidden;box-shadow:0 1px 3px rgba(0,0,0,0.04);">
          {{if .BrandColor}}<tr><td style="height:3px;background-color:{{.BrandColor}};line-height:0;font-size:0;">&nbsp;</td></tr>{{end}}
          {{if or .LogoURL .CompanyName}}<tr>
            <td style="padding:24px 32px;border-bottom:1px solid #e5e7eb;">
              <table role="presentation" cellpadding="0" cellspacing="0">
                <tr>
                  {{if .LogoURL}}<td style="padding-right:12px;vertical-align:middle;"><img src="{{.LogoURL}}" alt="" height="32" style="display:block;max-height:32px;"></td>{{end}}
                  {{if .CompanyName}}<td style="vertical-align:middle;font-size:16px;font-weight:600;color:#111827;">{{.CompanyName}}</td>{{end}}
                </tr>
              </table>
            </td>
          </tr>{{end}}
          <tr>
            <td style="padding:32px;font-size:15px;line-height:1.6;color:#1f2937;">
              {{.Content}}
              {{if .CTAURL}}
              <table role="presentation" cellpadding="0" cellspacing="0" style="margin:24px 0 8px;">
                <tr>
                  <td style="border-radius:6px;background-color:{{if .BrandColor}}{{.BrandColor}}{{else}}#111827{{end}};">
                    <a href="{{.CTAURL}}" style="display:inline-block;padding:12px 24px;color:#ffffff;text-decoration:none;font-weight:500;font-size:15px;">{{.CTALabel}}</a>
                  </td>
                </tr>
              </table>
              {{end}}
            </td>
          </tr>
          <tr>
            <td style="padding:20px 32px;border-top:1px solid #e5e7eb;font-size:13px;color:#6b7280;">
              {{if .SupportURL}}Need help? <a href="{{.SupportURL}}" style="color:#6b7280;text-decoration:underline;">Contact support</a><br>{{end}}
              <span style="color:#9ca3af;">Powered by Hardcount Billing</span>
            </td>
          </tr>
        </table>
      </td>
    </tr>
  </table>
</body>
</html>`

// layoutInputs is the view model fed into layoutHTML. Content is
// pre-rendered HTML (template.HTML marks it pre-escaped) so per-email
// templates can inject their own safe markup without html/template
// double-escaping it.
type layoutInputs struct {
	Subject     string
	CompanyName string
	LogoURL     string
	BrandColor  string
	SupportURL  string
	Content     template.HTML
	CTAURL      string
	CTALabel    string
}

var layoutTmpl = template.Must(template.New("layout").Parse(layoutHTML))

// renderLayout assembles the final HTML email by wrapping pre-rendered
// content in the shared chrome.
func renderLayout(inputs layoutInputs) (string, error) {
	var buf bytes.Buffer
	if err := layoutTmpl.Execute(&buf, inputs); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// escape wraps html.EscapeString so per-email helpers can build
// template.HTML content without dragging in the full html/template
// pipeline for single-field interpolation.
func escape(s string) string { return html.EscapeString(s) }

// --- Per-email content builders ---

// Each builder returns a (subject, html-content, cta-url, cta-label)
// tuple. The caller renders through renderLayout then ships via the
// multipart pipeline alongside the plaintext fallback.

// zeroDue is the plumbed fact `amount_due_cents == 0` (paid or fully
// credit-covered at send time). The copy adapts: the old rendering titled
// a settled invoice "Invoice X — USD 0.00" with a "View & pay" CTA —
// asking the customer to pay nothing (2026-07-19 FLOW E copy finding).
//
// writtenOff is the invoice's uncollectible status. Its hosted page has no
// Pay button (ADR-110), so the email offers "View invoice" and asks the
// customer to contact the seller, never "View & pay".
func renderInvoiceHTML(customerName, invoiceNumber, amount string, hostedURL string, zeroDue, writtenOff bool) (subject, contentHTML, ctaURL, ctaLabel string) {
	subject = "Invoice " + invoiceNumber + " — " + amount
	if zeroDue {
		subject = "Invoice " + invoiceNumber + " — no payment due"
	}
	heading := "Your invoice is ready"
	if writtenOff && !zeroDue {
		heading = "Your invoice"
	}
	var b strings.Builder
	b.WriteString(`<h1 style="margin:0 0 12px;font-size:20px;color:#111827;">` + heading + `</h1>`)
	b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">Hi ` + escape(customerName) + `,</p>`)
	b.WriteString(`<p style="margin:0 0 20px;color:#4b5563;">Invoice <strong style="color:#111827;">` + escape(invoiceNumber) + `</strong> is now available.</p>`)
	b.WriteString(`<div style="background:#f9fafb;border-radius:8px;padding:16px 20px;margin:8px 0 16px;">`)
	if zeroDue {
		b.WriteString(`<div style="font-size:13px;color:#6b7280;text-transform:uppercase;letter-spacing:0.05em;">Amount due</div>`)
		b.WriteString(`<div style="font-size:24px;font-weight:600;color:#111827;margin-top:4px;font-variant-numeric:tabular-nums;">` + escape(amount) + ` — settled</div>`)
	} else {
		b.WriteString(`<div style="font-size:13px;color:#6b7280;text-transform:uppercase;letter-spacing:0.05em;">Amount due</div>`)
		b.WriteString(`<div style="font-size:24px;font-weight:600;color:#111827;margin-top:4px;font-variant-numeric:tabular-nums;">` + escape(amount) + `</div>`)
	}
	b.WriteString(`</div>`)
	switch {
	case zeroDue && hostedURL != "":
		b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">This invoice is settled — no payment is needed. The PDF is attached for your records.</p>`)
		ctaURL = hostedURL
		ctaLabel = "View invoice"
	case zeroDue:
		b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">This invoice is settled — no payment is needed. The PDF is attached for your records.</p>`)
	case writtenOff:
		b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">` + escape(closedForOnlinePayment) + ` The PDF is attached for your records.</p>`)
		if hostedURL != "" {
			ctaURL = hostedURL
			ctaLabel = "View invoice"
		}
	case hostedURL != "":
		b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">Tap the button below to view or pay your invoice. The PDF is also attached for your records.</p>`)
		ctaURL = hostedURL
		ctaLabel = "View & pay invoice"
	default:
		b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">Please find the invoice PDF attached.</p>`)
	}
	return subject, b.String(), ctaURL, ctaLabel
}

func renderReceiptHTML(customerName, invoiceNumber, amount, hostedURL string) (subject, contentHTML, ctaURL, ctaLabel string) {
	subject = "Payment received for invoice " + invoiceNumber
	var b strings.Builder
	b.WriteString(`<h1 style="margin:0 0 12px;font-size:20px;color:#111827;">Payment received</h1>`)
	b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">Hi ` + escape(customerName) + `,</p>`)
	b.WriteString(`<p style="margin:0 0 20px;color:#4b5563;">Thanks — we've received your payment of <strong style="color:#111827;">` + escape(amount) + `</strong> for invoice <strong style="color:#111827;">` + escape(invoiceNumber) + `</strong>.</p>`)
	if hostedURL != "" {
		ctaURL = hostedURL
		ctaLabel = "View receipt"
	}
	return subject, b.String(), ctaURL, ctaLabel
}

func renderDunningWarningHTML(customerName, invoiceNumber string, attemptNumber, maxAttempts int, nextRetryDate, failureReason, hostedURL string) (subject, contentHTML, ctaURL, ctaLabel string) {
	// No final-attempt variant: the dunning service deliberately skips
	// the warning on the attempt that exhausts the retry budget (N-1
	// warnings during retries 1..N-1, then ONE escalation email — see
	// willExhaustThisAttempt in internal/dunning/service.go), so every
	// warning rendered here has a real next retry ahead of it.
	subject = "Action required — payment retry for invoice " + invoiceNumber
	var b strings.Builder
	b.WriteString(`<h1 style="margin:0 0 12px;font-size:20px;color:#111827;">We weren't able to process your payment</h1>`)
	b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">Hi ` + escape(customerName) + `,</p>`)
	b.WriteString(`<p style="margin:0 0 12px;color:#4b5563;">` + escape(retryFailedSentence(invoiceNumber, attemptNumber, maxAttempts)) + `</p>`)
	// Surface the decline reason inline so customers can act
	// (insufficient_funds → top up; lost_card → swap card). Empty
	// reason = no callout, no false noise.
	if strings.TrimSpace(failureReason) != "" {
		b.WriteString(`<div style="background:#fef2f2;border-left:3px solid #fca5a5;padding:10px 14px;margin:0 0 16px;color:#7f1d1d;font-size:13px;">`)
		b.WriteString(`Reason: ` + escape(failureReason))
		b.WriteString(`</div>`)
	}
	b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">` + escape(retryNextSentence(nextRetryDate)) + `</p>`)
	if hostedURL != "" {
		ctaURL = hostedURL
		ctaLabel = "Pay invoice"
	}
	return subject, b.String(), ctaURL, ctaLabel
}

// retryFailedSentence and retryNextSentence are the ordinary warning's copy,
// shared by its HTML and plain-text parts so the two cannot drift. The count
// is of retries: the charge that started dunning came first and is not one of
// them, so "Attempt 1 of 3" understated what the customer had been through.
func retryFailedSentence(invoiceNumber string, retry, retries int) string {
	return "Retry " + itoa(retry) + " of " + itoa(retries) + " to charge invoice " + invoiceNumber + " didn't go through."
}

func retryNextSentence(nextRetryDate string) string {
	return "We'll try again on " + nextRetryDate + ". To avoid further retries, please pay the invoice or update your payment method."
}

// renderNoCardWarningHTML is the dunning warning for a retry that found no
// card on file (domain.DeclineAskAddCard). It charged nothing, so it says
// nothing about a charge that didn't go through, shows no retry count, and
// promises no retry date: no retry can succeed until a card is added, and the
// hosted invoice page it links to takes a payment but cannot save a card.
func renderNoCardWarningHTML(customerName, invoiceNumber, hostedURL string) (subject, contentHTML, ctaURL, ctaLabel string) {
	subject = "Action required — payment for invoice " + invoiceNumber
	var b strings.Builder
	b.WriteString(`<h1 style="margin:0 0 12px;font-size:20px;color:#111827;">Your invoice is unpaid</h1>`)
	b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">Hi ` + escape(customerName) + `,</p>`)
	b.WriteString(`<p style="margin:0 0 12px;color:#4b5563;">` + escape(noCardSentence(invoiceNumber)) + `</p>`)
	b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">` + escape(payOrContactAsk(hostedURL != "")) + `</p>`)
	if hostedURL != "" {
		ctaURL = hostedURL
		ctaLabel = "Pay invoice"
	}
	return subject, b.String(), ctaURL, ctaLabel
}

// noCardSentence is the no-card warning's copy, shared by its HTML and
// plain-text parts.
func noCardSentence(invoiceNumber string) string {
	return "We couldn't charge invoice " + invoiceNumber + " because there is no payment method on file."
}

// payOrContactAsk asks the customer to pay on the hosted invoice page, or,
// when this server has no hosted page to link, to contact the seller.
func payOrContactAsk(hasLink bool) string {
	if hasLink {
		return "Please pay the invoice using the link below."
	}
	return "Please contact us to pay the invoice."
}

// renderCardRefusedWarningHTML is the dunning warning for a card the bank
// refused for good (#966): lost, stolen, revoked and the rest of
// domain.IsHardDecline. Dunning retries skip that card, so unlike
// renderDunningWarningHTML it promises no retry date and shows no attempt
// count. It does not say the card won't be charged again: Collect payment
// and the customer's next invoice still charge it. It does not say why the bank refused: Stripe asks
// that lost, stolen and fraud declines reach the customer as a plain decline
// (https://docs.stripe.com/declines/codes).
func renderCardRefusedWarningHTML(customerName, invoiceNumber, hostedURL string) (subject, contentHTML, ctaURL, ctaLabel string) {
	subject = "Action required — use a different card for invoice " + invoiceNumber
	var b strings.Builder
	b.WriteString(`<h1 style="margin:0 0 12px;font-size:20px;color:#111827;">Please use a different card</h1>`)
	b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">Hi ` + escape(customerName) + `,</p>`)
	b.WriteString(`<p style="margin:0 0 12px;color:#4b5563;">` + escape(cardRefusedSentence(invoiceNumber)) + `</p>`)
	b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">` + escape(cardRefusedAsk) + `</p>`)
	if hostedURL != "" {
		ctaURL = hostedURL
		ctaLabel = "Pay invoice"
	}
	return subject, b.String(), ctaURL, ctaLabel
}

// cardRefusedSentence and cardRefusedAsk are the card-refused warning's copy,
// shared by its HTML and plain-text parts so the two cannot drift.
func cardRefusedSentence(invoiceNumber string) string {
	return "Your bank declined the card on file for invoice " + invoiceNumber + "."
}

const cardRefusedAsk = "Please pay the invoice with a different card."

// renderConfirmPaymentHTML is the email for a payment the customer's bank
// wants them to confirm (domain.DeclineAskConfirm: Stripe's
// authentication_required, usually 3-D Secure). It is both the first notice
// and each dunning reminder, as Stripe Billing sends a confirmation link and
// reminders until the customer confirms
// (https://docs.stripe.com/billing/revenue-recovery/customer-emails#payment-confirmation-notifications).
// Nothing was declined, so it says nothing about a decline, and no retry
// date: a charge made without the customer cannot pass the bank's check.
// The hosted invoice page runs the confirmation when the customer pays.
func renderConfirmPaymentHTML(customerName, invoiceNumber, hostedURL string) (subject, contentHTML, ctaURL, ctaLabel string) {
	subject = "Action required — confirm your payment for invoice " + invoiceNumber
	var b strings.Builder
	b.WriteString(`<h1 style="margin:0 0 12px;font-size:20px;color:#111827;">Please confirm your payment</h1>`)
	b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">Hi ` + escape(customerName) + `,</p>`)
	b.WriteString(`<p style="margin:0 0 12px;color:#4b5563;">` + escape(confirmPaymentSentence(invoiceNumber)) + `</p>`)
	b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">` + escape(confirmPaymentAsk(hostedURL != "")) + `</p>`)
	if hostedURL != "" {
		ctaURL = hostedURL
		ctaLabel = "Pay invoice"
	}
	return subject, b.String(), ctaURL, ctaLabel
}

// confirmPaymentSentence and confirmPaymentAsk are the confirm email's copy,
// shared by its HTML and plain-text parts so the two cannot drift.
func confirmPaymentSentence(invoiceNumber string) string {
	return "Your bank asked you to confirm the payment for invoice " + invoiceNumber + " before it can go through."
}

func confirmPaymentAsk(hasLink bool) string {
	if hasLink {
		return "Please pay the invoice using the link below. Your bank may ask you to confirm the payment."
	}
	return "Please contact us to pay the invoice."
}

func renderDunningEscalationHTML(customerName, invoiceNumber string, outcome domain.DunningEscalationOutcome, hostedURL string) (subject, contentHTML, ctaURL, ctaLabel string) {
	subject = "Action required for invoice " + invoiceNumber
	var b strings.Builder
	b.WriteString(`<h1 style="margin:0 0 12px;font-size:20px;color:#111827;">We couldn't collect payment</h1>`)
	b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">Hi ` + escape(customerName) + `,</p>`)
	b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">` + escape(escalationLead(invoiceNumber)) + `</p>`)

	// Never render the internal enum — that leak (P13) showed the debtor
	// "Action taken: mark_uncollectible". dunningEscalationCopy composes
	// the customer sentence from the OUTCOME and is shared with the
	// plaintext part.
	sentence, stillPayable := dunningEscalationCopy(outcome)
	b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">` + escape(sentence) + `</p>`)
	if hostedURL != "" {
		ctaURL = hostedURL
		ctaLabel = "Pay invoice"
		if !stillPayable {
			ctaLabel = "View invoice"
		}
	}
	return subject, b.String(), ctaURL, ctaLabel
}

// escalationLead opens the escalation email in both parts. It says only that
// the invoice is unpaid: "All payment attempts were unsuccessful" was false
// for a customer with no card on file, whose retries charged nothing.
func escalationLead(invoiceNumber string) string {
	return "Invoice " + invoiceNumber + " is still unpaid."
}

// closedForOnlinePayment is what a customer is told about a written-off
// invoice: the hosted page offers no Pay button (ADR-110) and nothing charges
// it, but the money is still owed. Shared by the escalation and invoice
// emails.
const closedForOnlinePayment = "This invoice is closed for online payment. Please contact us to arrange payment."

// dunningEscalationCopy composes the customer-facing sentence used by BOTH
// multipart/alternative parts of the escalation email. Single source on
// purpose — the P13 raw-enum leak was fixed in the HTML part only, and the
// plaintext twin kept showing text-only clients "Action taken:
// mark_uncollectible" for another two months.
//
// It reads the OUTCOME, never the policy (ADR-112): exhaustion now settles
// two independent things, and only the outcome knows which of them applied
// to this invoice. A one-off invoice under a cancel policy cancels nothing,
// so it must get the neutral sentence.
//
// stillPayable=false once the invoice is written off — a written-off
// invoice has no Pay button on the hosted page (ADR-110), so callers offer
// the link as "View invoice" and point at support rather than a dead end.
// It is the invoice half that decides this; the subscription half never
// changes whether THIS invoice can be paid online.
func dunningEscalationCopy(o domain.DunningEscalationOutcome) (sentence string, stillPayable bool) {
	// Subscription clause. Pause is collection-only (keep_as_draft):
	// service continues and new invoices stay drafts, so no "service is
	// paused" / "resume service" claim is made. Nor "collection stopped": a
	// dunning run open on another unpaid invoice keeps retrying the card.
	var subClause string
	switch {
	case o.SubscriptionCanceled:
		subClause = "Your subscription has been canceled."
	case o.SubscriptionPaused:
		subClause = "New invoices for your subscription will not be charged automatically."
	}

	if o.InvoiceWrittenOff {
		// Closed for online payment, and the balance is NOT described as
		// still collectible through the link — it isn't.
		s := closedForOnlinePayment
		if subClause != "" {
			s = subClause + " " + s
		}
		return s, false
	}

	if subClause != "" {
		return subClause + " The outstanding balance remains due — you can settle it using the link below.", true
	}

	// Nothing applied: a (none, none) policy, a one-off invoice whose
	// policy only acts on subscriptions, or an unwired mover. Neutral — no
	// state change is asserted.
	return "The invoice remains due. Please settle it using the link below or contact us.", true
}

func renderPaymentFailedHTML(customerName, invoiceNumber, reason, hostedURL string) (subject, contentHTML, ctaURL, ctaLabel string) {
	subject = "Payment failed for invoice " + invoiceNumber
	var b strings.Builder
	b.WriteString(`<h1 style="margin:0 0 12px;font-size:20px;color:#111827;">Payment unsuccessful</h1>`)
	b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">Hi ` + escape(customerName) + `,</p>`)
	b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">We weren't able to process the charge for invoice <strong style="color:#111827;">` + escape(invoiceNumber) + `</strong>.</p>`)
	if strings.TrimSpace(reason) != "" {
		b.WriteString(`<div style="background:#fef2f2;border-left:3px solid #fca5a5;padding:12px 16px;margin:8px 0 16px;color:#7f1d1d;font-size:14px;">` + escape(reason) + `</div>`)
	}
	b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">Pay the invoice or update your payment method to avoid service interruption.</p>`)
	if hostedURL != "" {
		ctaURL = hostedURL
		ctaLabel = "Pay invoice"
	}
	return subject, b.String(), ctaURL, ctaLabel
}

// renderPaymentUpdateRequestHTML is the invoice-context thin wrapper
// retained for back-compat with the SendPaymentSetupRequest call
// site. Delegates to the unified renderPaymentSetupLinkHTML so the
// template stays single-source.
func renderPaymentUpdateRequestHTML(customerName, invoiceNumber, amountDue, updateURL string, writtenOff bool) (subject, contentHTML, ctaURL, ctaLabel string) {
	return renderPaymentSetupLinkHTML(paymentSetupLinkContext{
		CustomerName:   customerName,
		SetupURL:       updateURL,
		InvoiceNumber:  invoiceNumber,
		AmountDueLabel: amountDue,
		WrittenOff:     writtenOff,
	})
}

// paymentSetupLinkContext is the unified shape for both
// operator-initiated and engine-fired "add a payment method" emails.
// Invoice fields are optional: when InvoiceNumber is empty the
// template renders the generic operator-initiated copy; when set
// it embeds invoice context ("for invoice X — $Y") so the customer
// reads exactly why they need to add a card.
type paymentSetupLinkContext struct {
	CustomerName   string
	OperatorNote   string // optional free-form operator note
	SetupURL       string
	InvoiceNumber  string // optional — engine-fired no-PM-at-finalize path
	AmountDueLabel string // formatted ($X.XX); empty when InvoiceNumber empty
	// WrittenOff marks the invoice as already written off as bad debt. It
	// changes ONE thing and it is the honesty of the whole email: NOTHING
	// collects a written-off invoice (ADR-113 — every charge claim pins
	// `finalized`), so the ordinary "we'll collect it automatically" promise
	// would be a commitment the engine cannot keep. Adding a card is still
	// genuinely useful — it serves the fresh recovery invoice and future
	// billing — but the customer is asked to contact the seller to settle
	// this one. Not "we'll be in touch": nothing sends that follow-up.
	WrittenOff bool
}

// renderPaymentSetupLinkHTML is the single template used by every
// "add a payment method" email send (operator-initiated +
// engine-fired no-PM-at-finalize). Replaces the previous two-
// template split (renderPaymentUpdateRequestHTML +
// renderPaymentSetupLinkHTML) so subject + body stay consistent
// across send paths. Industry parity: Stripe "Send Payment Method",
// Chargebee "Request Payment Method" — both use one template
// internally with optional invoice context.
func renderPaymentSetupLinkHTML(ctx paymentSetupLinkContext) (subject, contentHTML, ctaURL, ctaLabel string) {
	hasInvoice := ctx.InvoiceNumber != ""
	// Invoice context means the NO-PAYMENT-METHOD path (finalize-time or
	// operator resend) — never a decline. Real declines are emailed by the
	// dunning templates and link to the hosted invoice, not here. The
	// previous copy ("We couldn't process payment… update your card")
	// asserted a decline to customers who were never charged and had no
	// card to update — born-false for 100% of this template's audience
	// (2026-07-22 payment-surfacing audit, P1-2).
	if hasInvoice {
		subject = "Set up payment for invoice " + ctx.InvoiceNumber
	} else {
		subject = "Add a payment method"
	}
	var b strings.Builder
	heading := "Add a payment method"
	if hasInvoice {
		heading = "Set up your payment method"
	}
	b.WriteString(`<h1 style="margin:0 0 12px;font-size:20px;color:#111827;">` + escape(heading) + `</h1>`)
	b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">Hi ` + escape(ctx.CustomerName) + `,</p>`)
	switch {
	case ctx.OperatorNote != "":
		// Operator's custom note renders verbatim — they may have
		// added billing context ("your card on file expired") that
		// the template shouldn't try to autocompose.
		b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">` + escape(ctx.OperatorNote) + `</p>`)
	case hasInvoice:
		lead, ask := setupLinkInvoiceCopy(ctx.InvoiceNumber, ctx.AmountDueLabel, ctx.WrittenOff)
		b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">` + escape(lead) + `</p>`)
		b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">` + escape(ask) + `</p>`)
	default:
		b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">Please add a payment method on file so we can process your billing. Use the secure link below — your card details go directly to our payment processor and never touch our servers.</p>`)
	}
	b.WriteString(`<p style="margin:0 0 16px;color:#6b7280;font-size:13px;">The link expires in 24 hours and can only be used once.</p>`)
	ctaURL = ctx.SetupURL
	ctaLabel = "Add payment method"
	return subject, b.String(), ctaURL, ctaLabel
}

// setupLinkInvoiceCopy is the setup-link email's invoice copy, shared by its
// HTML and plain-text parts. The text part used to promise "we'll collect it
// automatically" on a written-off invoice, which nothing collects, while the
// HTML part had been fixed.
func setupLinkInvoiceCopy(invoiceNumber, amountDue string, writtenOff bool) (lead, ask string) {
	const secure = " Your card details go directly to our payment processor and never touch our servers."
	if writtenOff {
		return "Invoice " + invoiceNumber + " (" + amountDue + ") is unpaid.",
			"Add a payment method with the secure link below for future billing. It won't pay this invoice automatically, so please contact us to settle it." + secure
	}
	return "Invoice " + invoiceNumber + " (" + amountDue + ") is ready.",
		"Add a payment method with the secure link below and we'll collect it automatically." + secure
}

// itoa is a local shim to avoid pulling strconv just for escape() input.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// renderCreditNoteHTML is the credit-note document email (ADR-082
// rider). No CTA — no hosted CN page exists; the PDF is the document.
// invoiceNumber names the APPLIED invoice so finance can match the
// credit to the bill it adjusts.
func renderCreditNoteHTML(customerName, creditNoteNumber, invoiceNumber, amount, companyName string) (subject, contentHTML string) {
	from := companyName
	if from == "" {
		from = "your billing team"
	}
	subject = "Credit note " + creditNoteNumber + " from " + from
	var b strings.Builder
	b.WriteString(`<h1 style="margin:0 0 12px;font-size:20px;color:#111827;">Credit note issued</h1>`)
	b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">Hi ` + escape(customerName) + `,</p>`)
	b.WriteString(`<p style="margin:0 0 20px;color:#4b5563;">Credit note <strong style="color:#111827;">` + escape(creditNoteNumber) + `</strong> has been issued against invoice <strong style="color:#111827;">` + escape(invoiceNumber) + `</strong>.</p>`)
	b.WriteString(`<div style="background:#f9fafb;border-radius:8px;padding:16px 20px;margin:8px 0 16px;">`)
	b.WriteString(`<div style="font-size:13px;color:#6b7280;text-transform:uppercase;letter-spacing:0.05em;">Amount credited</div>`)
	b.WriteString(`<div style="font-size:24px;font-weight:600;color:#111827;margin-top:4px;font-variant-numeric:tabular-nums;">` + escape(amount) + `</div>`)
	b.WriteString(`</div>`)
	b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">The credit note PDF is attached for your records.</p>`)
	return subject, b.String()
}
