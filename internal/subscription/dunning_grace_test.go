package subscription

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// Dunning's final pause and cancel wait for the invoice grace (ADR-120
// amendment 2026-10-22); the operator's writes close the ended period at
// once. These tests pin which closes wait, on the in-memory store with a
// biller that closes whole periods the way the engine does. The
// real-Postgres proof is api.TestInvoiceGrace_DunningPauseWaitsForTheGrace and
// its siblings.

var (
	graceB  = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	graceP2 = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
)

const testGrace = time.Hour

// periodBiller closes every period whose end is at or before now less the
// grace it is given, as Engine.BillDue does, and records each grace.
type periodBiller struct {
	store  *memStore
	clk    clock.Clock
	graces []time.Duration
}

func (b *periodBiller) BillDue(ctx context.Context, _, id string, grace time.Duration) (int, error) {
	b.graces = append(b.graces, grace)
	sub := b.store.subs[id]
	n := 0
	for sub.NextBillingAt != nil && !sub.NextBillingAt.After(b.clk.Now(ctx).Add(-grace)) {
		start := *sub.NextBillingAt
		end := start.AddDate(0, 1, 0)
		sub.CurrentBillingPeriodStart, sub.CurrentBillingPeriodEnd, sub.NextBillingAt = &start, &end, &end
		n++
	}
	b.store.subs[id] = sub
	return n, nil
}

type graceFixture struct {
	store  *memStore
	svc    *Service
	biller *periodBiller
	subID  string
}

// newGraceFixture is an active subscription whose period ends at periodEnd,
// served at now with a 1h invoice grace. testClockID pins it to a test clock.
func newGraceFixture(t *testing.T, periodEnd, now time.Time, testClockID string) *graceFixture {
	t.Helper()
	store := newMemStore()
	clk := clock.NewFake(now)
	subID, _ := seedSubWithItemAt(t, store, "t1", "cus_1", "plan_a", periodEnd.AddDate(0, -1, 0), periodEnd)
	sub := store.subs[subID]
	next := periodEnd
	sub.NextBillingAt = &next
	sub.TestClockID = testClockID
	store.subs[subID] = sub
	biller := &periodBiller{store: store, clk: clk}
	svc := NewService(store, clk)
	svc.SetDueBiller(biller)
	svc.SetInvoiceGrace(testGrace)
	return &graceFixture{store: store, svc: svc, biller: biller, subID: subID}
}

func (f *graceFixture) sub() domain.Subscription { return f.store.subs[f.subID] }

// dunningAction runs dunning's pause or cancel.
func (f *graceFixture) dunningAction(action string) error {
	ctx := context.Background()
	if action == "pause" {
		_, err := f.svc.PauseCollectionForDunning(ctx, "t1", f.subID, "inv_1")
		return err
	}
	_, _, err := f.svc.CancelForDunning(ctx, "t1", f.subID)
	return err
}

// applied reports whether the action's end state holds.
func (f *graceFixture) applied(action string) bool {
	if action == "pause" {
		return f.sub().PauseCollection != nil
	}
	return f.sub().Status == domain.SubscriptionCanceled
}

func wantGraceWait(t *testing.T, err error, until time.Time) {
	t.Helper()
	var g *InsideInvoiceGraceError
	if !errors.As(err, &g) {
		t.Fatalf("err = %v, want InsideInvoiceGraceError", err)
	}
	if !g.Until.Equal(until) {
		t.Fatalf("waits until %v, want %v", g.Until, until)
	}
}

// The close is asked with the grace and closes nothing; the guard after it
// answers the wait. Mutation checks: remove the guard from closeEndedPeriods
// → the pause is refused unbilled_period and the cancel applies; pass 0 to
// BillDue → April closes.
func TestDunningActions_InsideTheGrace_WaitAndBillNothing(t *testing.T) {
	for _, action := range []string{"pause", "cancel"} {
		t.Run(action, func(t *testing.T) {
			f := newGraceFixture(t, graceB, graceB.Add(5*time.Minute), "")
			wantGraceWait(t, f.dunningAction(action), graceB.Add(testGrace))
			if len(f.biller.graces) != 1 || f.biller.graces[0] != testGrace {
				t.Fatalf("graces=%v, want one close with the grace", f.biller.graces)
			}
			if f.applied(action) || !f.sub().CurrentBillingPeriodStart.Equal(graceB.AddDate(0, -1, 0)) {
				t.Fatalf("status=%s pause=%+v period start=%v, want nothing changed", f.sub().Status, f.sub().PauseCollection, f.sub().CurrentBillingPeriodStart)
			}
		})
	}
}

