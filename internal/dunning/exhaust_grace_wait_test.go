package dunning

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// A final pause or cancel that would close a period still inside its invoice
// grace answers SubscriptionActionNotYet (ADR-120 amendment 2026-10-22). The
// run then waits: it stays active and is due again when the grace ends. It is
// not a failed action (no action_failed label, no 24h wait), nothing else
// fires (no write-off, escalation row, email or webhook), and the invoice's
// charge hold is released so a payment can still land in the wait.
// The real-Postgres proof that the wait bills the grace-window usage is
// api.TestInvoiceGrace_DunningPauseWaitsForTheGrace.

// graceWaitAction is a pauser and a canceler that waits for the grace while
// wait is set, and acts after. before, when set, runs inside the first call:
// the window between the exhaustion's claim and its deferral write.
type graceWaitAction struct {
	until  time.Time
	wait   bool
	calls  int
	before func()
}

func (a *graceWaitAction) act() error {
	a.calls++
	if a.before != nil {
		f := a.before
		a.before = nil
		f()
	}
	if a.wait {
		return &SubscriptionActionNotYet{Until: a.until}
	}
	return nil
}

func (a *graceWaitAction) PauseCollection(context.Context, string, string, string) (bool, error) {
	if err := a.act(); err != nil {
		return false, err
	}
	return true, nil
}

func (a *graceWaitAction) Cancel(context.Context, string, string) error { return a.act() }

// countingHolds is the invoice's charge hold: every claim is granted.
type countingHolds struct{ claimed, released int }

func (h *countingHolds) ClaimChargeHoldForFinalAction(context.Context, string, string) (domain.ChargeHold, error) {
	h.claimed++
	return domain.ChargeHold{Until: time.Now().UTC().Add(5 * time.Minute)}, nil
}

func (h *countingHolds) ReleaseChargeHold(context.Context, string, string, domain.ChargeHold) error {
	h.released++
	return nil
}

type graceWaitHarness struct {
	store  *memStore
	svc    *Service
	action *graceWaitAction
	holds  *countingHolds
	writer *capturingUncollect
	emails *recordingEmailNotifier
	events *captureDispatcher
}

func newGraceWaitHarness(subAction domain.DunningSubscriptionAction) *graceWaitHarness {
	h := &graceWaitHarness{
		store:  newMemStore(),
		action: &graceWaitAction{until: time.Now().UTC().Add(40 * time.Minute).Truncate(time.Microsecond), wait: true},
		holds:  &countingHolds{},
		writer: &capturingUncollect{},
		emails: &recordingEmailNotifier{},
		events: &captureDispatcher{},
	}
	twoAxisPolicy(h.store, subAction, domain.InvActionMarkUncollectible)
	h.svc = NewService(h.store, &noopRetrier{}, nil)
	h.svc.SetSubscriptionPauser(h.action, stubInvGet{inv: unpaidInv()})
	h.svc.SetSubscriptionCanceler(h.action)
	h.svc.SetChargeHoldClaimer(h.holds)
	h.svc.SetInvoiceUncollectibleMarker(h.writer)
	h.svc.SetEmailNotifier(h.emails)
	h.svc.SetCustomerEmailFetcher(stubCustomerEmail{})
	h.svc.SetEventDispatcher(h.events)
	return h
}

func (h *graceWaitHarness) escalatedRows() int {
	n := 0
	for _, e := range h.store.events {
		if e.EventType == domain.DunningEventEscalated {
			n++
		}
	}
	return n
}

func (h *graceWaitHarness) escalatedWebhooks() int {
	return h.events.countOf(domain.EventDunningEscalated)
}

