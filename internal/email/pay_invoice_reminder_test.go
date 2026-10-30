package email

import (
	"strings"
	"testing"
)

// The reminder a dunning run sends while Stripe is not connected
// (DeclineAskPayInvoice, ADR-060 amendment 2026-10-30) asks the customer to
// pay the invoice and links the hosted page. Nothing was charged and no card
// can be added online, so neither part may ask for a card, mention a
// payment method, or promise a retry; the button says "View invoice"
// because the page cannot take a payment. Without a hosted link it asks the
// customer to contact the seller.
func TestPayInvoiceReminder_AsksToPayNeverForACard(t *testing.T) {
	const hosted = "https://pay.example.com/invoice/tok_abc"
	subject, html, ctaURL, ctaLabel := renderPayInvoiceReminderHTML("Acme", "INV-7", hosted)
	text := payInvoiceReminderTextBody("Acme", "INV-7", hosted)

	if ctaURL != hosted || ctaLabel != "View invoice" {
		t.Errorf("button %q → %q, want \"View invoice\" → the hosted page", ctaLabel, ctaURL)
	}
	for part, body := range map[string]string{"subject": subject, "html": html, "text": text} {
		low := strings.ToLower(body)
		for _, banned := range []string{"card", "payment method", "retry", "try again", "charge"} {
			if strings.Contains(low, banned) {
				t.Errorf("%s mentions %q: %s", part, banned, body)
			}
		}
	}
	for _, want := range []string{"Invoice INV-7 is still unpaid.", "Please pay the invoice. You can view it using the link below."} {
		if !strings.Contains(html, want) || !strings.Contains(text, want) {
			t.Errorf("both parts must say %q\nhtml: %s\ntext: %s", want, html, text)
		}
	}
	if !strings.Contains(text, "View invoice: "+hosted) {
		t.Errorf("text part has no hosted link: %s", text)
	}
	if !strings.Contains(subject, "INV-7") {
		t.Errorf("subject %q does not name the invoice", subject)
	}

	_, html, ctaURL, _ = renderPayInvoiceReminderHTML("Acme", "INV-7", "")
	text = payInvoiceReminderTextBody("Acme", "INV-7", "")
	if ctaURL != "" || !strings.Contains(html, "Please contact us to pay the invoice.") || !strings.Contains(text, "Please contact us to pay the invoice.") {
		t.Errorf("without a hosted link: button %q, html %s, text %s; want no button and a contact-us ask", ctaURL, html, text)
	}
}
