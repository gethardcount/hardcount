package dunning

import (
	"context"
	"fmt"
	"testing"

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
