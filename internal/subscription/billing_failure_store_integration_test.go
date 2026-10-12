package subscription_test

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

func (s *bfSeed) record(id string, w subscription.BillingFailureWrite) bool {
	s.t.Helper()
	ok, err := s.store.RecordBillingFailure(s.ctx, s.tenantID, id, w)
	if err != nil {
		s.t.Fatalf("RecordBillingFailure(%s): %v", id, err)
	}
	return ok
}

// TestRecordBillingFailure_Guards locks each guard in the failure write's
// WHERE on real Postgres (ADR-119). A failure may land only on the work the
// failing attempt read, and only once per attempt.
//
// Mutation seams, each turning one subtest red:
//   - drop `<due> IS NOT DISTINCT FROM $2` → "due instant moved";
//   - drop the observed-count CASE         → "two writers, same attempt";
//   - drop `test_clock_id IS NULL`         → "clock-pinned";
//   - drop the status IN                   → "canceled".
func TestRecordBillingFailure_Guards(t *testing.T) {
	s := newBFSeed(t, "rec")
	due := s.now.Add(-48 * time.Hour)
	next := s.now.Add(15 * time.Minute)
	write := func(dueSeen time.Time, observed int) subscription.BillingFailureWrite {
		return subscription.BillingFailureWrite{
			DueSeen: dueSeen, ObservedAttempts: observed, Attempts: observed + 1,
			FailedAt: s.now, NextAttemptAt: &next, LastError: "recorded by the test",
		}
	}

	t.Run("first failure is saved and read back", func(t *testing.T) {
		id := s.sub(bfRow{id: "sub_rec_first", nextBilling: due})
		if !s.record(id, write(due, 0)) {
			t.Fatal("a first failure on due work was not written")
		}
		sub := s.get(id)
		bf := sub.BillingFailure
		if bf == nil || bf.Attempts != 1 || bf.Parked || bf.NextAttemptAt == nil || !bf.NextAttemptAt.Equal(next) ||
			!bf.FailedAt.Equal(s.now) || bf.LastError != "recorded by the test" || bf.MaxAttempts != domain.MaxBillingAttempts {
			t.Fatalf("read back %+v", bf)
		}
		// The write touches only the five columns: updated_at is the seed's.
		if !sub.UpdatedAt.Equal(due.Add(-30 * 24 * time.Hour)) {
			t.Fatalf("updated_at moved to %s: the failure write must not touch it", sub.UpdatedAt)
		}
	})

	t.Run("due instant moved", func(t *testing.T) {
		id := s.sub(bfRow{id: "sub_rec_moved", nextBilling: due})
		before := s.rawRecord(id)
		// The attempt read the previous close; another writer has billed it.
		if s.record(id, write(due.Add(-30*24*time.Hour), 0)) {
			t.Fatal("a failure for a due instant the row no longer has was written — it would gate work that never failed")
		}
		if got := s.rawRecord(id); got != before {
			t.Fatalf("columns changed on a refused write: %s → %s", before, got)
		}
	})

	t.Run("two writers, same attempt", func(t *testing.T) {
		id := s.sub(bfRow{id: "sub_rec_race", nextBilling: due})
		var wg sync.WaitGroup
		start := make(chan struct{})
		wrote := make([]bool, 2)
		for i := range wrote {
			wg.Go(func() {
				<-start
				ok, err := s.store.RecordBillingFailure(s.ctx, s.tenantID, id, write(due, 0))
				if err != nil {
					t.Errorf("writer %d: %v", i, err)
				}
				wrote[i] = ok
			})
		}
		close(start)
		wg.Wait()
		if wrote[0] == wrote[1] {
			t.Fatalf("writers reported %v, want exactly one to land", wrote)
		}
		if bf := s.get(id).BillingFailure; bf == nil || bf.Attempts != 1 {
			t.Fatalf("after two writers of one attempt: %+v, want attempts 1", bf)
		}
	})

	t.Run("observed count must match", func(t *testing.T) {
		id := s.sub(bfRow{id: "sub_rec_count", nextBilling: due, failedFor: &due, count: 3, nextAttempt: &next})
		if s.record(id, write(due, 2)) {
			t.Fatal("a write that observed 2 landed on a row holding 3")
		}
		if !s.record(id, write(due, 3)) {
			t.Fatal("a write that observed the row's count was refused")
		}
		if bf := s.get(id).BillingFailure; bf == nil || bf.Attempts != 4 {
			t.Fatalf("got %+v, want attempts 4", bf)
		}
	})

	t.Run("clock-pinned", func(t *testing.T) {
		at := s.frozen.Add(-time.Hour)
		id := s.sub(bfRow{id: "sub_rec_clock", nextBilling: at, clockID: s.clockID})
		if s.record(id, write(at, 0)) {
			t.Fatal("a real-time wait was written on a clock-pinned subscription")
		}
	})

	t.Run("canceled", func(t *testing.T) {
		id := s.sub(bfRow{id: "sub_rec_canceled", status: "canceled", nextBilling: due})
		if s.record(id, write(due, 0)) {
			t.Fatal("a failure was written on a canceled subscription")
		}
	})

	t.Run("stale columns under a new due instant restart at 1", func(t *testing.T) {
		stale := due.Add(-30 * 24 * time.Hour)
		id := s.sub(bfRow{id: "sub_rec_stale", nextBilling: due, failedFor: &stale, count: 5})
		if s.get(id).BillingFailure != nil {
			t.Fatal("seed: the stale record reads as live")
		}
		if !s.record(id, write(due, 0)) {
			t.Fatal("a first failure on new work was refused because of stale columns")
		}
		if bf := s.get(id).BillingFailure; bf == nil || bf.Attempts != 1 {
			t.Fatalf("got %+v, want attempts 1", bf)
		}
	})

	t.Run("read as trialing, now active, same due instant", func(t *testing.T) {
		trialEnd := s.now.Add(-72 * time.Hour)
		id := s.sub(bfRow{id: "sub_rec_flip", status: "trialing", nextBilling: due, trialEnd: &trialEnd})
		seen := s.get(id)
		// The trial flip commits on its own; the failure comes after it.
		if _, err := s.store.ActivateAfterTrial(s.ctx, s.tenantID, id, trialEnd); err != nil {
			t.Fatalf("activate: %v", err)
		}
		if !s.record(id, write(*domain.DueInstant(seen), 0)) {
			t.Fatal("a failure after the trial flip committed was not recorded on that attempt")
		}
		if sub := s.get(id); sub.Status != domain.SubscriptionActive || sub.BillingFailure == nil {
			t.Fatalf("status=%s failure=%+v", sub.Status, sub.BillingFailure)
		}
	})

	t.Run("no next attempt parks", func(t *testing.T) {
		id := s.sub(bfRow{id: "sub_rec_park", nextBilling: due, failedFor: &due, count: 7, nextAttempt: &next})
		w := write(due, 7)
		w.NextAttemptAt = nil
		if !s.record(id, w) {
			t.Fatal("the eighth failure was refused")
		}
		if bf := s.get(id).BillingFailure; bf == nil || !bf.Parked || bf.NextAttemptAt != nil || bf.Attempts != 8 {
			t.Fatalf("got %+v, want parked at 8", bf)
		}
	})

	t.Run("due through the cancel arm", func(t *testing.T) {
		// next_billing_at is a month out; the due work is the mid-period
		// cancel at cancel_at, and that is the key.
		cancelAt := s.now.Add(-time.Hour)
		id := s.sub(bfRow{id: "sub_rec_cancelarm", nextBilling: s.now.Add(20 * 24 * time.Hour), cancelAt: &cancelAt})
		if s.record(id, write(s.now.Add(20*24*time.Hour), 0)) {
			t.Fatal("a write keyed on next_billing_at landed while the due instant is cancel_at")
		}
		if !s.record(id, write(cancelAt, 0)) {
			t.Fatal("a failing mid-period cancel was not recorded")
		}
		if bf := s.get(id).BillingFailure; bf == nil || bf.Attempts != 1 {
			t.Fatalf("got %+v", bf)
		}
	})
}