// Mutation checks: send the wait down the action_failed path → resolution
// action_failed and next_action_at a day out; return before the hold release
// → released 0; skip the deferral write → next_action_at stays the lease.
func TestExhaust_SubscriptionActionNotYet_WaitsForTheGrace(t *testing.T) {
	for _, action := range []domain.DunningSubscriptionAction{domain.SubActionPause, domain.SubActionCancel} {
		t.Run(string(action), func(t *testing.T) {
			h := newGraceWaitHarness(action)
			ctx := context.Background()
			run := exhaustedRun(t, h.store, h.svc)

			h.svc.ProcessDueRuns(ctx, "t1", 20)

			got := h.store.runs[run.ID]
			if got.State != domain.DunningActive || got.Resolution == domain.ResolutionActionFailed || got.ResolvedAt != nil {
				t.Fatalf("run = state %q resolution %q resolved_at %v, want active and not action_failed", got.State, got.Resolution, got.ResolvedAt)
			}
			if got.NextActionAt == nil || !got.NextActionAt.Equal(h.action.until) {
				t.Fatalf("next_action_at = %v, want the end of the grace %v", got.NextActionAt, h.action.until)
			}
			if got.AttemptCount != run.AttemptCount {
				t.Fatalf("attempt_count = %d, want %d unchanged", got.AttemptCount, run.AttemptCount)
			}
			if h.holds.claimed != 1 || h.holds.released != 1 {
				t.Fatalf("charge hold claimed %d released %d, want 1 and 1: the wait must not keep the invoice held", h.holds.claimed, h.holds.released)
			}
			if len(h.writer.calls) != 0 {
				t.Fatalf("write-off calls = %d, want 0: the invoice action waits with the subscription action", len(h.writer.calls))
			}
			if n := h.escalatedRows(); n != 0 || h.emails.escalations != 0 || h.escalatedWebhooks() != 0 {
				t.Fatalf("escalated rows %d, emails %d, webhooks %d; want none while the action waits", n, h.emails.escalations, h.escalatedWebhooks())
			}

			// Not due before the grace ends: a tick now does nothing.
			h.svc.ProcessDueRuns(ctx, "t1", 20)
			if h.action.calls != 1 {
				t.Fatalf("action calls = %d after a tick inside the wait, want 1", h.action.calls)
			}

			// The grace has passed: the run is due, the action applies, and the
			// run escalates once with the write-off.
			h.action.wait = false
			r := h.store.runs[run.ID]
			past := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
			r.NextActionAt = &past
			h.store.runs[run.ID] = r
			h.svc.ProcessDueRuns(ctx, "t1", 20)

			got = h.store.runs[run.ID]
			if got.State != domain.DunningEscalated || got.Resolution != domain.ResolutionRetriesExhausted {
				t.Fatalf("after the grace: run = state %q resolution %q, want escalated", got.State, got.Resolution)
			}
			if h.action.calls != 2 || len(h.writer.calls) != 1 || h.escalatedRows() != 1 || h.emails.escalations != 1 {
				t.Fatalf("after the grace: action calls %d, write-offs %d, escalated rows %d, emails %d; want 2, 1, 1, 1",
					h.action.calls, len(h.writer.calls), h.escalatedRows(), h.emails.escalations)
			}
		})
	}
}

// TestExhaust_NotYet_StaleLeaseLoses is the collision: this processor's
// exhaustion outlived its lease, and another processor claimed the run again
// (a fresh lease) while the subscription action was running. The wait is
// written only over this processor's own lease, so it does not overwrite the
// other processor's claim.
//
// Mutation check: write the wait with a state-only guard (UpdateRunIfActive)
// or with the row's current next_action_at as the expected value → the other
// processor's lease is overwritten.
func TestExhaust_NotYet_StaleLeaseLoses(t *testing.T) {
	h := newGraceWaitHarness(domain.SubActionPause)
	run := exhaustedRun(t, h.store, h.svc)
	otherLease := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Microsecond)
	h.action.before = func() {
		r := h.store.runs[run.ID]
		r.NextActionAt = &otherLease
		h.store.runs[run.ID] = r
	}

	h.svc.ProcessDueRuns(context.Background(), "t1", 20)

	got := h.store.runs[run.ID]
	if got.NextActionAt == nil || !got.NextActionAt.Equal(otherLease) {
		t.Fatalf("next_action_at = %v, want the other processor's lease %v left as it is", got.NextActionAt, otherLease)
	}
	if got.State != domain.DunningActive || got.Resolution == domain.ResolutionActionFailed {
		t.Fatalf("run = state %q resolution %q, want active and untouched", got.State, got.Resolution)
	}
	if h.escalatedRows() != 0 || len(h.writer.calls) != 0 {
		t.Fatalf("escalated rows %d, write-offs %d; want none", h.escalatedRows(), len(h.writer.calls))
	}
}

// TestExhaust_NotYet_ActionFailedReattemptKeepsItsMark: an earlier final
// action failed, so the run is active as action_failed and due a day later.
// That re-attempt lands inside a new period's grace and waits. The wait
// writes only next_action_at: the run keeps action_failed, which stays true
// (the last attempt of the action failed; the wait attempted nothing) until
// the action runs after the grace and the run escalates (ADR-112 amendment
// 2026-10-22).
func TestExhaust_NotYet_ActionFailedReattemptKeepsItsMark(t *testing.T) {
	h := newGraceWaitHarness(domain.SubActionPause)
	ctx := context.Background()
	run := exhaustedRun(t, h.store, h.svc)
	r := h.store.runs[run.ID]
	r.Resolution = domain.ResolutionActionFailed
	h.store.runs[run.ID] = r

	h.svc.ProcessDueRuns(ctx, "t1", 20)

	got := h.store.runs[run.ID]
	if got.State != domain.DunningActive || got.Resolution != domain.ResolutionActionFailed || got.ResolvedAt != nil {
		t.Fatalf("run = state %q resolution %q resolved_at %v, want active and still action_failed", got.State, got.Resolution, got.ResolvedAt)
	}
	if got.NextActionAt == nil || !got.NextActionAt.Equal(h.action.until) {
		t.Fatalf("next_action_at = %v, want the end of the grace %v, not a day out", got.NextActionAt, h.action.until)
	}

	h.action.wait = false
	past := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	got.NextActionAt = &past
	h.store.runs[run.ID] = got
	h.svc.ProcessDueRuns(ctx, "t1", 20)
	if got = h.store.runs[run.ID]; got.State != domain.DunningEscalated || got.Resolution != domain.ResolutionRetriesExhausted {
		t.Fatalf("after the grace: run = state %q resolution %q, want escalated as retries_exhausted", got.State, got.Resolution)
	}
}
