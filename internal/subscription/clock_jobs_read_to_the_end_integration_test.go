package subscription

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/walk"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// A clock's trial and pause jobs read every due row in one call: a clock has
// no next tick, so a job that stopped after one page left the clock ready
// with rows undone. Two pages and one row more prove the cursor moves on.
const clockRows = 2*100 + 1

// seedClockSubs inserts n subscriptions on one test clock, each with its own
// customer, and returns the clock and the subscription ids. trialing makes
// each a trial that ended an hour ago; otherwise each is active with a
// collection pause that ended an hour ago.
func seedClockSubs(t *testing.T, db *postgres.DB, ctx context.Context, tenantID string, n int, trialing bool) (string, time.Time, []string) {
	t.Helper()
	frozen := time.Now().UTC().Truncate(time.Microsecond)
	ended := frozen.Add(-time.Hour)
	clockID := postgres.NewID("hc_tclk")
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status) VALUES ($1, $2, 'drain', $3, 'advancing')`,
		clockID, tenantID, frozen); err != nil {
		t.Fatalf("seed clock: %v", err)
	}
	ids := make([]string, n)
	for i := range n {
		custID, subID := postgres.NewID("hc_cus"), postgres.NewID("hc_sub")
		ids[i] = subID
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO customers (id, tenant_id, external_id, display_name, email, test_clock_id, created_at, updated_at)
			VALUES ($1, $2, $3, 'Drain', '', $4, $5, $5)`, custID, tenantID, fmt.Sprintf("drain-%d-%s", i, subID), clockID, frozen); err != nil {
			t.Fatalf("seed customer: %v", err)
		}
		status, periodEnd := "active", frozen.Add(30*24*time.Hour)
		var trialStart, trialEnd, pauseBehavior, pauseResumes any
		if trialing {
			status, trialStart, trialEnd = "trialing", ended.Add(-14*24*time.Hour), ended
			periodEnd = ended
		} else {
			pauseBehavior, pauseResumes = "keep_as_draft", ended
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO subscriptions (
				id, tenant_id, code, display_name, customer_id, test_clock_id, status, billing_time,
				trial_start_at, trial_end_at,
				current_billing_period_start, current_billing_period_end, next_billing_at,
				pause_collection_behavior, pause_collection_resumes_at, created_at, updated_at
			) VALUES ($1, $2, $3, 'Drain Sub', $4, $5, $6, 'anniversary', $7, $8, $9, $10, $10, $11, $12, $9, $9)`,
			subID, tenantID, "code-"+subID, custID, clockID, status, trialStart, trialEnd,
			frozen.Add(-24*time.Hour), periodEnd, pauseBehavior, pauseResumes); err != nil {
			t.Fatalf("seed subscription: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return clockID, frozen, ids
}

func TestProcessExpiredTrialsForClock_DoesEveryPage(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Trials Every Page")
	store := NewPostgresStore(db)
	clockID, frozen, ids := seedClockSubs(t, db, ctx, tenantID, clockRows, true)

	svc := NewService(store, clock.Real())
	n, errs := svc.ProcessExpiredTrialsForClock(ctx, tenantID, clockID, frozen)
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	if n != clockRows {
		t.Errorf("trials ended = %d, want %d: the job stopped before the last page", n, clockRows)
	}
	for _, id := range []string{ids[0], ids[100], ids[clockRows-1]} {
		if sub, err := store.Get(ctx, tenantID, id); err != nil || sub.Status != domain.SubscriptionActive {
			t.Errorf("sub %s: status %q err %v, want active", id, sub.Status, err)
		}
	}
}

func TestProcessExpiredPauseCollectionsForClock_DoesEveryPage(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Pauses Every Page")
	store := NewPostgresStore(db)
	clockID, frozen, ids := seedClockSubs(t, db, ctx, tenantID, clockRows, false)

	svc := NewService(store, clock.Real())
	n, errs := svc.ProcessExpiredPauseCollectionsForClock(ctx, tenantID, clockID, frozen)
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	if n != clockRows {
		t.Errorf("pauses resumed = %d, want %d: the job stopped before the last page", n, clockRows)
	}
	for _, id := range []string{ids[0], ids[100], ids[clockRows-1]} {
		if sub, err := store.Get(ctx, tenantID, id); err != nil || sub.PauseCollection != nil {
			t.Errorf("sub %s: pause %+v err %v, want resumed", id, sub.PauseCollection, err)
		}
	}
}

// Walking the clock's pause list page by page, as the job does, returns
// every due pause exactly once when the order they come due in disagrees with
// the order they were inserted in (and so with id order) and with the order
// they were touched in: the last inserted come due first, and the first-due
// pauses were touched last.
// The page's order and the cursor must both be (resumes_at, id); a page in
// another order would end on a late-due row and leave the earlier ones
// behind the cursor.
func TestListExpiredPauseCollectionsForClock_WalkReturnsEveryRowOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Pauses Walk")
	store := NewPostgresStore(db)
	const n, page = 5, 2
	clockID, frozen, ids := seedClockSubs(t, db, ctx, tenantID, n, false)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	base := frozen.Add(-24 * time.Hour)
	due := func(i int) int { return n - 1 - i } // the last inserted come due first
	for i, id := range ids {
		at := base.Add(time.Duration(due(i)) * time.Minute)
		updated := at
		if due(i) < 3 {
			updated = updated.Add(time.Hour) // touched last
		}
		if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET pause_collection_resumes_at = $2, updated_at = $3 WHERE id = $1`,
			id, at, updated); err != nil {
			t.Fatalf("set times: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	seen := map[string]int{}
	var afterAt time.Time
	afterID := ""
	for range n + 1 {
		got, err := store.ListExpiredPauseCollectionsForClock(ctx, tenantID, clockID, frozen, afterAt, afterID, page)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, sub := range got {
			seen[sub.ID]++
		}
		if len(got) < page {
			break
		}
		last := got[len(got)-1]
		afterAt, afterID = *last.PauseCollection.ResumesAt, last.ID
	}
	for i, id := range ids {
		if seen[id] != 1 {
			t.Errorf("pause due %d of %d returned %d times, want once", due(i)+1, n, seen[id])
		}
	}
}

// The clock's trial list walks the same way, by (trial_end_at, id): the last
// inserted trials end first, and the first to end were touched last.
func TestListExpiredTrialsForClock_WalkReturnsEveryRowOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Trials Walk")
	store := NewPostgresStore(db)
	const n, page = 5, 2
	clockID, frozen, ids := seedClockSubs(t, db, ctx, tenantID, n, true)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	base := frozen.Add(-24 * time.Hour)
	ends := func(i int) int { return n - 1 - i } // the last inserted end first
	for i, id := range ids {
		at := base.Add(time.Duration(ends(i)) * time.Minute)
		updated := at
		if ends(i) < 3 {
			updated = updated.Add(time.Hour) // touched last
		}
		if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET trial_end_at = $2, updated_at = $3 WHERE id = $1`,
			id, at, updated); err != nil {
			t.Fatalf("set times: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Walked as the job walks it, with its key.
	seen := map[string]int{}
	list := func(ctx context.Context, after walk.Key, limit int) ([]domain.Subscription, error) {
		return store.ListExpiredTrialsForClock(ctx, tenantID, clockID, frozen, after.At, after.ID, limit)
	}
	for sub, err := range walk.Walk(ctx, "ListExpiredTrialsForClock", list, expiredTrialKey, page) {
		if err != nil {
			t.Fatalf("walk: %v", err)
		}
		seen[sub.ID]++
	}
	for i, id := range ids {
		if seen[id] != 1 {
			t.Errorf("trial ending %d of %d returned %d times, want once", ends(i)+1, n, seen[id])
		}
	}
}

// A row another transaction holds is waited for, not skipped: a clock has
// one runner, and a skipped row would wait for a later pass, if one came.
func TestProcessExpiredTrialsForClock_WaitsForALockedRow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Trials Locked Row")
	store := NewPostgresStore(db)
	clockID, frozen, ids := seedClockSubs(t, db, ctx, tenantID, 3, true)

	holder, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer postgres.Rollback(holder)
	if _, err := holder.ExecContext(ctx, `SELECT id FROM subscriptions WHERE id = $1 FOR UPDATE`, ids[1]); err != nil {
		t.Fatalf("lock row: %v", err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = holder.Commit()
	}()

	svc := NewService(store, clock.Real())
	n, errs := svc.ProcessExpiredTrialsForClock(ctx, tenantID, clockID, frozen)
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	if n != 3 {
		t.Errorf("trials ended = %d, want 3: the locked row was skipped", n)
	}
	if sub, err := store.Get(ctx, tenantID, ids[1]); err != nil || sub.Status != domain.SubscriptionActive {
		t.Errorf("locked sub: status %q err %v, want active", sub.Status, err)
	}
}

// Each clock list returns a row another transaction holds, so its job waits
// for that row at its write instead of skipping it.
func TestClockLists_IncludeARowAnotherTransactionHolds(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Lists Held Row")
	store := NewPostgresStore(db)
	trialClock, trialFrozen, trialIDs := seedClockSubs(t, db, ctx, tenantID, 3, true)
	pauseClock, pauseFrozen, pauseIDs := seedClockSubs(t, db, ctx, tenantID, 3, false)

	holder, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer postgres.Rollback(holder)
	if _, err := holder.ExecContext(ctx, `SELECT id FROM subscriptions WHERE id = ANY($1) FOR UPDATE`, []string{trialIDs[1], pauseIDs[1]}); err != nil {
		t.Fatalf("lock rows: %v", err)
	}

	lists := map[string]func() ([]domain.Subscription, error){
		"trials": func() ([]domain.Subscription, error) {
			return store.ListExpiredTrialsForClock(ctx, tenantID, trialClock, trialFrozen, time.Time{}, "", 100)
		},
		"pauses": func() ([]domain.Subscription, error) {
			return store.ListExpiredPauseCollectionsForClock(ctx, tenantID, pauseClock, pauseFrozen, time.Time{}, "", 100)
		},
		"period closes": func() ([]domain.Subscription, error) {
			return store.GetDueBillingForClock(ctx, tenantID, trialClock, nil, 100)
		},
	}
	for name, list := range lists {
		subs, err := list()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(subs) != 3 {
			t.Errorf("%s list returned %d rows, want 3: the held row was skipped", name, len(subs))
		}
	}
}

// runEnds runs job with a deadline and fails the test if the job is still
// running when it passes.
func runEnds(t *testing.T, ctx context.Context, job func(context.Context) (int, []error)) []error {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	type result struct{ errs []error }
	done := make(chan result, 1)
	go func() { _, errs := job(ctx); done <- result{errs} }()
	select {
	case r := <-done:
		return r.errs
	case <-ctx.Done():
		t.Fatal("the job did not end: it read the rows it left as they were again")
		return nil
	}
}

// A row the job leaves as it was stays in the list. The cursor moves past
// it, so each row is tried once and the job ends with one error per row.
func TestProcessExpiredTrialsForClock_RowsThatStayListed(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Trials Stay Listed")
	clockID, frozen, _ := seedClockSubs(t, db, ctx, tenantID, clockRows, true)
	svc := NewService(NewPostgresStore(db), clock.Real())
	svc.SetBiller(&fakeBiller{createTxErr: errors.New("day-1 invoice refused")})

	errs := runEnds(t, ctx, func(ctx context.Context) (int, []error) {
		return svc.ProcessExpiredTrialsForClock(ctx, tenantID, clockID, frozen)
	})
	if len(errs) != clockRows {
		t.Errorf("errors = %d, want %d: one per row, each row tried once", len(errs), clockRows)
	}
}

func TestProcessExpiredPauseCollectionsForClock_RowsThatStayListed(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Pauses Stay Listed")
	clockID, frozen, _ := seedClockSubs(t, db, ctx, tenantID, clockRows, false)
	svc := NewService(NewPostgresStore(db), clock.Real())
	svc.SetAuditLogger(failingAudit{})

	errs := runEnds(t, ctx, func(ctx context.Context) (int, []error) {
		return svc.ProcessExpiredPauseCollectionsForClock(ctx, tenantID, clockID, frozen)
	})
	if len(errs) != clockRows {
		t.Errorf("errors = %d, want %d: one per row, each row tried once", len(errs), clockRows)
	}
}

// extendAfterList is the real store, except that each trial list is followed
// by an operator's Extend trial on the listed rows, committed before the job
// flips them.
type extendAfterList struct {
	*PostgresStore
	t      *testing.T
	newEnd time.Time
}

func (e extendAfterList) extend(ctx context.Context, subs []domain.Subscription) {
	for _, sub := range subs {
		periodEnd := e.newEnd.AddDate(0, 1, 0)
		if _, err := e.ExtendTrial(ctx, sub.TenantID, sub.ID, e.newEnd, e.newEnd, periodEnd, e.newEnd, e.newEnd.Day()); err != nil {
			e.t.Fatalf("extend trial: %v", err)
		}
	}
}

func (e extendAfterList) ListExpiredTrialsForClock(ctx context.Context, tenantID, clockID string, frozen, afterAt time.Time, afterID string, limit int) ([]domain.Subscription, error) {
	subs, err := e.PostgresStore.ListExpiredTrialsForClock(ctx, tenantID, clockID, frozen, afterAt, afterID, limit)
	e.extend(ctx, subs)
	return subs, err
}

func (e extendAfterList) ListExpiredTrials(ctx context.Context, before time.Time, livemode bool, limit int) ([]domain.Subscription, error) {
	subs, err := e.PostgresStore.ListExpiredTrials(ctx, before, livemode, limit)
	e.extend(ctx, subs)
	return subs, err
}

// An Extend trial that commits after a trial job listed the row and before it
// flips it moves the trial's end past the time the job acts for. The flip
// proves that end, so the subscription stays trialing with its new end: no
// activation and no day-1 invoice at the old end, and no error. On a clock
// and in real time alike.
func TestTrialJobs_ExtendBetweenListAndFlip(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Trial Extend Race")
	store := NewPostgresStore(db)
	newEnd := time.Now().UTC().Add(7 * 24 * time.Hour).Truncate(time.Microsecond)
	biller := &fakeBiller{}

	check := func(t *testing.T, id string, n int, errs []error) {
		t.Helper()
		if len(errs) > 0 || n != 0 {
			t.Errorf("trials ended = %d, errors %v; want 0 and none: the trial was extended", n, errs)
		}
		sub, err := store.Get(ctx, tenantID, id)
		if err != nil || sub.Status != domain.SubscriptionTrialing || sub.TrialEndAt == nil || !sub.TrialEndAt.Equal(newEnd) {
			t.Errorf("sub after the race: status %q trial end %v err %v, want trialing until %s", sub.Status, sub.TrialEndAt, err, newEnd)
		}
	}

	t.Run("clock", func(t *testing.T) {
		clockID, frozen, ids := seedClockSubs(t, db, ctx, tenantID, 1, true)
		svc := NewService(extendAfterList{PostgresStore: store, t: t, newEnd: newEnd}, clock.Real())
		svc.SetBiller(biller)
		n, errs := svc.ProcessExpiredTrialsForClock(ctx, tenantID, clockID, frozen)
		check(t, ids[0], n, errs)
	})
	t.Run("real time", func(t *testing.T) {
		custID, id := postgres.NewID("hc_cus"), postgres.NewID("hc_sub")
		ended := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx, `INSERT INTO customers (id, tenant_id, external_id, display_name, email, created_at, updated_at)
			VALUES ($1, $2, $3, 'Race', '', $4, $4)`, custID, tenantID, "race-"+id, ended); err != nil {
			t.Fatalf("seed customer: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, billing_time,
				trial_start_at, trial_end_at, current_billing_period_start, current_billing_period_end, next_billing_at, created_at, updated_at)
			VALUES ($1, $2, $3, 'Race Sub', $4, 'trialing', 'anniversary', $5, $6, $6, $7, $7, $5, $5)`,
			id, tenantID, "code-"+id, custID, ended.Add(-14*24*time.Hour), ended, ended.Add(30*24*time.Hour)); err != nil {
			t.Fatalf("seed subscription: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		svc := NewService(extendAfterList{PostgresStore: store, t: t, newEnd: newEnd}, clock.Real())
		svc.SetBiller(biller)
		n, errs := svc.ProcessExpiredTrials(ctx, 100)
		check(t, id, n, errs)
	})
	if biller.createTxCalls != 0 {
		t.Errorf("day-1 invoices billed = %d, want 0", biller.createTxCalls)
	}
}

