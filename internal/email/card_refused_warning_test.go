package email

import (
	stdhtml "html"
	"strings"
	"testing"
)

// #966: when the bank refused the card for good, dunning retries skip it, so
// the warning asks for a different card and promises no retry date. It does
// not say why (Stripe asks that lost, stolen and fraud declines reach the
// customer as a plain decline). Nor does it say the card won't be charged
// again: Collect payment and the customer's next invoice still charge it
// (review of #1360, round 2).
func TestCardRefusedWarning_AsksForADifferentCardWithoutARetryDate(t *testing.T) {
	subject, rawHTML, ctaURL, ctaLabel := renderCardRefusedWarningHTML("Ada", "INV-0042", "https://pay.example.com/i/tok")
	html := stdhtml.UnescapeString(rawHTML) // the copy is escaped into the markup
	text := cardRefusedWarningTextBody("Ada", "INV-0042", "https://pay.example.com/i/tok")

	if !strings.Contains(subject, "INV-0042") || !strings.Contains(subject, "different card") {
		t.Errorf("subject %q, want the invoice number and \"different card\"", subject)
	}
	for part, body := range map[string]string{"html": html, "text": text} {
		for _, want := range []string{"INV-0042", "Your bank declined the card on file", "different card"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s part does not say %q:\n%s", part, want, body)
			}
		}
		lower := strings.ToLower(body)
		for _, never := range []string{"again", "attempt", "lost", "stolen", "fraud"} {
			if strings.Contains(lower, never) {
				t.Errorf("%s part says %q:\n%s", part, never, body)
			}
		}
	}
	if ctaURL != "https://pay.example.com/i/tok" || ctaLabel != "Pay invoice" {
		t.Errorf("CTA %q %q, want the hosted invoice page labelled Pay invoice", ctaLabel, ctaURL)
	}
	if !strings.Contains(text, "Pay invoice: https://pay.example.com/i/tok") {
		t.Errorf("text part has no pay link:\n%s", text)
	}

	// No hosted page: no button, and the text names no link.
	if _, _, url, label := renderCardRefusedWarningHTML("Ada", "INV-0042", ""); url != "" || label != "" {
		t.Errorf("CTA %q %q with no hosted page, want none", label, url)
	}
	if body := cardRefusedWarningTextBody("Ada", "INV-0042", ""); strings.Contains(body, "Pay invoice:") {
		t.Errorf("text part offers a link with no hosted page:\n%s", body)
	}
}

// Control: the ordinary warning still promises the next retry.
func TestDunningWarning_OrdinaryDeclineStillPromisesTheRetry(t *testing.T) {
	_, html, _, _ := renderDunningWarningHTML("Ada", "INV-0042", 1, 3, "October 21, 2026", "Your card has insufficient funds.", "https://pay.example.com/i/tok")
	text := dunningWarningTextBody("Ada", "INV-0042", 1, 3, "October 21, 2026", "Your card has insufficient funds.", "https://pay.example.com/i/tok")
	for part, body := range map[string]string{"html": html, "text": text} {
		if !strings.Contains(body, "try again on") || !strings.Contains(body, "October 21, 2026") {
			t.Errorf("%s part does not promise the retry date:\n%s", part, body)
		}
	}
}

// The warning goes out for every retry that counted: a card decline, a retry
// with no card on file, and a charge Stripe refused for another reason. Only
// the first is a decline, so the sentence says the attempt didn't go
// through, and the reason line (when there is one) says why (min-charge
// check 2026-10-22, finding 4.1).
func TestDunningWarning_DoesNotCallEveryFailedAttemptADecline(t *testing.T) {
	_, rawHTML, _, _ := renderDunningWarningHTML("Ada", "INV-0042", 1, 3, "October 21, 2026", "", "https://pay.example.com/i/tok")
	html := stdhtml.UnescapeString(rawHTML)
	text := dunningWarningTextBody("Ada", "INV-0042", 1, 3, "October 21, 2026", "", "https://pay.example.com/i/tok")
	for part, body := range map[string]string{"html": html, "text": text} {
		plain := strings.NewReplacer(`<strong style="color:#111827;">`, "", "</strong>", "").Replace(body)
		if !strings.Contains(plain, "Attempt 1 of 3 to charge invoice INV-0042 didn't go through.") {
			t.Errorf("%s part does not say the attempt didn't go through:\n%s", part, body)
		}
		if strings.Contains(strings.ToLower(body), "declined") {
			t.Errorf("%s part calls the attempt a decline:\n%s", part, body)
		}
	}
}

// Review of #1225: the payment-failed email's plain-text part labelled the
// hosted invoice link "Update payment method:", while the HTML button on the
// same link says "Pay invoice". The link opens the page that takes payment,
// and after a hard decline paying there with a different card is what the
// customer is asked to do, so both parts name it the same way.
func TestPaymentFailedText_LabelsTheHostedLinkAsPay(t *testing.T) {
	const hosted = "https://pay.example.com/i/tok"
	_, _, ctaURL, ctaLabel := renderPaymentFailedHTML("Ada", "INV-0042", "Your card was declined.", hosted)
	text := paymentFailedTextBody("Ada", "INV-0042", "Your card was declined.", hosted)
	if ctaURL != hosted || ctaLabel != "Pay invoice" {
		t.Fatalf("HTML CTA %q %q, want Pay invoice on the hosted page", ctaLabel, ctaURL)
	}
	if !strings.Contains(text, ctaLabel+": "+hosted) {
		t.Errorf("text part does not label the hosted link %q:\n%s", ctaLabel, text)
	}
	if strings.Contains(text, "Update payment method") {
		t.Errorf("text part calls the pay link an update link:\n%s", text)
	}
}
