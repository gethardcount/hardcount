package domain

import (
	"strings"
	"testing"
	"time"
)

// With no HOSTED_INVOICE_BASE_URL the hosted invoice page takes no payment
// and the payment email has no link. The authentication_required notice
// must not tell the operator to email that link: it says to set the address,
// and offers no email action.
func TestNeedsConfirmationAttention_HostedPayOff(t *testing.T) {
	failed := Invoice{
		ID: "inv_1", Status: InvoiceFinalized, PaymentStatus: PaymentFailed,
		LastPaymentError: "This payment requires authentication.",
		UpdatedAt:        time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC),
	}
	for _, retrying := range []bool{true, false} {
		att := ClassifyInvoiceAttention(failed, AttentionContext{
			HasPaymentMethod: true, RetryBlockingDeclineCode: AuthenticationRequiredCode,
			DunningRetrying: retrying, HostedPayOff: true,
		})
		if att == nil || att.Code != "payment.authentication_required" {
			t.Fatalf("retrying=%v: attention %+v, want payment.authentication_required", retrying, att)
		}
		checkShape(t, att.Message)
		if !strings.Contains(att.Message, "HOSTED_INVOICE_BASE_URL") || !strings.Contains(att.Detail, "HOSTED_INVOICE_BASE_URL") {
			t.Errorf("retrying=%v: message/detail do not name the setting:\n%s\n%s", retrying, att.Message, att.Detail)
		}
		if strings.Contains(att.Message, "email them the payment link") {
			t.Errorf("retrying=%v: message still sends the operator to a link that doesn't exist: %q", retrying, att.Message)
		}
		if len(att.Actions) != 0 {
			t.Errorf("retrying=%v: actions = %v, want none (the payment email would carry no link)", retrying, actionCodes(att))
		}
	}
}

// The card_refused notice offers the hosted invoice page only when it can
// take payment.
func TestRefusedCardAttention_HostedPayOff(t *testing.T) {
	failed := Invoice{
		ID: "inv_1", Status: InvoiceFinalized, PaymentStatus: PaymentFailed,
		LastPaymentError: "Your card was declined.",
		UpdatedAt:        time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC),
	}
	for _, retrying := range []bool{true, false} {
		on := ClassifyInvoiceAttention(failed, AttentionContext{HasPaymentMethod: true, RetryBlockingDeclineCode: "stolen_card", DunningRetrying: retrying})
		off := ClassifyInvoiceAttention(failed, AttentionContext{HasPaymentMethod: true, RetryBlockingDeclineCode: "stolen_card", DunningRetrying: retrying, HostedPayOff: true})
		if on == nil || off == nil || on.Code != "payment.card_refused" || off.Code != "payment.card_refused" {
			t.Fatalf("retrying=%v: got %+v / %+v, want payment.card_refused", retrying, on, off)
		}
		if !strings.Contains(on.Detail, "hosted invoice page") {
			t.Errorf("retrying=%v: with the page working, detail no longer offers it: %q", retrying, on.Detail)
		}
		if strings.Contains(off.Detail, "hosted invoice page") {
			t.Errorf("retrying=%v: with no hosted address, detail still offers the page: %q", retrying, off.Detail)
		}
		if strings.HasSuffix(off.Detail, " ") {
			t.Errorf("retrying=%v: detail ends with a space: %q", retrying, off.Detail)
		}
	}
}

// With no CUSTOMER_PORTAL_URL the customer page can't make a setup link to
// copy, so the no-payment-method notice doesn't offer one.
func TestNoPaymentMethodCopy_SetupLinkCopyOff(t *testing.T) {
	const copyFrom = "copy one from the customer page"
	cases := []struct {
		name               string
		hasEmail, emailOff bool
		line2              string
	}{
		{"email, sending works", true, false, "A setup link is emailed to the customer once; use Resend setup link to send it again."},
		{"no email", false, false, "Setup links are emailed only to customers with an email address, and this server can't make one to copy."},
		{"email off", true, true, "This server can't email setup links or make one to copy, so none was sent."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			att := ClassifyInvoiceAttention(noPMInvoice(false), AttentionContext{
				StripeConnected: true, CustomerHasEmail: tc.hasEmail, SetupLinkEmailOff: tc.emailOff, SetupLinkCopyOff: true,
			})
			if att == nil || att.Reason != AttentionReasonNoPaymentMethod {
				t.Fatalf("got %+v, want no_payment_method", att)
			}
			checkShape(t, att.Message)
			lines := strings.Split(att.Message, "\n")
			if got := lines[len(lines)-1]; got != tc.line2 {
				t.Errorf("line 2 = %q, want %q", got, tc.line2)
			}
			if strings.Contains(att.Message, copyFrom) || strings.Contains(att.Detail, "can also copy") {
				t.Errorf("notice still offers a copied link:\n%s\n%s", att.Message, att.Detail)
			}
			if !strings.Contains(att.Detail, "CUSTOMER_PORTAL_URL") {
				t.Errorf("detail does not say how to make copying work: %q", att.Detail)
			}
		})
	}
}
