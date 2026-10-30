package dunning

import (
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
)

// Without Stripe for the invoice's mode the retrier charges nothing and
// reports ErrNoOnlinePaymentOnRetry (ADR-060 amendment 2026-10-30). Dunning
// counts it like a no-card retry (same timeline reason, same schedule) and
// sends the pay-the-invoice reminder: no card ask, no reason line. The run
// still ends in the policy's final action. Mutation: drop the
// ErrNoOnlinePaymentOnRetry case from retryAsk (the reminder asks for a card).
func TestProcessRun_NoStripe_RemindsToPayAndStillExhausts(t *testing.T) {
	h := newHardDeclineHarness(t, &scriptedRetrier{results: []error{
		domain.ErrNoOnlinePaymentOnRetry, domain.ErrNoOnlinePaymentOnRetry, domain.ErrNoOnlinePaymentOnRetry,
	}})

	run := h.tick()
	if run.AttemptCount != 1 || run.State != domain.DunningActive || run.NextActionAt == nil {
		t.Fatalf("run attempt %d state %s next %v, want 1, active and rescheduled", run.AttemptCount, run.State, run.NextActionAt)
	}
	if evt := h.lastRetryEvent(); evt.Reason != string(domain.DunningCauseNoPaymentMethod) {
		t.Errorf("retry event reason %q, want %q (a retry that charged nothing)", evt.Reason, domain.DunningCauseNoPaymentMethod)
	}
	if h.emails.warnings != 1 || h.emails.lastWarnAsk != domain.DeclineAskPayInvoice || h.emails.lastWarnReason != "" {
		t.Errorf("warnings %d (ask %q, reason %q), want 1 pay-the-invoice reminder with no reason",
			h.emails.warnings, h.emails.lastWarnAsk, h.emails.lastWarnReason)
	}

	h.tick()
	run = h.tick()
	if run.State != domain.DunningEscalated || run.Resolution != domain.ResolutionRetriesExhausted || run.AttemptCount != 3 {
		t.Fatalf("run %s/%s after %d attempts, want escalated/retries_exhausted after 3", run.State, run.Resolution, run.AttemptCount)
	}
	if h.emails.warnings != 2 || h.emails.lastWarnAsk != domain.DeclineAskPayInvoice || h.emails.escalations != 1 {
		t.Errorf("warnings %d (last ask %q), escalations %d; want 2 pay-the-invoice reminders and 1 escalation",
			h.emails.warnings, h.emails.lastWarnAsk, h.emails.escalations)
	}
}

// With Stripe connected and no card the reminder still asks for a card.
func TestRetryAsk_NoStripeVersusNoCard(t *testing.T) {
	if got := retryAsk(domain.ErrNoOnlinePaymentOnRetry); got != domain.DeclineAskPayInvoice {
		t.Errorf("no Stripe: ask %q, want %q", got, domain.DeclineAskPayInvoice)
	}
	if got := retryAsk(domain.ErrNoPaymentMethodOnRetry); got != domain.DeclineAskAddCard {
		t.Errorf("no card: ask %q, want %q", got, domain.DeclineAskAddCard)
	}
}
