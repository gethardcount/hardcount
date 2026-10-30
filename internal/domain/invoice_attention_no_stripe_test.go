package domain

import (
	"strings"
	"testing"
)

// TestNoPaymentMethodCopy_NoStripe: while Stripe is not connected for the
// invoice's mode no card can be added online, so the collector emails the
// customer the invoice instead of a setup link (ADR-060 amendment
// 2026-10-30). The notice says so, says what happened to the invoice email,
// and offers no Resend setup link (there is no link); the customer page stays.
// Once recovery has ended it says retries stopped. Mutation: drop the
// !StripeConnected branch (the notice promises a charge once a card is added
// and offers Resend setup link).
func TestNoPaymentMethodCopy_NoStripe(t *testing.T) {
	const (
		line1   = "Stripe isn't connected in this mode, so no card can be added online."
		sent    = "The invoice is emailed to the customer once."
		noEmail = "The invoice is emailed only to customers with an email address; add one on the customer page."
		off     = "This server can't send email, so the invoice was not emailed to the customer."
	)
	cases := []struct {
		name                      string
		simulated, pause          bool
		hasEmail, emailOff, ended bool
		reason                    AttentionReason
		message                   string
	}{
		{"email", false, false, true, false, false, AttentionReasonNoPaymentMethod, line1 + "\n" + sent},
		{"sim, email", true, false, true, false, false, AttentionReasonNoPaymentMethod, line1 + "\n" + sent},
		{"no email", false, false, false, false, false, AttentionReasonNoPaymentMethod, line1 + "\n" + noEmail},
		{"email off", false, false, true, true, false, AttentionReasonNoPaymentMethod, line1 + "\n" + off},
		{"no email and email off", false, false, false, true, false, AttentionReasonNoPaymentMethod, line1 + "\n" + noEmail},
		{"paused, email", false, true, true, false, false, AttentionReasonNoPaymentMethod, line1 + "\n" + sent},
		{"recovery ended", false, false, true, false, true, AttentionReasonDunningExhausted, "Retries stopped; " + line1 + "\n" + sent},
		{"recovery ended, paused", false, true, true, false, true, AttentionReasonDunningExhausted, "Retries stopped and collection is paused; " + line1 + "\n" + sent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			att := ClassifyInvoiceAttention(noPMInvoice(tc.simulated), AttentionContext{
				StripeConnected: false, CustomerHasEmail: tc.hasEmail, InvoiceEmailOff: tc.emailOff,
				CollectionPaused: tc.pause, DunningEscalated: tc.ended,
			})
			if att == nil || att.Reason != tc.reason {
				t.Fatalf("got %+v, want %s", att, tc.reason)
			}
			if att.Message != tc.message {
				t.Errorf("message =\n%q\nwant\n%q", att.Message, tc.message)
			}
			checkShape(t, att.Message)
			for _, banned := range []string{"setup link", "charged automatically", "adds a card"} {
				if strings.Contains(att.Message, banned) || strings.Contains(att.Detail, banned) {
					t.Errorf("notice mentions %q without Stripe:\n%s\n%s", banned, att.Message, att.Detail)
				}
			}
			if !strings.Contains(att.Detail, "connect Stripe under Settings") {
				t.Errorf("detail %q does not say how to collect by card", att.Detail)
			}
			if got := actionCodes(att); !sameActions(got, AttentionActionAddPaymentMethod) {
				t.Errorf("actions = %v, want only Open customer page", got)
			}
		})
	}

	// With Stripe connected the setup-link copy is unchanged.
	att := ClassifyInvoiceAttention(noPMInvoice(false), AttentionContext{StripeConnected: true, CustomerHasEmail: true})
	if att == nil || !strings.Contains(att.Message, "A setup link is emailed to the customer once") {
		t.Fatalf("connected: got %+v, want the setup-link copy", att)
	}
}