// Mutation check: pass the grace from the operator's paths → they wait too.
func TestOperatorWrites_InsideTheGrace_CloseNow(t *testing.T) {
	now := graceB.Add(5 * time.Minute)
	t.Run("pause", func(t *testing.T) {
		f := newGraceFixture(t, graceB, now, "")
		if _, err := f.svc.PauseCollection(context.Background(), "t1", f.subID, PauseCollectionInput{Behavior: domain.PauseCollectionKeepAsDraft}); err != nil {
			t.Fatalf("PauseCollection: %v", err)
		}
		if len(f.biller.graces) != 1 || f.biller.graces[0] != 0 || !f.sub().CurrentBillingPeriodStart.Equal(graceB) || f.sub().PauseCollection == nil {
			t.Fatalf("graces=%v period start=%v pause=%+v, want one close with no grace, May running, paused", f.biller.graces, f.sub().CurrentBillingPeriodStart, f.sub().PauseCollection)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		f := newGraceFixture(t, graceB, now, "")
		if _, _, err := f.svc.Cancel(context.Background(), "t1", f.subID); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if len(f.biller.graces) != 1 || f.biller.graces[0] != 0 || f.sub().Status != domain.SubscriptionCanceled {
			t.Fatalf("graces=%v status=%s, want one close with no grace and canceled", f.biller.graces, f.sub().Status)
		}
	})
}

// The cases where dunning's action does not wait. Mutation check: drop the
// test-clock exemption → the test-clock case waits.
func TestDunningActions_CloseWhenNoGraceApplies(t *testing.T) {
	cases := []struct {
		name      string
		now       time.Time
		grace     time.Duration
		testClock string
		wantGrace time.Duration
	}{
		{"test clock", graceB.Add(5 * time.Minute), testGrace, "tc_1", 0},
		{"grace 0", graceB.Add(5 * time.Minute), 0, "", 0},
		{"past the grace", graceB.Add(testGrace), testGrace, "", testGrace},
	}
	for _, c := range cases {
		for _, action := range []string{"pause", "cancel"} {
			t.Run(c.name+"/"+action, func(t *testing.T) {
				f := newGraceFixture(t, graceB, c.now, c.testClock)
				f.svc.SetInvoiceGrace(c.grace)
				if err := f.dunningAction(action); err != nil {
					t.Fatalf("%s: %v", action, err)
				}
				if len(f.biller.graces) != 1 || f.biller.graces[0] != c.wantGrace {
					t.Fatalf("graces=%v, want one close with grace %v", f.biller.graces, c.wantGrace)
				}
				if !f.applied(action) || !f.sub().CurrentBillingPeriodStart.Equal(graceB) {
					t.Fatalf("status=%s pause=%+v period start=%v, want the action applied with May running", f.sub().Status, f.sub().PauseCollection, f.sub().CurrentBillingPeriodStart)
				}
			})
		}
	}
}

// TestDunningActions_CatchUp_ClosesTheOlderPeriodAndWaits: April and May both
// ended unbilled. Dunning's pause at Jun 1 00:05 closes April, which is past
// its grace, and waits for May's.
//
// Mutation check: pass 0 to BillDue from closeEndedPeriods → May closes too.
func TestDunningActions_CatchUp_ClosesTheOlderPeriodAndWaits(t *testing.T) {
	f := newGraceFixture(t, graceB, graceP2.Add(5*time.Minute), "")
	wantGraceWait(t, f.dunningAction("pause"), graceP2.Add(testGrace))
	if len(f.biller.graces) != 1 || f.biller.graces[0] != testGrace {
		t.Fatalf("graces=%v, want one close with the grace", f.biller.graces)
	}
	if !f.sub().CurrentBillingPeriodStart.Equal(graceB) || f.sub().PauseCollection != nil {
		t.Fatalf("period start=%v pause=%+v, want April closed, May open, not paused", f.sub().CurrentBillingPeriodStart, f.sub().PauseCollection)
	}
}
