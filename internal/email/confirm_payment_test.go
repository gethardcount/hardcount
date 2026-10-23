package email

import (
	"context"
	stdhtml "html"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
)

// P4 of the Stripe card-flow review (2026-10-22): when the bank asks the
// customer to confirm an off-session payment (authentication_required), the
// customer got the generic "We couldn't process payment". The email says
// their bank asked them to confirm the payment, gives the pay link (the
// hosted page runs 3-D Secure), and says nothing about a decline or a retry
// date: no charge made without the customer can pass the bank's check.
func TestConfirmPaymentEmail_AsksToConfirmWithThePayLink(t *testing.T) {
	const hosted = "https://pay.example.com/i/tok"
	subject, rawHTML, ctaURL, ctaLabel := renderConfirmPaymentHTML("Ada", "INV-0042", hosted)
	html := stdhtml.UnescapeString(rawHTML)
	text := confirmPaymentTextBody("Ada", "INV-0042", hosted)

	if !strings.Contains(subject, "INV-0042") || !strings.Contains(subject, "confirm your payment") {
		t.Errorf("subject %q, want the invoice number and \"confirm your payment\"", subject)
	}
	for part, body := range map[string]string{"html": html, "text": text} {
		// "may ask": 3-D Secure on the hosted page can pass without a
		// challenge, and a customer paying with another card may not be
		// asked (review of #1360, round 2).
		for _, want := range []string{"INV-0042", "Your bank asked you to confirm the payment", "pay the invoice", "Your bank may ask you to confirm"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s part does not say %q:\n%s", part, want, body)
			}
		}
		lower := strings.ToLower(body)
		for _, never := range []string{"declin", "try again", "attempt", "unsuccessful", "off session", "authentication", "will ask"} {
			if strings.Contains(lower, never) {
				t.Errorf("%s part says %q:\n%s", part, never, body)
			}
		}
	}
	if ctaURL != hosted || ctaLabel != "Pay invoice" {
		t.Errorf("CTA %q %q, want Pay invoice on the hosted page", ctaLabel, ctaURL)
	}
	if !strings.Contains(text, "Pay invoice: "+hosted) {
		t.Errorf("text part has no pay link:\n%s", text)
	}

	// No hosted page: no button, no link, and the copy does not point at one.
	// Nor does it say the bank will ask them to confirm: a customer who pays
	// some other way (a bank transfer) is not asked.
	_, rawNoLink, url, label := renderConfirmPaymentHTML("Ada", "INV-0042", "")
	if url != "" || label != "" {
		t.Errorf("CTA %q %q with no hosted page, want none", label, url)
	}
	for part, body := range map[string]string{"html": stdhtml.UnescapeString(rawNoLink), "text": confirmPaymentTextBody("Ada", "INV-0042", "")} {
		if strings.Contains(body, "link below") || strings.Contains(body, "Pay invoice:") {
			t.Errorf("%s part points at a link with no hosted page:\n%s", part, body)
		}
		if !strings.Contains(body, "Please contact us to pay the invoice.") || strings.Contains(body, "will ask you") {
			t.Errorf("%s part with no hosted page, want only \"Please contact us to pay the invoice.\":\n%s", part, body)
		}
	}
}

// askDeliverer records the copy each send asked for.
type askDeliverer struct {
	recordingDeliverer
	reasons []string
	asks    []domain.DeclineAsk
}

func (a *askDeliverer) SendDunningWarning(_ context.Context, _, _ string, _ []string, _, _ string, _, _ int, _, reason, _ string, ask domain.DeclineAsk) error {
	a.reasons, a.asks = append(a.reasons, reason), append(a.asks, ask)
	return nil
}

func (a *askDeliverer) SendPaymentFailed(_ context.Context, _, _ string, _ []string, _, _, reason, _ string, ask domain.DeclineAsk) error {
	a.reasons, a.asks = append(a.reasons, reason), append(a.asks, ask)
	return nil
}

// The dispatcher sends a queued row with the copy and the reason it was
// queued with: decline_ask, or for a row an older binary queued,
// card_needs_replacing. A payment-failed email's reason is read from reason,
// where the settle transaction writes the customer's reason. The previous
// release wrote failure_reason there, unfiltered (Velox's own text, such as
// "reconciled: requires_payment_method"), and it is not shown.
func TestDispatcher_SendsTheQueuedCopyAndReason(t *testing.T) {
	cases := []struct {
		name       string
		typ        string
		payload    map[string]any
		wantAsk    domain.DeclineAsk
		wantReason string
	}{
		{"warning, confirm", TypeDunningWarning, map[string]any{"decline_ask": "confirm"}, domain.DeclineAskConfirm, ""},
		{"warning, new card", TypeDunningWarning, map[string]any{"decline_ask": "new_card", "card_needs_replacing": true}, domain.DeclineAskNewCard, ""},
		{"warning queued by the older binary", TypeDunningWarning, map[string]any{"card_needs_replacing": true}, domain.DeclineAskNewCard, ""},
		{"warning, ordinary", TypeDunningWarning, map[string]any{"failure_reason": "Your card has insufficient funds."}, domain.DeclineAskNone, "Your card has insufficient funds."},
		{"payment failed, confirm", TypePaymentFailed, map[string]any{"decline_ask": "confirm"}, domain.DeclineAskConfirm, ""},
		{"payment failed, settle transaction", TypePaymentFailed, map[string]any{"reason": "Your card was declined."}, domain.DeclineAskNone, "Your card was declined."},
		{"payment failed, queued by the previous release", TypePaymentFailed, map[string]any{"failure_reason": "reconciled: requires_payment_method"}, domain.DeclineAskNone, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &askDeliverer{}
			d := NewDispatcher(nil, sender, DispatcherConfig{}, leader.AlwaysLead{})
			d.SetSettledChecker(&recordingChecker{state: ""})
			row := rowOf(tc.typ)
			for k, v := range tc.payload {
				row.Payload[k] = v
			}
			if err := d.handle(context.Background(), row); err != nil {
				t.Fatalf("handle: %v", err)
			}
			if len(sender.asks) != 1 || sender.asks[0] != tc.wantAsk || sender.reasons[0] != tc.wantReason {
				t.Errorf("sent asks %q reasons %q, want %q / %q", sender.asks, sender.reasons, tc.wantAsk, tc.wantReason)
			}
		})
	}
}
