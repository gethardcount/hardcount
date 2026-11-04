package activity

import (
	"strings"
	"time"
)

// This file is the one email vocabulary. Every surface that shows an email
// builds its line here, from the outbox row. Those surfaces are the invoice
// Activity card and the customer's Sent emails card. Building every line here
// makes an email read the same wherever it appears: its name, one outcome
// word, why it was not sent, and who sent it.

// emailNames maps an email type to its name. Each name is a noun, so a title
// reads as one event with its outcome word after it: "Failed payment notice
// delivered", never "Payment failed email failed".
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

// The SendFailure constants are the email send failures that an outcome can
// name in plain words. The email adapter classifies an outbox row's error
// into one of them.
const (
	SendFailureSMTPNotConfigured   = "smtp_not_configured"
	SendFailureRecipientSuppressed = "recipient_suppressed"
	SendFailureInvalidRecipient    = "invalid_recipient"
)

// skipReasons says why Hardcount did not send an email. The reason is the
// state the invoice reached while the email sat queued, as the outbox row
// records it (email_outbox.skip_reason).
var skipReasons = map[string]string{
	"paid":          "The invoice was already paid",
	"voided":        "The invoice was already voided",
	"uncollectible": "The invoice was already marked uncollectible",
	// Test-clock teardown deletes a clock's invoices.
	"gone": "The invoice no longer exists",
}

// skippedBeforeTheReason is the reason for a row skipped before the outbox
// recorded which state the invoice reached. All such a row can say is that
// the invoice was in a final state.
const skippedBeforeTheReason = "The invoice was already closed"

// SkipReasonWords returns the words for a recorded skip reason, and false
// for a reason with no words of its own.
func SkipReasonWords(reason string) (string, bool) {
	w, ok := skipReasons[reason]
	return w, ok
}

// Email is one email_outbox row, as its activity line needs it.
type Email struct {
	// Type is the row's email_type.
	Type string
	// Status is the outbox status: pending, dispatched, failed or skipped.
	Status string
	// DeliveryState is what the email provider reported back (ADR-098).
	DeliveryState string
	// SendFailure is a failed row's classified send error (one of the
	// SendFailure constants), or empty.
	SendFailure string
	// SkipReason is the state the invoice reached that made Hardcount skip a
	// queued email, or empty for a row skipped before it was recorded.
	SkipReason string
	// Trigger is why a payment setup email was sent (its payload's trigger).
	Trigger string
	// To is the recipient.
	To string
	// CreatedAt is when the row was queued, and DispatchedAt is when the mail
	// server took it. Both are real-world instants.
	CreatedAt    time.Time
	DispatchedAt *time.Time
	// SimEffectiveAt is the instant on the entity's test clock that caused
	// the email (ADR-104), or nil for an email not caused on a test clock.
	SimEffectiveAt *time.Time
}

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

// Outcome maps an outbox row's status, the provider's delivery verdict
// (ADR-098), and the classified send failure or recorded skip reason to its
// outcome.
func Outcome(e Email) EmailOutcome {
	switch e.Status {
	case "pending":
		return EmailOutcome{Word: "queued", Tone: ToneNeutral}
	case "failed":
		return EmailOutcome{Word: "couldn't be sent", Tone: ToneFailure, Problem: sendFailureProblem(e.SendFailure)}
	case "skipped":
		// The dispatcher skips an action-required email whose invoice reached
		// a final state while the row sat queued.
		reason, ok := SkipReasonWords(e.SkipReason)
		if !ok {
			reason = skippedBeforeTheReason
		}
		return EmailOutcome{Word: "not sent", Tone: ToneNeutral, Reason: reason}
	}
	switch e.DeliveryState {
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
		return "This address bounced or marked an earlier email as spam, so Hardcount no longer emails it."
	case SendFailureInvalidRecipient:
		return "The customer's email address is not valid."
	}
	return "The customer did not get this email."
}

// EmailCause returns a payment setup email's cause, from the trigger it was
// queued with. It returns "" for a trigger with no words.
func EmailCause(trigger string) string {
	switch trigger {
	case "finalize_no_pm":
		return "No payment method when the invoice was finalized"
	case "auto_charge_retry_no_pm":
		return "A charge found no payment method"
	case "operator_resend":
		// The row does not record who resent it yet. Once it does, the actor
		// replaces this word.
		return "Resent"
	}
	return ""
}

// emailActor names who sent an email when the email itself proves it. Only
// Hardcount sends receipts, failed-payment notices and payment-recovery
// emails, and a setup email whose trigger is automatic. An invoice or
// credit-note email, or a resend, is an operator's act, and the row does not
// record that actor yet.
func emailActor(emailType, trigger string) *Actor {
	switch emailType {
	case "payment_receipt", "payment_failed", "dunning_warning", "dunning_escalation":
		return Automatic()
	case "payment_setup_request":
		if trigger == "finalize_no_pm" || trigger == "auto_charge_retry_no_pm" {
			return Automatic()
		}
	}
	return nil
}

// EmailLine returns an email's line. The title is its name and outcome word.
// The meta line holds, in order: the reason it was not sent, a setup email's
// cause, the objects it is about, and who it went to. about holds links to
// the invoice or credit note the email names, when those are not the page's
// own entity. ok is false for an email type that no activity surface shows.
func EmailLine(e Email, about ...Segment) (Line, bool) {
	name, ok := EmailName(e.Type)
	if !ok {
		return Line{}, false
	}
	out := Outcome(e)
	l := Line{
		Kind: "email." + e.Type, Title: name + " " + out.Word, Tone: out.Tone, Problem: out.Problem,
		Actor: emailActor(e.Type, e.Trigger),
	}
	if out.Reason != "" {
		l.Detail = append(l.Detail, Text(out.Reason))
	}
	if e.Type == "payment_setup_request" {
		if c := EmailCause(e.Trigger); c != "" {
			l.Detail = append(l.Detail, Text(c))
		}
	}
	for _, s := range about {
		if len(s) > 0 {
			l.Detail = append(l.Detail, s)
		}
	}
	if to := strings.TrimSpace(e.To); to != "" {
		l.Detail = append(l.Detail, Text("To "+to))
	}
	return l, true
}

// EmailClause returns an email's one appearance, as a clause in another row's
// meta line: "Payment reminder email delivered". It is for the
// payment-recovery emails that fold into the retry and retries-ran-out rows.
func EmailClause(e Email) string {
	name, ok := EmailName(e.Type)
	if !ok {
		return ""
	}
	return name + " " + Outcome(e).Word
}

// EmailInstant places an email on the entity's calendar (ADR-104). The
// instant is the one on the entity's test clock that caused the send. The
// real send time is its Recorded time: when the mail server took it, or else
// when it was queued. An email not caused on a test clock is dated at its
// real send time and is not pinned.
func EmailInstant(e Email) (occurred, recorded time.Time, pinned bool) {
	wall := e.CreatedAt
	if e.DispatchedAt != nil {
		wall = *e.DispatchedAt
	}
	if e.SimEffectiveAt != nil {
		return *e.SimEffectiveAt, wall, true
	}
	return wall, time.Time{}, false
}