// TestDueInstantWriters_VoidOrKeepTheRecord is the site-set table of ADR-119:
// every UPDATE of a subscriptions row, and what it does to a live
// billing-failure record. No writer touches the five columns. A writer that
// makes the failing work stop being due voids the record by moving the due
// instant (or the status); every other writer leaves it live.
//
// Each case also proves the columns themselves were not written, and (through
// bfSeed.get) that the Go rule and the SQL predicate agree about the row.
func TestDueInstantWriters_VoidOrKeepTheRecord(t *testing.T) {
	s := newBFSeed(t, "wr")
	plan, err := pricing.NewPostgresStore(s.db).CreatePlan(s.ctx, s.tenantID, domain.Plan{
		Code: "bf-plan", Name: "bf", Currency: "USD", BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	day := 24 * time.Hour
	due := s.now.Add(-2 * day)       // a close that is overdue
	future := s.now.Add(20 * day)    // a close that is not due yet
	cancelDue := s.now.Add(-3 * day) // a mid-period cancel that is overdue
	trialEnd := s.now.Add(-4 * day)
	parked := func(r bfRow, key time.Time) bfRow {
		r.failedFor, r.count = &key, 8
		return r
	}
	closeTo := func(id string, start, end time.Time) error {
		sub := s.get(id)
		return s.store.WithTenantTx(s.ctx, s.tenantID, func(tx *sql.Tx) error {
			return s.store.ClosePeriodTx(s.ctx, tx, s.tenantID, id, subscription.SnapshotOf(sub), start, end, end, 0)
		})
	}

	cases := []struct {
		name     string
		row      bfRow
		act      func(id string) error
		wantLive bool
	}{
		// --- void: the due instant moves ---
		{"ClosePeriodTx advancing the period", parked(bfRow{nextBilling: due}, due), func(id string) error {
			return closeTo(id, due, due.Add(30*day))
		}, false},
		{"ExtendTrial", parked(bfRow{status: "trialing", nextBilling: due, trialEnd: &trialEnd}, due), func(id string) error {
			_, err := s.store.ExtendTrial(s.ctx, s.tenantID, id, s.now.Add(7*day), s.now.Add(7*day), s.now.Add(37*day), s.now.Add(37*day), 0)
			return err
		}, false},
		{"EndTrialEarly", parked(bfRow{status: "trialing", nextBilling: due, trialEnd: &trialEnd}, due), func(id string) error {
			_, err := s.store.EndTrialEarly(s.ctx, s.tenantID, id, s.now, s.now, s.now.Add(30*day), s.now.Add(30*day), 0)
			return err
		}, false},
		{"ClearScheduledCancellation on a failing mid-period cancel", parked(bfRow{nextBilling: future, cancelAt: &cancelDue}, cancelDue), func(id string) error {
			_, err := s.store.ClearScheduledCancellation(s.ctx, s.tenantID, id)
			return err
		}, false},
		{"ScheduleCancellation moving a failing mid-period cancel", parked(bfRow{nextBilling: future, cancelAt: &cancelDue}, cancelDue), func(id string) error {
			_, err := s.store.ScheduleCancellation(s.ctx, s.tenantID, id, &future, false, domain.SubscriptionActive)
			return err
		}, false},
		// --- void: the row leaves active/trialing; the columns stay as evidence ---
		{"CancelAtomic", parked(bfRow{nextBilling: due}, due), func(id string) error {
			_, err := s.store.CancelAtomic(s.ctx, s.tenantID, id)
			return err
		}, false},
		{"FireScheduledCancellationTx", parked(bfRow{nextBilling: due}, due), func(id string) error {
			sub := s.get(id)
			return s.store.WithTenantTx(s.ctx, s.tenantID, func(tx *sql.Tx) error {
				_, err := s.store.FireScheduledCancellationTx(s.ctx, tx, s.tenantID, id, subscription.SnapshotOf(sub), s.now)
				return err
			})
		}, false},
		{"FireScheduledCancellationWithBill", parked(bfRow{nextBilling: future, cancelAt: &cancelDue}, cancelDue), func(id string) error {
			_, err := s.store.FireScheduledCancellationWithBill(s.ctx, s.tenantID, id, cancelDue, nil)
			return err
		}, false},
		{"CancelAtTrialEnd", parked(bfRow{status: "trialing", nextBilling: due, trialEnd: &trialEnd, cancelAt: &trialEnd}, due), func(id string) error {
			_, err := s.store.CancelAtTrialEnd(s.ctx, s.tenantID, id, trialEnd)
			return err
		}, false},

		// --- keep: the same work is still due ---
		{"ClosePeriodTx verify-only (same values)", parked(bfRow{nextBilling: due}, due), func(id string) error {
			return closeTo(id, due.Add(-30*day), due)
		}, true},
		{"SetPauseCollection", parked(bfRow{nextBilling: due}, due), func(id string) error {
			_, err := s.store.SetPauseCollection(s.ctx, s.tenantID, id, domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft})
			return err
		}, true},
		{"ClearPauseCollection", parked(bfRow{nextBilling: due}, due), func(id string) error {
			if _, err := s.store.SetPauseCollection(s.ctx, s.tenantID, id, domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft}); err != nil {
				return err
			}
			_, err := s.store.ClearPauseCollection(s.ctx, s.tenantID, id)
			return err
		}, true},
		{"SetBillingThresholds", parked(bfRow{nextBilling: due}, due), func(id string) error {
			_, err := s.store.SetBillingThresholds(s.ctx, s.tenantID, id, domain.BillingThresholds{AmountGTE: 5000, ResetBillingCycle: true})
			return err
		}, true},
		{"ClearBillingThresholds", parked(bfRow{nextBilling: due}, due), func(id string) error {
			_, err := s.store.ClearBillingThresholds(s.ctx, s.tenantID, id)
			return err
		}, true},
		{"AddItem", parked(bfRow{nextBilling: due}, due), func(id string) error {
			_, err := s.store.AddItem(s.ctx, s.tenantID, domain.SubscriptionItem{SubscriptionID: id, PlanID: plan.ID, Quantity: 1})
			return err
		}, true},
		{"ActivateAfterTrial (status only)", parked(bfRow{status: "trialing", nextBilling: due, trialEnd: &trialEnd}, due), func(id string) error {
			_, err := s.store.ActivateAfterTrial(s.ctx, s.tenantID, id, trialEnd)
			return err
		}, true},
		{"ScheduleCancellation at period end on a failing close", parked(bfRow{nextBilling: due}, due), func(id string) error {
			_, err := s.store.ScheduleCancellation(s.ctx, s.tenantID, id, nil, true, domain.SubscriptionActive)
			return err
		}, true},
		{"ScheduleCancellation at a later instant on a failing close", parked(bfRow{nextBilling: due}, due), func(id string) error {
			_, err := s.store.ScheduleCancellation(s.ctx, s.tenantID, id, &future, false, domain.SubscriptionActive)
			return err
		}, true},
		{"ClearScheduledCancellation on a failing close", parked(bfRow{nextBilling: due, cancelAt: &future}, due), func(id string) error {
			_, err := s.store.ClearScheduledCancellation(s.ctx, s.tenantID, id)
			return err
		}, true},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.row.id = "sub_wr_" + string(rune('a'+i))
			id := s.sub(c.row)
			if s.get(id).BillingFailure == nil {
				t.Fatal("seed: the record is not live before the writer runs")
			}
			before := s.rawRecord(id)
			if err := c.act(id); err != nil {
				t.Fatalf("writer: %v", err)
			}
			if got := s.rawRecord(id); got != before {
				t.Fatalf("the writer touched the billing_* columns: %s → %s", before, got)
			}
			if live := s.get(id).BillingFailure != nil; live != c.wantLive {
				t.Fatalf("record live after the writer = %v, want %v", live, c.wantLive)
			}
		})
	}
}

