package errs

import "regexp"

// Scrub removes personal data (PII) from free-text error messages before
// they are stored (invoices.last_payment_error,
// stripe_webhook_events.failure_message) or logged. Scrubbing where the text
// comes in makes every later surface safe (database columns, slog fields,
// HTTP error bodies), without each call site having to remember.
//
// What it redacts, and why:
//
//   - A card's last 4 digits in free text ("ending in 4242"). The last4 is
//     stored as a field on customer_payment_setups. Left inline in error
//     text, it turns every error row into a second copy, outside the
//     encryption-at-rest path and outside the operator's access-control
//     model.
//   - Raw runs of 13 to 19 digits that look like a card number, for PCI.
//     They should never reach Hardcount, and a second safeguard costs
//     nothing.
//   - Email addresses, which are PII. Emails are stored on customers and
//     billing_profiles, where access is audited. An email in an error
//     string bypasses that.
//
// What it does not redact, on purpose:
//
//   - Stripe object IDs (pi_*, cus_*, pm_*, ch_*, seti_*, in_*). These are
//     opaque correlation identifiers, not PII. Operators need them to trace
//     an incident in the Stripe dashboard, and Stripe itself logs them in
//     every webhook and API response.
//   - Decline codes ("card_declined", "insufficient_funds"). They are
//     machine-readable, carry no PII and are critical for triage.
//
// Scrub is idempotent: Scrub(Scrub(s)) == Scrub(s).
func Scrub(s string) string {
	if s == "" {
		return s
	}
	s = reCardLast4.ReplaceAllString(s, "$1****")
	s = reCardNumber.ReplaceAllString(s, "[REDACTED_CARD]")
	s = reEmail.ReplaceAllString(s, "[REDACTED_EMAIL]")
	return s
}

// reCardLast4 matches "ending in 4242", "ending 4242" or "card 4242". The
// digits are captured apart from the phrase, so the phrase stays readable
// for the operator. It is case-insensitive, and the prefix is kept verbatim.
var reCardLast4 = regexp.MustCompile(`(?i)((?:ending(?:\s+in)?|card)\s+)\d{4}\b`)

// reCardNumber matches a raw run of 13 to 19 digits that is not part of a
// longer digit run. It is bounded by a non-digit (or the start or end), so
// it does not eat Stripe IDs that happen to contain digits (pi_3M… and so
// on). The underscore after the prefix already keeps this regex from
// matching those IDs, and the word boundary is a second safeguard.
var reCardNumber = regexp.MustCompile(`\b\d{13,19}\b`)

// reEmail matches addresses shaped roughly like RFC 5322. It is simple on
// purpose: it redacts and does not parse for delivery, so matching too much
// is better than leaving anything behind. It is anchored by non-word
// boundaries, so "x@y.z" inside a sentence is still caught.
var reEmail = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
