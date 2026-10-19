package activity

import "strings"

// Email names. Each is a noun, so a title reads as one event with its
// outcome word after it: "Failed payment notice delivered", never "Payment
// failed email failed".
var emailNames = map[string]string{
	"invoice":               "Invoice email",
	"payment_receipt":       "Receipt email",
	"payment_failed":        "Failed payment notice",
	"payment_setup_request": "Payment setup email",
	"credit_note":           "Credit note email",
	"dunning_warning":       "Payment reminder email",
	"dunning_escalation":    "Final notice",
}

// EmailName returns the name of an email type, and false for a type no
// activity surface shows.
func EmailName(emailType string) (string, bool) {
	n, ok := emailNames[emailType]
	return n, ok
}

// EmailTypes lists the email types that have a name, for tests that range
// over every one.
func EmailTypes() []string {
	out := make([]string, 0, len(emailNames))
	for t := range emailNames {
		out = append(out, t)
	}
	return out
}

// Email send failures the outcome can name in plain words. The email
// adapter classifies the outbox row's error into one of these.
const (
	SendFailureSMTPNotConfigured   = "smtp_not_configured"
	SendFailureRecipientSuppressed = "recipient_suppressed"
	SendFailureInvalidRecipient    = "invalid_recipient"
)

// EmailOutcome is what happened to one email, in the words every surface
// uses: queued, sent, delivered, bounced, marked as spam, couldn't be sent,
// not sent.
type EmailOutcome struct {
	// Word goes at the end of the title, after the email's name.
	Word string
	Tone Tone
	// Problem is set on failure outcomes whose title does not say it all.
	Problem string
	// Reason is set on "not sent", which is neutral, so its reason goes in
	// the meta line.
	Reason string
}

// Outcome maps an outbox row's status and the provider's delivery verdict
// (ADR-098) to its outcome. failure is the classified send error of a failed
// row (one of the SendFailure constants, or empty).
func Outcome(status, deliveryState, failure string) EmailOutcome {
	switch status {
	case "pending":
		return EmailOutcome{Word: "queued", Tone: ToneNeutral}
	case "failed":
		return EmailOutcome{Word: "couldn't be sent", Tone: ToneFailure, Problem: sendFailureProblem(failure)}
	case "skipped":
		// The dispatcher skips an action-required email whose invoice reached
		// a terminal state while the row sat queued. The state it reached is
		// kept only in the row's error text today, so this says what it can
		// know.
		return EmailOutcome{Word: "not sent", Tone: ToneNeutral, Reason: "The invoice was already closed"}
	}
	switch deliveryState {
	case "delivered":
		return EmailOutcome{Word: "delivered", Tone: ToneNeutral}
	case "bounced":
		return EmailOutcome{Word: "bounced", Tone: ToneFailure, Problem: "The customer's mail server rejected it."}
	case "complained":
		return EmailOutcome{Word: "marked as spam", Tone: ToneFailure}
	}
	return EmailOutcome{Word: "sent", Tone: ToneNeutral}
}

func sendFailureProblem(failure string) string {
	switch failure {
	case SendFailureSMTPNotConfigured:
		return "Email sending is not set up on this server."
	case SendFailureRecipientSuppressed:
		return "This address bounced or marked an earlier email as spam, so Velox no longer emails it."
	case SendFailureInvalidRecipient:
		return "The customer's email address is not valid."
	}
	return "The customer did not get this email."
}

// EmailClause is an email's one appearance as a clause in another row's meta
// line, for the payment-recovery emails that fold into the retry and
// retries-ran-out rows: "Payment reminder email delivered".
func EmailClause(emailType, status, deliveryState, failure string) string {
	name, ok := EmailName(emailType)
	if !ok {
		return ""
	}
	return name + " " + Outcome(status, deliveryState, failure).Word
}

// Credit-note reasons the engine and the refund form write as codes. Any
// other reason is the operator's own text and is shown as written.
var creditNoteReasons = map[string]string{
	"subscription_downgrade":         "Plan downgrade",
	"subscription_quantity_decrease": "Quantity decrease",
	"subscription_item_removed":      "Item removed",
	"subscription_cancellation":      "Cancellation",
	"subscription_plan_change":       "Plan change",
	"commit_relief":                  "Unused prepaid credits refunded",
	// The refund form's reasons (Stripe's refund reasons, plus other).
	"duplicate":             "Duplicate",
	"fraudulent":            "Fraudulent",
	"requested_by_customer": "Requested by customer",
	"other":                 "Other",
}

// CreditNoteReason labels a credit note's reason.
func CreditNoteReason(reason string) string {
	if l, ok := creditNoteReasons[reason]; ok {
		return l
	}
	return reason
}

// CreditNoteReasonCodes lists the coded reasons, for tests.
func CreditNoteReasonCodes() []string {
	out := make([]string, 0, len(creditNoteReasons))
	for c := range creditNoteReasons {
		out = append(out, c)
	}
	return out
}

// RefundParts says where a credit note's total went: each part that is not
// zero, as a meta segment.
func RefundParts(refundCents, creditCents, outOfBandCents int64, currency string) []Segment {
	var out []Segment
	if refundCents > 0 {
		out = append(out, Segment{M(refundCents, currency), T(" refunded to card")})
	}
	if creditCents > 0 {
		out = append(out, Segment{M(creditCents, currency), T(" to credit balance")})
	}
	if outOfBandCents > 0 {
		out = append(out, Segment{M(outOfBandCents, currency), T(" refunded outside Velox")})
	}
	return out
}

// Decline reasons, written about the cardholder, not to them: "Card
// declined: insufficient funds", not the provider's "Your card has
// insufficient funds." The provider's "your" means the operator on the
// operator's screen.
var declineReasons = map[string]string{
	"insufficient_funds": "Card declined: insufficient funds",
	"expired_card":       "Card declined: expired card",
	"incorrect_cvc":      "Card declined: incorrect security code",
	"invalid_cvc":        "Card declined: incorrect security code",
	"lost_card":          "Card declined: reported lost or stolen",
	"stolen_card":        "Card declined: reported lost or stolen",
	"generic_decline":    "Card declined by the bank",
	"card_declined":      "Card declined by the bank",
	"do_not_honor":       "Card declined by the bank",
}

// DeclineCodes lists the mapped decline codes, for tests.
func DeclineCodes() []string {
	out := make([]string, 0, len(declineReasons))
	for c := range declineReasons {
		out = append(out, c)
	}
	return out
}

// DeclineReason is a failed charge's problem line: the mapped decline code
// when there is one, else the provider's own text without Velox's wrappers.
func DeclineReason(declineCode, providerText string) string {
	if r, ok := declineReasons[strings.ToLower(strings.TrimSpace(declineCode))]; ok {
		return r
	}
	return StripWrapper(providerText)
}

// wrappers are the prefixes the charge path puts in front of the provider's
// message (payment.Stripe's "%s: %w" with its verb), which the dunning retry
// event stores as its reason.
var wrappers = []string{"payment failed: ", "payment state unknown: "}

// StripWrapper removes Velox's own wrapper from a provider's message
// ("payment failed: Your card was declined." → "Your card was declined."),
// so only the provider's words reach the screen.
func StripWrapper(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range wrappers {
		if len(s) > len(p) && strings.EqualFold(s[:len(p)], p) {
			return strings.TrimSpace(s[len(p):])
		}
	}
	return s
}