// activationOrder is the real store; it records the order the job flips
// trials in.
type activationOrder struct {
	*PostgresStore
	ids []string
}

func (a *activationOrder) ActivateAfterTrialWithBill(ctx context.Context, tenantID, id string, at time.Time, billFn func(tx *sql.Tx, activated domain.Subscription) error) (domain.Subscription, error) {
	a.ids = append(a.ids, id)
	return a.PostgresStore.ActivateAfterTrialWithBill(ctx, tenantID, id, at, billFn)
}

// A clock ends its trials in the order they fell due, not in id order, so
// their day-1 invoices are numbered in that order too.
func TestProcessExpiredTrialsForClock_InDueOrder(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Trials Due Order")
	clockID, frozen, ids := seedClockSubs(t, db, ctx, tenantID, 3, true)
	// The later the id, the earlier the trial ended.
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET trial_end_at = $2 WHERE id = $1`, id, frozen.Add(-time.Duration(i+1)*time.Hour)); err != nil {
			t.Fatalf("set trial end: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	rec := &activationOrder{PostgresStore: NewPostgresStore(db)}
	if _, errs := NewService(rec, clock.Real()).ProcessExpiredTrialsForClock(ctx, tenantID, clockID, frozen); len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	if want := []string{ids[2], ids[1], ids[0]}; !slices.Equal(rec.ids, want) {
		t.Errorf("trials ended in order %v, want the order they fell due %v", rec.ids, want)
	}
}

// The flip itself refuses a trial whose end moved past the time it acts for,
// with the not-due error, not a generic invalid state.
func TestActivateAfterTrial_AfterAnExtendIsNotDue(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Trial Not Due")
	store := NewPostgresStore(db)
	_, frozen, ids := seedClockSubs(t, db, ctx, tenantID, 1, true)
	oldEnd := frozen.Add(-time.Hour)
	newEnd := frozen.Add(7 * 24 * time.Hour)
	if _, err := store.ExtendTrial(ctx, tenantID, ids[0], newEnd, newEnd, newEnd.AddDate(0, 1, 0), newEnd, newEnd.Day()); err != nil {
		t.Fatalf("extend trial: %v", err)
	}
	if _, err := store.ActivateAfterTrialWithBill(ctx, tenantID, ids[0], oldEnd, nil); !errors.Is(err, domain.ErrTrialNotDue) {
		t.Errorf("flip at the old end after an extend: err = %v, want ErrTrialNotDue", err)
	}
}