// TestClearBillingFailure covers the operator retry's write: it clears only a
// live record, hands the cleared record to the in-transaction hook, and shares
// fate with that hook.
func TestClearBillingFailure(t *testing.T) {
	s := newBFSeed(t, "clr")
	due := s.now.Add(-48 * time.Hour)
	const blank = "-|0|-|-|-"

	t.Run("live parked record: cleared, hook sees it", func(t *testing.T) {
		id := s.sub(bfRow{id: "sub_clr_live", nextBilling: due, failedFor: &due, count: 8})
		var seen domain.BillingFailure
		cleared, err := s.store.ClearBillingFailure(s.ctx, s.tenantID, id, func(tx *sql.Tx, c domain.BillingFailure) error {
			seen = c
			return nil
		})
		if err != nil {
			t.Fatalf("clear: %v", err)
		}
		if cleared.Attempts != 8 || !cleared.Parked || cleared.LastError != "seeded failure" || seen != cleared {
			t.Fatalf("cleared=%+v hook saw %+v", cleared, seen)
		}
		if got := s.rawRecord(id); got != blank {
			t.Fatalf("columns after clear: %s", got)
		}
		if s.get(id).BillingFailure != nil {
			t.Fatal("record still live after clear")
		}
		// A second click finds nothing to retry.
		_, err = s.store.ClearBillingFailure(s.ctx, s.tenantID, id, nil)
		if !errors.Is(err, errs.ErrInvalidState) || errs.Code(err) != "billing_not_failing" {
			t.Fatalf("second clear: %v (code %q), want InvalidState billing_not_failing", err, errs.Code(err))
		}
	})

	t.Run("hook fails: the record is not cleared", func(t *testing.T) {
		id := s.sub(bfRow{id: "sub_clr_hook", nextBilling: due, failedFor: &due, count: 8})
		before := s.rawRecord(id)
		boom := errors.New("audit insert failed (test)")
		_, err := s.store.ClearBillingFailure(s.ctx, s.tenantID, id, func(*sql.Tx, domain.BillingFailure) error { return boom })
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the hook's error", err)
		}
		if got := s.rawRecord(id); got != before {
			t.Fatalf("the clear survived a failed hook: %s → %s", before, got)
		}
	})

	notFailing := []bfRow{
		{id: "sub_clr_healthy", nextBilling: due},
		{id: "sub_clr_stale", nextBilling: due, failedFor: ptr(due.Add(-30 * 24 * time.Hour)), count: 8},
		{id: "sub_clr_clock", nextBilling: s.frozen.Add(-time.Hour), clockID: s.clockID, failedFor: ptr(s.frozen.Add(-time.Hour)), count: 8},
		{id: "sub_clr_canceled", status: "canceled", nextBilling: due, failedFor: &due, count: 8},
	}
	for _, r := range notFailing {
		t.Run("no live record: "+r.id, func(t *testing.T) {
			id := s.sub(r)
			before := s.rawRecord(id)
			called := false
			_, err := s.store.ClearBillingFailure(s.ctx, s.tenantID, id, func(*sql.Tx, domain.BillingFailure) error { called = true; return nil })
			if !errors.Is(err, errs.ErrInvalidState) || errs.Code(err) != "billing_not_failing" {
				t.Fatalf("err = %v (code %q), want InvalidState billing_not_failing", err, errs.Code(err))
			}
			if called {
				t.Fatal("the hook ran with nothing cleared — an audit row would be written for a no-op")
			}
			if got := s.rawRecord(id); got != before {
				t.Fatalf("columns changed: %s → %s", before, got)
			}
		})
	}

	t.Run("missing subscription", func(t *testing.T) {
		_, err := s.store.ClearBillingFailure(s.ctx, s.tenantID, "sub_clr_nope", nil)
		if !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

// TestList_BillingFailingFilter: GET /v1/subscriptions?billing=failing returns
// exactly the subscriptions with a live record, retrying and parked, and its
// total counts the same set. Rows that only look like failures are left out: a
// record for a due instant the row no longer has, a record on a clock-pinned
// row, and the columns a canceled subscription keeps as evidence.
func TestList_BillingFailingFilter(t *testing.T) {
	s := newBFSeed(t, "list")
	due := s.now.Add(-48 * time.Hour)
	next := s.now.Add(time.Hour)
	onClock := s.frozen.Add(-time.Hour)
	s.sub(bfRow{id: "sub_ls_healthy", nextBilling: due})
	s.sub(bfRow{id: "sub_ls_retrying", nextBilling: due, failedFor: &due, count: 2, nextAttempt: &next})
	s.sub(bfRow{id: "sub_ls_parked", nextBilling: due, failedFor: &due, count: 8})
	s.sub(bfRow{id: "sub_ls_stale", nextBilling: due, failedFor: ptr(due.Add(-30 * 24 * time.Hour)), count: 8})
	s.sub(bfRow{id: "sub_ls_clock", nextBilling: onClock, clockID: s.clockID, failedFor: &onClock, count: 8})
	s.sub(bfRow{id: "sub_ls_canceled", status: "canceled", nextBilling: due, failedFor: &due, count: 8})

	subs, total, err := s.store.List(s.ctx, subscription.ListFilter{
		TenantID: s.tenantID, Billing: subscription.ListBillingFailing, Sort: "display_name", SortDir: "asc",
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := ids(subs); total != 2 || len(got) != 2 || got[0] != "sub_ls_parked" || got[1] != "sub_ls_retrying" {
		t.Fatalf("billing=failing returned %v (total %d), want [sub_ls_parked sub_ls_retrying] and 2", got, total)
	}
	for _, sub := range subs {
		if sub.BillingFailure == nil {
			t.Fatalf("%s is in the failing list but reads no billing_failure", sub.ID)
		}
	}
	// It composes with the exact-match status filter.
	if _, total, err := s.store.List(s.ctx, subscription.ListFilter{TenantID: s.tenantID, Billing: subscription.ListBillingFailing, Status: "trialing"}); err != nil || total != 0 {
		t.Fatalf("billing=failing&status=trialing: total=%d err=%v, want 0", total, err)
	}
	// Control: no filter returns all six.
	if _, total, err := s.store.List(s.ctx, subscription.ListFilter{TenantID: s.tenantID}); err != nil || total != 6 {
		t.Fatalf("unfiltered: total=%d err=%v, want 6", total, err)
	}
}
