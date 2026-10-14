package subscription

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// seedExpiredTrialMem puts a trialing sub with an elapsed trial into the mem
// store, with the given schedule.
func seedExpiredTrialMem(mem *memStore, cancelAtPeriodEnd bool, cancelAt *time.Time) (domain.Subscription, time.Time) {
	trialEnd := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := trialEnd.Add(30 * 24 * time.Hour)
	sub := domain.Subscription{
		ID: "sub_tc", TenantID: "t1", CustomerID: "cus_tc", Code: "sub-tc",
		Status: domain.SubscriptionTrialing, BillingTime: domain.BillingTimeAnniversary,
		TrialEndAt:                &trialEnd,
		CurrentBillingPeriodStart: &trialEnd,
		CurrentBillingPeriodEnd:   &periodEnd,
		NextBillingAt:             &periodEnd,
		CancelAtPeriodEnd:         cancelAtPeriodEnd,
		CancelAt:                  cancelAt,
	}
	mem.subs[sub.ID] = sub
	return sub, trialEnd
}

// TestProcessExpiredTrials_RoutesDueCancel locks the ADR-069 routing: a due
// schedule cancels FREE — exactly one subscription.canceled
// (reason=trial_end_cancel), ZERO subscription.trial_ended, ZERO bill calls,
// canceled_at = trial_end_at. Mutation seam: drop the trialCancelDue
// pre-route in ProcessExpiredTrials and the sub activates + bills instead.
func TestProcessExpiredTrials_RoutesDueCancel(t *testing.T) {
	mem := newMemStore()
	// Fake clock past trial end so the (mem) scan window logic is irrelevant;
	// we call the processor directly on the seeded set.
	svc := NewService(mem, clock.NewFake(time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)))
	fb := &fakeBiller{}
	svc.SetBiller(fb)
	fd := &fakeDispatcher{}
	svc.SetEventDispatcher(fd)
	_, trialEnd := seedExpiredTrialMem(mem, true, nil)

	processed, errsOut := svc.ProcessExpiredTrials(context.Background(), 50)
	if len(errsOut) != 0 {
		t.Fatalf("errors: %v", errsOut)
	}
	if processed != 1 {
		t.Fatalf("processed = %d, want 1", processed)
	}
	out := mem.subs["sub_tc"]
	if out.Status != domain.SubscriptionCanceled {
		t.Fatalf("status = %s, want canceled (free trial-end cancel)", out.Status)
	}
	if out.CanceledAt == nil || !out.CanceledAt.Equal(trialEnd) {
		t.Fatalf("canceled_at = %v, want trial_end %v", out.CanceledAt, trialEnd)
	}
	if fb.createTxCalls != 0 || fb.calls != 0 {
		t.Fatalf("bill calls = %d/%d, want 0/0 (trials are FREE)", fb.createTxCalls, fb.calls)
	}
	// Both lifecycle events moved IN-TX to the store writers (DispatchTx
	// subscription subset, 2026-07-05): subscription.canceled rides the
	// CancelAtTrialEnd tx (its CAS is what makes the winner fire exactly
	// once), and trial_ended must not fire at all on the cancel route.
	// Post-commit dispatch of either here would double-emit — assert none.
	for _, e := range fd.events {
		if e.eventType == domain.EventSubscriptionCanceled || e.eventType == domain.EventSubscriptionTrialEnded {
			t.Fatalf("%s must not be dispatched post-commit — lifecycle events are enqueued in the transition tx", e.eventType)
		}
	}
}

// TestProcessExpiredTrials_NoSchedule_ActivatesAsBefore is the control: the
// routing must not swallow normal activations.
func TestProcessExpiredTrials_NoSchedule_ActivatesAsBefore(t *testing.T) {
	mem := newMemStore()
	svc := NewService(mem, clock.NewFake(time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)))
	fb := &fakeBiller{}
	svc.SetBiller(fb)
	fd := &fakeDispatcher{}
	svc.SetEventDispatcher(fd)
	seedExpiredTrialMem(mem, false, nil)

	processed, errsOut := svc.ProcessExpiredTrials(context.Background(), 50)
	if len(errsOut) != 0 || processed != 1 {
		t.Fatalf("processed=%d errs=%v", processed, errsOut)
	}
	if mem.subs["sub_tc"].Status != domain.SubscriptionActive {
		t.Fatalf("status = %s, want active", mem.subs["sub_tc"].Status)
	}
	if fb.createTxCalls != 1 {
		t.Fatalf("BillOnCreateTx = %d, want 1 (day-1 in-tx coverage)", fb.createTxCalls)
	}
}

