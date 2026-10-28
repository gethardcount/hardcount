package subscription_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// seedTrialingSub inserts a trialing sub whose trial elapsed (trial_end 1h
// ago), with the given schedule fields.
func seedTrialingSub(t *testing.T, db *postgres.DB, ctx context.Context, tenantID string, cancelAtPeriodEnd bool, cancelAt *time.Time) (string, time.Time) {
	t.Helper()
	custID := postgres.NewID("hc_cus")
	subID := postgres.NewID("hc_sub")
	trialEnd := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Microsecond)
	trialStart := trialEnd.Add(-14 * 24 * time.Hour)
	periodEnd := trialEnd.Add(30 * 24 * time.Hour)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO customers (id, tenant_id, external_id, display_name, email, created_at, updated_at)
		VALUES ($1, $2, $3, 'Trial Cancel', '', $4, $4)
	`, custID, tenantID, "cus-"+subID, now); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscriptions (
			id, tenant_id, code, display_name, customer_id, status, billing_time,
			trial_start_at, trial_end_at,
			current_billing_period_start, current_billing_period_end, next_billing_at,
			cancel_at_period_end, cancel_at, created_at, updated_at
		) VALUES ($1, $2, $3, 'Trial Cancel Sub', $4, 'trialing', 'anniversary',
			$5, $6, $6, $7, $7, $8, $9, $10, $10)
	`, subID, tenantID, "code-"+subID, custID, trialStart, trialEnd, periodEnd,
		cancelAtPeriodEnd, postgres.NullableTime(cancelAt), now); err != nil {
		t.Fatalf("seed sub: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return subID, trialEnd
}

// TestActivateAfterTrial_SQLGuardBlocksScheduledCancel is the ADR-069 TOCTOU
// lock on real Postgres: an activation attempt against a trialing sub with a
// due cancel schedule must be BLOCKED IN SQL (ErrTrialCancelDue) — the
// pre-fix shape activated and billed a customer who canceled. Mutation seam:
// strip the schedule predicate from activateAfterTrialInTx and this fails by
// activating.
func TestActivateAfterTrial_SQLGuardBlocksScheduledCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := subscription.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Trial Guard Flag")

	subID, trialEnd := seedTrialingSub(t, db, ctx, tenantID, true, nil)
	_, err := store.ActivateAfterTrialWithBill(ctx, tenantID, subID, trialEnd, nil)
	if !errors.Is(err, domain.ErrTrialCancelDue) {
		t.Fatalf("activation with due flag schedule: err = %v, want ErrTrialCancelDue", err)
	}
	sub, gerr := store.Get(ctx, tenantID, subID)
	if gerr != nil {
		t.Fatalf("get: %v", gerr)
	}
	if sub.Status != domain.SubscriptionTrialing {
		t.Fatalf("status = %s — the guard failed and the sub activated", sub.Status)
	}

	// Explicit cancel_at == trial_end blocks the same way.
	subID2, trialEnd2 := seedTrialingSub(t, db, ctx, tenantID, false, &trialEnd)
	_ = trialEnd2
	if _, err := store.ActivateAfterTrialWithBill(ctx, tenantID, subID2, trialEnd, nil); !errors.Is(err, domain.ErrTrialCancelDue) {
		t.Fatalf("activation with due explicit schedule: err = %v, want ErrTrialCancelDue", err)
	}
}

