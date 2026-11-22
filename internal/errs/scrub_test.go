package errs

import "testing"

// Scrub redacts card digits and email addresses, and keeps Stripe IDs and
// decline codes.
func TestScrub_RedactsCardDigitsAndEmails(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "clean message unchanged", in: "your card was declined", want: "your card was declined"},

		// Card last4 in free text
		{name: "ending in 4242", in: "Card ending in 4242 was declined.", want: "Card ending in **** was declined."},
		{name: "ending 4242 no 'in'", in: "Ending 4242 was declined.", want: "Ending **** was declined."},
		{name: "card 4242", in: "Card 4242 declined.", want: "Card **** declined."},
		{name: "mixed case", in: "ENDING IN 4242", want: "ENDING IN ****"},

		// Raw card numbers (PCI)
		{name: "16-digit card", in: "card 4242424242424242 was rejected", want: "card [REDACTED_CARD] was rejected"},
		{name: "15-digit amex", in: "card 378282246310005 declined", want: "card [REDACTED_CARD] declined"},
		{name: "short digit run untouched", in: "attempt 42 failed", want: "attempt 42 failed"},
		{name: "20-digit run untouched", in: "id 12345678901234567890", want: "id 12345678901234567890"},

		// Emails
		{name: "plain email", in: "customer alice@example.com was declined", want: "customer [REDACTED_EMAIL] was declined"},
		{name: "email with plus", in: "bob+test@corp.co.uk failed", want: "[REDACTED_EMAIL] failed"},
		{name: "email in parens", in: "payment for (user@example.com)", want: "payment for ([REDACTED_EMAIL])"},

		// Stripe IDs must survive — they're correlation, not PII
		{name: "PI id kept", in: "PaymentIntent pi_3MqL8B2eZvKYlo2C0abc failed", want: "PaymentIntent pi_3MqL8B2eZvKYlo2C0abc failed"},
		{name: "customer id kept", in: "customer cus_Nabc123xyz has no card", want: "customer cus_Nabc123xyz has no card"},
		{name: "payment method id kept", in: "pm_1234567890abcdef invalid", want: "pm_1234567890abcdef invalid"},

		// Decline codes stay
		{name: "decline code kept", in: "insufficient_funds: try another card", want: "insufficient_funds: try another card"},

		// Combinations
		{
			name: "multiple PII in one message",
			in:   "customer alice@example.com's card ending in 4242 was declined",
			want: "customer [REDACTED_EMAIL]'s card ending in **** was declined",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Scrub(tc.in)
			if got != tc.want {
				t.Errorf("Scrub(%q)\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestScrub_Idempotent(t *testing.T) {
	in := "card ending in 4242; contact alice@example.com; raw 4242424242424242"
	once := Scrub(in)
	twice := Scrub(once)
	if once != twice {
		t.Errorf("Scrub is not idempotent:\n  first:  %q\n  second: %q", once, twice)
	}
}
