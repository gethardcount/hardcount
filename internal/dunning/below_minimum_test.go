package dunning

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// belowMinimum is the adapter's report of a retry Stripe refused because the
// amount is below its minimum charge (payment.BelowMinimumError).
func belowMinimum() error {
	return fmt.Errorf("below Stripe's minimum charge: Amount must be at least $0.50 usd: %w", domain.ErrBelowMinimumCharge)
}

type subInvoiceGetter struct{}

func (subInvoiceGetter) Get(_ context.Context, _, id string) (domain.Invoice, error) {
	return domain.Invoice{ID: id, SubscriptionID: "sub_1", Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending, AmountDueCents: 30}, nil
}

// A retry Stripe refuses as below its minimum ends the run there: resolved
// with amount_below_minimum at the retry's instant, no warning email, no
// escalation, and, on what would have been the last retry, no final action
// (the default pauses the subscription). Before, it counted as a decline: a
// warning ("was declined. Reason: payment failed: Amount must be at least
// $0.50 usd"), then the final action over an amount no card can pay.
func TestProcessRun_BelowMinimum_EndsTheRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attempts int // already used before this tick; policy max is 3
	}{
		{"first retry", 0},
		{"last retry", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHardDeclineHarness(t, &scriptedRetrier{results: []error{belowMinimum()}})
			twoAxisPolicy(h.store, domain.SubActionPause, domain.InvActionNone)
			pauser := &recordingPauser{}
			h.svc.SetSubscriptionPauser(pauser, subInvoiceGetter{})
			run := h.store.runs[h.runID]
			run.AttemptCount = tc.attempts
			h.store.runs[h.runID] = run

			got := h.tick()

			if got.State != domain.DunningResolved || got.Resolution != domain.ResolutionAmountBelowMinimum {
				t.Fatalf("run %s/%s, want resolved/amount_below_minimum", got.State, got.Resolution)
			}
			if got.AttemptCount != tc.attempts+1 {
				t.Errorf("attempt_count %d, want %d: Stripe was asked, so the attempt counts", got.AttemptCount, tc.attempts+1)
			}
			if got.ResolvedAt == nil || got.LastAttemptAt == nil || !got.ResolvedAt.Equal(*got.LastAttemptAt) {
				t.Errorf("resolved_at %v, last_attempt_at %v: want the retry's own instant", got.ResolvedAt, got.LastAttemptAt)
			}
			if h.emails.warnings != 0 || h.emails.escalations != 0 {
				t.Errorf("warnings %d escalations %d, want none", h.emails.warnings, h.emails.escalations)
			}
			if pauser.calls != 0 {
				t.Errorf("final action paused the subscription %d times, want never", pauser.calls)
			}
			if evt := h.lastRetryEvent(); evt.Reason != domain.DunningRetryReasonBelowMinimum {
				t.Errorf("retry event reason %q, want %q", evt.Reason, domain.DunningRetryReasonBelowMinimum)
			}
		})
	}
}

// stampableInvoiceGetter is the invoice as dunning reads it: open, with the
// below-minimum stamp once stamped is set. The stamp is the durable record
// of Stripe's refusal (invoices.below_minimum_at); the resolve that should
// follow it is a separate call that can be lost.
type stampableInvoiceGetter struct{ stamped bool }

func (g *stampableInvoiceGetter) Get(_ context.Context, _, id string) (domain.Invoice, error) {
	inv := domain.Invoice{ID: id, SubscriptionID: "sub_1", Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending, AmountDueCents: 30}
	if g.stamped {
		at := time.Now().UTC()
		inv.BelowMinimumAt = &at
	}
	return inv, nil
}

// stampingRetrier is a card-less retry (no charge, ErrNoPaymentMethodOnRetry)
// during which Stripe's below-minimum refusal of a hosted Pay click is
// stamped on the invoice and its resolve is lost.
type stampingRetrier struct {
	getter *stampableInvoiceGetter
	calls  int
}

func (r *stampingRetrier) RetryPayment(context.Context, string, string, string) error {
	r.calls++
	r.getter.stamped = true
	return domain.ErrNoPaymentMethodOnRetry
}

// A run on an invoice already stamped below Stripe's minimum ends at the
// next tick, before any retry or final action, even though the resolve that
// should have ended it was lost (a DB blip, the customer closing the tab, a
// restart between the stamp and the resolve). A card-less or refused-card
// retry never asks Stripe again, so without this nothing ended the run: the
// customer got reminders and the final action paused the subscription. The
// last case is the final retry's crash window: its lease was recorded, the
// stamp committed, the process died, and exhaust-on-entry picks it up.
func TestProcessRun_InvoiceStampedBelowMinimum_EndsTheRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attempts int // policy max is 3
	}{
		{"mid-schedule", 1},
		{"exhaust on entry", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retrier := &scriptedRetrier{results: []error{domain.ErrNoPaymentMethodOnRetry}}
			h := newHardDeclineHarness(t, retrier)
			twoAxisPolicy(h.store, domain.SubActionPause, domain.InvActionNone)
			pauser := &recordingPauser{}
			h.svc.SetSubscriptionPauser(pauser, &stampableInvoiceGetter{stamped: true})
			run := h.store.runs[h.runID]
			run.AttemptCount = tc.attempts
			h.store.runs[h.runID] = run

			got := h.tick()

			if got.State != domain.DunningResolved || got.Resolution != domain.ResolutionAmountBelowMinimum {
				t.Fatalf("run %s/%s, want resolved/amount_below_minimum", got.State, got.Resolution)
			}
			if retrier.calls != 0 {
				t.Errorf("retries %d, want none", retrier.calls)
			}
			if got.AttemptCount != tc.attempts {
				t.Errorf("attempt_count %d, want %d (no retry ran)", got.AttemptCount, tc.attempts)
			}
			if h.emails.warnings != 0 || h.emails.escalations != 0 {
				t.Errorf("warnings %d escalations %d, want none", h.emails.warnings, h.emails.escalations)
			}
			if pauser.calls != 0 {
				t.Errorf("final action paused the subscription %d times, want never", pauser.calls)
			}
		})
	}
}

// The stamp can land during the final retry itself (a hosted Pay click
// Stripe refuses while the tick runs), after the tick-start check read the
// invoice. The final action's own late re-check reads it again and ends the
// run instead of pausing the subscription.
func TestExhaustRun_StampedDuringTheFinalRetry_EndsTheRun(t *testing.T) {
	getter := &stampableInvoiceGetter{}
	retrier := &stampingRetrier{getter: getter}
	h := newHardDeclineHarness(t, retrier)
	twoAxisPolicy(h.store, domain.SubActionPause, domain.InvActionNone)
	pauser := &recordingPauser{}
	h.svc.SetSubscriptionPauser(pauser, getter)
	run := h.store.runs[h.runID]
	run.AttemptCount = 2 // the next retry is the last
	h.store.runs[h.runID] = run

	got := h.tick()

	if retrier.calls != 1 {
		t.Fatalf("retries %d, want 1", retrier.calls)
	}
	if got.State != domain.DunningResolved || got.Resolution != domain.ResolutionAmountBelowMinimum {
		t.Errorf("run %s/%s, want resolved/amount_below_minimum", got.State, got.Resolution)
	}
	if pauser.calls != 0 || h.emails.escalations != 0 {
		t.Errorf("pauses %d escalations %d, want none", pauser.calls, h.emails.escalations)
	}
}
