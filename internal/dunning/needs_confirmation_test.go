package dunning

import (
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// The bank asked the customer to confirm the payment (authentication_required).
// Stripe Billing does not retry this code: its schedule and attempt count
// carry on, retries charge only a new payment method, and the customer gets
// a confirmation link and reminders
// (https://docs.stripe.com/billing/revenue-recovery/smart-retries#non-retryable-decline-codes,
// https://docs.stripe.com/billing/revenue-recovery/customer-emails#payment-confirmation-notifications).
// These pin what dunning does with the retrier's reports.

// A retry that skipped the card counts, keeps the schedule, records that it
// charged nothing, and sends the "confirm your payment" reminder with no
// reason line and no retry date.
func TestProcessRun_NeedsConfirmationSkip_CountsAndAsksToConfirm(t *testing.T) {
	h := newHardDeclineHarness(t, &scriptedRetrier{results: []error{domain.ErrCardNeedsConfirmation}})

	run := h.tick()

	if run.AttemptCount != 1 || run.State != domain.DunningActive || run.NextActionAt == nil {
		t.Fatalf("run attempt %d state %s next %v, want 1, active and rescheduled", run.AttemptCount, run.State, run.NextActionAt)
	}
	evt := h.lastRetryEvent()
	if evt.Reason != domain.DunningRetryReasonNeedsConfirmation {
		t.Errorf("retry event reason %q, want %q", evt.Reason, domain.DunningRetryReasonNeedsConfirmation)
	}
	if _, ok := evt.Metadata["payment_intent_id"]; ok {
		t.Errorf("retry event metadata %v names a PaymentIntent, but nothing was charged", evt.Metadata)
	}
	if h.emails.warnings != 1 || h.emails.lastWarnAsk != domain.DeclineAskConfirm || h.emails.lastWarnReason != "" {
		t.Errorf("warnings %d (ask %q, reason %q), want 1 confirm reminder with no reason", h.emails.warnings, h.emails.lastWarnAsk, h.emails.lastWarnReason)
	}
}

// The retry whose own charge drew authentication_required (the card was
// replaced by one whose bank also wants confirmation) is a counted decline
// on the timeline, and its reminder asks to confirm.
func TestProcessRun_AuthenticationRequiredOnRetry_AsksToConfirm(t *testing.T) {
	h := newHardDeclineHarness(t, &scriptedRetrier{results: []error{
		&RetryError{PaymentIntentID: "pi_3ds", Err: errors.New("payment failed: Your card was declined. This transaction requires authentication."), Ask: domain.DeclineAskConfirm},
	}})

	h.tick()

	if pi, _ := h.lastRetryEvent().Metadata["payment_intent_id"].(string); pi != "pi_3ds" {
		t.Errorf("retry event PaymentIntent %q, want pi_3ds", pi)
	}
	if h.emails.warnings != 1 || h.emails.lastWarnAsk != domain.DeclineAskConfirm || h.emails.lastWarnReason != "" {
		t.Errorf("warnings %d (ask %q, reason %q), want 1 confirm reminder with no reason", h.emails.warnings, h.emails.lastWarnAsk, h.emails.lastWarnReason)
	}
}

// A customer who never confirms does not hold the run open: each scheduled
// retry counts, a reminder goes out for each but the last, and the policy's
// final step runs when the attempts run out, as Stripe Billing's does.
func TestProcessRun_NeverConfirmed_StillExhausts(t *testing.T) {
	h := newHardDeclineHarness(t, &scriptedRetrier{results: []error{
		domain.ErrCardNeedsConfirmation, domain.ErrCardNeedsConfirmation, domain.ErrCardNeedsConfirmation,
	}})

	h.tick()
	h.tick()
	run := h.tick()

	if run.State != domain.DunningEscalated || run.Resolution != domain.ResolutionRetriesExhausted || run.AttemptCount != 3 {
		t.Fatalf("run %s/%s after %d attempts, want escalated/retries_exhausted after 3", run.State, run.Resolution, run.AttemptCount)
	}
	if h.emails.warnings != 2 || h.emails.lastWarnAsk != domain.DeclineAskConfirm || h.emails.escalations != 1 {
		t.Errorf("warnings %d (last ask %q), escalations %d; want 2 confirm reminders and 1 escalation", h.emails.warnings, h.emails.lastWarnAsk, h.emails.escalations)
	}
}