// TestScheduleCancel_TrialingBoundaries locks the cancel_at validation
// matrix (ADR-069): exactly trial_end_at (free) or >= period_end pass; the
// open interval is a 400 naming both boundaries. Mutation seam: drop the
// trialing relaxation → the trial_end value 400s; drop the interval
// rejection → the in-between value passes.
func TestScheduleCancel_TrialingBoundaries(t *testing.T) {
	mem := newMemStore()
	svc := NewService(mem, clock.NewFake(time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)))
	sub, _ := seedExpiredTrialMem(mem, false, nil)
	// Make the trial still-running for validation purposes (cancel_at must
	// be future).
	future := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	te := future
	sub.TrialEndAt = &te
	pe := future.Add(30 * 24 * time.Hour)
	sub.CurrentBillingPeriodEnd = &pe
	mem.subs[sub.ID] = sub

	// == trial_end: allowed, free cancel.
	if _, err := svc.ScheduleCancel(context.Background(), "t1", sub.ID, ScheduleCancelInput{CancelAt: &te}); err != nil {
		t.Fatalf("cancel_at == trial_end rejected: %v", err)
	}
	// Reset schedule.
	if _, err := svc.ClearScheduledCancel(context.Background(), "t1", sub.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}

	// In the open interval: 400 naming both boundaries.
	mid := te.Add(5 * 24 * time.Hour)
	_, err := svc.ScheduleCancel(context.Background(), "t1", sub.ID, ScheduleCancelInput{CancelAt: &mid})
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("in-between cancel_at: err = %v, want validation 400", err)
	}
	if !strings.Contains(err.Error(), "trial end") {
		t.Errorf("400 must name the trial-end boundary: %q", err.Error())
	}

	// >= period_end: allowed (existing machinery).
	if _, err := svc.ScheduleCancel(context.Background(), "t1", sub.ID, ScheduleCancelInput{CancelAt: &pe}); err != nil {
		t.Fatalf("cancel_at == period_end rejected: %v", err)
	}
}

// TestEndTrial_And_ExtendTrial_ScheduleGuards locks the 409s: EndTrial with
// ANY pending schedule 409s; ExtendTrial 409s only on an explicit cancel_at
// (the flag moves with the trial by design).
func TestEndTrial_And_ExtendTrial_ScheduleGuards(t *testing.T) {
	mem := newMemStore()
	svc := NewService(mem, clock.NewFake(time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)))
	sub, _ := seedExpiredTrialMem(mem, true, nil)
	future := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	s := mem.subs[sub.ID]
	s.TrialEndAt = &future
	mem.subs[sub.ID] = s

	if _, err := svc.EndTrial(context.Background(), "t1", sub.ID); !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("EndTrial with flag schedule: err = %v, want 409", err)
	} else if want := "This subscription is scheduled to cancel. Undo the scheduled cancellation first, then end the trial."; !strings.Contains(err.Error(), want) {
		// MANUAL_TEST quotes this text; the operator reads it as the 409 message.
		t.Fatalf("EndTrial refusal text = %q, want it to contain %q", err.Error(), want)
	}
	// ExtendTrial with flag-only schedule: allowed.
	if _, err := svc.ExtendTrial(context.Background(), "t1", sub.ID, future.Add(7*24*time.Hour)); err != nil {
		t.Fatalf("ExtendTrial with flag-only schedule must pass (it moves with the trial): %v", err)
	}

	// Explicit cancel_at: ExtendTrial 409s.
	s = mem.subs[sub.ID]
	explicit := *s.TrialEndAt
	s.CancelAt = &explicit
	s.CancelAtPeriodEnd = false
	mem.subs[sub.ID] = s
	if _, err := svc.ExtendTrial(context.Background(), "t1", sub.ID, explicit.Add(7*24*time.Hour)); !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("ExtendTrial with explicit cancel_at: err = %v, want 409", err)
	} else if want := "This subscription is scheduled to cancel on a set date. Undo the scheduled cancellation first, then extend the trial."; !strings.Contains(err.Error(), want) {
		t.Fatalf("ExtendTrial refusal text = %q, want it to contain %q", err.Error(), want)
	}
}
