package subscription

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// extendAfterList is the real store, except that each trial list is followed
// by an operator's Extend trial on the listed rows, committed before the job
// flips them.
type extendAfterList struct {
	*PostgresStore
	t      *testing.T
	newEnd time.Time
}

func (e extendAfterList) ListExpiredTrials(ctx context.Context, before time.Time, livemode bool, limit int) ([]domain.Subscription, error) {
	subs, err := e.PostgresStore.ListExpiredTrials(ctx, before, livemode, limit)
	for _, sub := range subs {
		periodEnd := e.newEnd.AddDate(0, 1, 0)
		if _, err := e.ExtendTrial(ctx, sub.TenantID, sub.ID, e.newEnd, e.newEnd, periodEnd, e.newEnd, e.newEnd.Day()); err != nil {
			e.t.Fatalf("extend trial: %v", err)
		}
	}
	return subs, err
}

// An Extend trial that commits after the trial job listed the row and before
// it flips it moves the trial's end past the time the job acts for. The flip
// proves that end, so the subscription stays trialing with its new end: no
// activation and no day-1 invoice at the old end, and no error.
func TestTrialJobs_ExtendBetweenListAndFlip(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Trial Extend Race")
	store := NewPostgresStore(db)
	newEnd := time.Now().UTC().Add(7 * 24 * time.Hour).Truncate(time.Microsecond)
	biller := &fakeBiller{}

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

	if len(errs) > 0 || n != 0 {
		t.Errorf("trials ended = %d, errors %v; want 0 and none: the trial was extended", n, errs)
	}
	sub, err := store.Get(ctx, tenantID, id)
	if err != nil || sub.Status != domain.SubscriptionTrialing || sub.TrialEndAt == nil || !sub.TrialEndAt.Equal(newEnd) {
		t.Errorf("sub after the race: status %q trial end %v err %v, want trialing until %s", sub.Status, sub.TrialEndAt, err, newEnd)
	}
	if biller.createTxCalls != 0 {
		t.Errorf("day-1 invoices billed = %d, want 0", biller.createTxCalls)
	}
}