// TestCancelAtTrialEnd_CASSemantics locks the dedicated transition: the
// happy path cancels free with canceled_at == trial_end_at and cleared
// schedule fields; a rescinded schedule or an extended trial makes the CAS
// lose (ErrTrialCancelConflict) — the customer is never terminated off a
// stale snapshot. Mutation seam: drop the schedule/trial_end predicates.
func TestCancelAtTrialEnd_CASSemantics(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := subscription.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Trial Cancel CAS")

	t.Run("happy path cancels free at trial_end", func(t *testing.T) {
		subID, trialEnd := seedTrialingSub(t, db, ctx, tenantID, true, nil)
		canceled, err := store.CancelAtTrialEnd(ctx, tenantID, subID, trialEnd)
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if canceled.Status != domain.SubscriptionCanceled {
			t.Fatalf("status = %s, want canceled", canceled.Status)
		}
		if canceled.CanceledAt == nil || !canceled.CanceledAt.Equal(trialEnd) {
			t.Fatalf("canceled_at = %v, want trial_end %v (never the observing site's now)", canceled.CanceledAt, trialEnd)
		}
		if canceled.CancelAt != nil || canceled.CancelAtPeriodEnd {
			t.Fatal("schedule fields must clear on the terminal transition")
		}
		// Idempotent re-entry: a second fire is a typed no-op.
		if _, err := store.CancelAtTrialEnd(ctx, tenantID, subID, trialEnd); !errors.Is(err, domain.ErrTrialCancelConflict) {
			t.Fatalf("second cancel: err = %v, want conflict no-op", err)
		}
	})

	t.Run("rescinded schedule wins", func(t *testing.T) {
		subID, trialEnd := seedTrialingSub(t, db, ctx, tenantID, true, nil)
		if _, err := store.ClearScheduledCancellation(ctx, tenantID, subID); err != nil {
			t.Fatalf("clear: %v", err)
		}
		if _, err := store.CancelAtTrialEnd(ctx, tenantID, subID, trialEnd); !errors.Is(err, domain.ErrTrialCancelConflict) {
			t.Fatalf("cancel after rescind: err = %v, want conflict (customer un-canceled in time)", err)
		}
	})

	t.Run("extended trial wins", func(t *testing.T) {
		subID, trialEnd := seedTrialingSub(t, db, ctx, tenantID, true, nil)
		// Simulate ExtendTrial committing between the scan snapshot and the
		// cancel fire: trial_end_at moves.
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		newEnd := trialEnd.Add(7 * 24 * time.Hour)
		if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET trial_end_at = $1 WHERE id = $2`, newEnd, subID); err != nil {
			t.Fatalf("extend: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if _, err := store.CancelAtTrialEnd(ctx, tenantID, subID, trialEnd); !errors.Is(err, domain.ErrTrialCancelConflict) {
			t.Fatalf("cancel with stale trial_end: err = %v, want conflict (extension honored)", err)
		}
	})
}

// TestScheduleCancel_RacesActivation_ExactlyOneMeaning: concurrent
// ScheduleCancel (validated under status=trialing) and activation — every
// interleaving ends in exactly one coherent outcome: either the schedule
// landed while trialing (activation then blocked → cancel due), or the
// activation won and the schedule 409s (status CAS). The forbidden state —
// active sub carrying a flag the operator was told meant "free at trial
// end" — must be unreachable.
func TestScheduleCancel_RacesActivation_ExactlyOneMeaning(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := subscription.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Sched vs Activate")

	for i := 0; i < 4; i++ {
		subID, trialEnd := seedTrialingSub(t, db, ctx, tenantID, false, nil)
		var schedErr, actErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, schedErr = store.ScheduleCancellation(ctx, tenantID, subID, nil, true, domain.SubscriptionTrialing)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, actErr = store.ActivateAfterTrialWithBill(ctx, tenantID, subID, trialEnd, nil)
		}()
		close(start)
		wg.Wait()

		sub, err := store.Get(ctx, tenantID, subID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		switch {
		case sub.Status == domain.SubscriptionTrialing && schedErr == nil && errors.Is(actErr, domain.ErrTrialCancelDue):
			// Schedule won; activation correctly blocked.
		case sub.Status == domain.SubscriptionActive && actErr == nil && errors.Is(schedErr, errs.ErrInvalidState):
			// Activation won; the trial-intent schedule correctly 409ed.
			if sub.CancelAtPeriodEnd {
				t.Fatalf("iteration %d: FORBIDDEN — active sub carries the trial-intent flag", i)
			}
		case sub.Status == domain.SubscriptionTrialing && schedErr == nil && actErr == nil:
			t.Fatalf("iteration %d: both writes claim success on a trialing row: schedErr=%v actErr=%v", i, schedErr, actErr)
		default:
			// Serialized orderings that are still coherent: schedule after
			// activation (schedErr 409, sub active, no flag) etc.
			if sub.Status == domain.SubscriptionActive && sub.CancelAtPeriodEnd {
				t.Fatalf("iteration %d: FORBIDDEN state (active + trial-intent flag); schedErr=%v actErr=%v", i, schedErr, actErr)
			}
		}
	}
}
