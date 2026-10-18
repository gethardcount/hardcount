package dunning

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// #966: a card the bank refused for good is not charged again. The retrier
// (the payment adapter) decides that and reports it; these tests pin what
// dunning does with each report: the attempt still counts and the schedule
// carries on (Stripe Billing's rule for hard declines), the customer is asked
// for a different card instead of being promised a retry, and a later
// successful charge on a replaced card resolves the run as usual.

// scriptedRetrier answers each RetryPayment with the next scripted result.
type scriptedRetrier struct {
	results []error
	calls   int
}

func (s *scriptedRetrier) RetryPayment(context.Context, string, string, string) error {
	s.calls++
	if s.calls > len(s.results) {
		return errors.New("scriptedRetrier: no result left")
	}
	return s.results[s.calls-1]
}

// hardDecline is the adapter's report of a retry whose charge the bank
// refused for good: the declined PaymentIntent, Stripe's message, and the flag.
func hardDecline(pi string) error {
	return &RetryError{PaymentIntentID: pi, Err: errors.New("payment failed: Your card was declined."), CardNeedsReplacing: true}
}

type hardDeclineHarness struct {
	t      *testing.T
	store  *memStore
	svc    *Service
	emails *recordingEmailNotifier
	runID  string
}

func newHardDeclineHarness(t *testing.T, retrier PaymentRetrier) *hardDeclineHarness {
	t.Helper()
	store := newMemStore() // max 3 attempts, schedule 72h then 120h, no final actions
	svc := NewService(store, retrier, nil)
	emails := &recordingEmailNotifier{}
	svc.SetEmailNotifier(emails)
	svc.SetCustomerEmailFetcher(stubCustomerEmail{})
	run, err := svc.StartDunning(context.Background(), "t1", "inv_1", "cus_1", time.Time{}, domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	return &hardDeclineHarness{t: t, store: store, svc: svc, emails: emails, runID: run.ID}
}

// tick makes the run due and runs one scheduler pass.
func (h *hardDeclineHarness) tick() domain.InvoiceDunningRun {
	h.t.Helper()
	run := h.store.runs[h.runID]
	past := time.Now().UTC().Add(-time.Minute)
	run.NextActionAt = &past
	h.store.runs[h.runID] = run
	if _, errs := h.svc.ProcessDueRuns(context.Background(), "t1", 20); len(errs) > 0 {
		h.t.Fatalf("process due runs: %v", errs)
	}
	return h.store.runs[h.runID]
}

func (h *hardDeclineHarness) lastRetryEvent() domain.InvoiceDunningEvent {
	h.t.Helper()
	for i := len(h.store.events) - 1; i >= 0; i-- {
		if h.store.events[i].EventType == domain.DunningEventRetryAttempted {
			return h.store.events[i]
		}
	}
	h.t.Fatal("no retry_attempted event")
	return domain.InvoiceDunningEvent{}
}

// A retry that skipped the refused card charged nothing, counts as an
// attempt, keeps the schedule, and asks the customer for a different card.
func TestProcessRun_RefusedCardSkip_CountsAndAsksForADifferentCard(t *testing.T) {
	h := newHardDeclineHarness(t, &scriptedRetrier{results: []error{domain.ErrCardNeedsReplacing}})
	before := time.Now().UTC()

	run := h.tick()

	if run.AttemptCount != 1 || run.State != domain.DunningActive {
		t.Fatalf("run attempt %d state %s, want 1 active — the skipped retry counts and the run carries on", run.AttemptCount, run.State)
	}
	// Rescheduled on the policy's next interval (72h), like any failed retry —
	// not left due, and not jumped to exhaustion.
	if run.NextActionAt == nil || run.NextActionAt.Before(before.Add(71*time.Hour)) || run.NextActionAt.After(before.Add(73*time.Hour)) {
		t.Errorf("next action %v, want about 72h out", run.NextActionAt)
	}
	evt := h.lastRetryEvent()
	if evt.Reason != domain.DunningRetryReasonCardRefused {
		t.Errorf("retry event reason %q, want %q (the timeline shows it as not charged)", evt.Reason, domain.DunningRetryReasonCardRefused)
	}
	if _, ok := evt.Metadata["payment_intent_id"]; ok {
		t.Errorf("retry event metadata %v names a PaymentIntent, but nothing was charged", evt.Metadata)
	}
	if h.emails.warnings != 1 || !h.emails.lastWarnCardRefused || h.emails.lastWarnReason != "" {
		t.Errorf("warnings %d (card refused %v, reason %q), want 1 card-refused warning with no reason",
			h.emails.warnings, h.emails.lastWarnCardRefused, h.emails.lastWarnReason)
	}
}

// The retry whose own charge drew the hard decline is an ordinary counted
// decline on the timeline (its PaymentIntent and Stripe's message), but its
// warning already asks for a different card: promising "we'll try again on
// <date>" would promise a charge that cannot succeed.
func TestProcessRun_HardDeclineOnRetry_AsksForADifferentCard(t *testing.T) {
	h := newHardDeclineHarness(t, &scriptedRetrier{results: []error{hardDecline("pi_lost")}})

	run := h.tick()

	if run.AttemptCount != 1 || run.State != domain.DunningActive {
		t.Fatalf("run attempt %d state %s, want 1 active", run.AttemptCount, run.State)
	}
	evt := h.lastRetryEvent()
	if evt.Reason != "payment failed: Your card was declined." {
		t.Errorf("retry event reason %q, want Stripe's decline message", evt.Reason)
	}
	if pi, _ := evt.Metadata["payment_intent_id"].(string); pi != "pi_lost" {
		t.Errorf("retry event PaymentIntent %q, want pi_lost", pi)
	}
	if h.emails.warnings != 1 || !h.emails.lastWarnCardRefused || h.emails.lastWarnReason != "" {
		t.Errorf("warnings %d (card refused %v, reason %q), want 1 card-refused warning with no reason",
			h.emails.warnings, h.emails.lastWarnCardRefused, h.emails.lastWarnReason)
	}
}

// Control: any other decline keeps the ordinary warning, with its reason and
// a promised retry date.
func TestProcessRun_OrdinaryDecline_KeepsTheRetryWarning(t *testing.T) {
	h := newHardDeclineHarness(t, &scriptedRetrier{results: []error{
		&RetryError{PaymentIntentID: "pi_nsf", Err: errors.New("payment failed: Your card has insufficient funds.")},
	}})

	h.tick()

	if h.emails.warnings != 1 || h.emails.lastWarnCardRefused || h.emails.lastWarnReason != "payment failed: Your card has insufficient funds." {
		t.Errorf("warnings %d (card refused %v, reason %q), want 1 ordinary warning carrying the decline reason",
			h.emails.warnings, h.emails.lastWarnCardRefused, h.emails.lastWarnReason)
	}
}

// The card is replaced mid-dunning: the first retry draws the hard decline,
// the second skips the refused card, and once a different card is the
// default the third charges it and the run resolves as recovered.
func TestProcessRun_CardReplacedMidDunning_Recovers(t *testing.T) {
	retrier := &scriptedRetrier{results: []error{hardDecline("pi_lost"), domain.ErrCardNeedsReplacing, nil}}
	h := newHardDeclineHarness(t, retrier)

	h.tick()
	if run := h.tick(); run.AttemptCount != 2 || run.State != domain.DunningActive {
		t.Fatalf("after the skipped retry: attempt %d state %s, want 2 active", run.AttemptCount, run.State)
	}
	run := h.tick()

	if retrier.calls != 3 {
		t.Errorf("retries %d, want 3", retrier.calls)
	}
	if run.State != domain.DunningResolved || run.Resolution != domain.ResolutionPaymentRecovered {
		t.Errorf("run %s/%s, want resolved/payment_recovered", run.State, run.Resolution)
	}
	if h.emails.escalations != 0 {
		t.Errorf("escalation emails %d, want 0", h.emails.escalations)
	}
}

// A card that is never replaced does not hold the run open forever: every
// scheduled retry counts, and the run escalates when the policy's attempts
// run out, exactly as it would have on declines.
func TestProcessRun_RefusedCardNeverReplaced_StillExhausts(t *testing.T) {
	h := newHardDeclineHarness(t, &scriptedRetrier{results: []error{
		hardDecline("pi_lost"), domain.ErrCardNeedsReplacing, domain.ErrCardNeedsReplacing,
	}})

	h.tick()
	h.tick()
	run := h.tick()

	if run.State != domain.DunningEscalated || run.Resolution != domain.ResolutionRetriesExhausted || run.AttemptCount != 3 {
		t.Fatalf("run %s/%s after %d attempts, want escalated/retries_exhausted after 3", run.State, run.Resolution, run.AttemptCount)
	}
	if h.emails.warnings != 2 || !h.emails.lastWarnCardRefused {
		t.Errorf("warnings %d (last card refused %v), want 2 card-refused warnings", h.emails.warnings, h.emails.lastWarnCardRefused)
	}
	if h.emails.escalations != 1 {
		t.Errorf("escalation emails %d, want 1", h.emails.escalations)
	}
}
