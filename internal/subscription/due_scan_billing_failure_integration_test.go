package subscription_test

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// bfRow is one subscriptions row for the billing-failure store tests, seeded
// by raw SQL so a test can put the row in any shape, including shapes no
// writer produces (a record on a clock-pinned row, a stale record).
type bfRow struct {
	id          string
	status      string // default active
	nextBilling time.Time
	cancelAt    *time.Time
	trialEnd    *time.Time
	clockID     any // nil = wall clock

	// The saved record. failedFor nil = no record.
	failedFor   *time.Time
	count       int
	nextAttempt *time.Time
}

// bfSeed is the fixture: one test-mode tenant, one customer, one ready test
// clock frozen ten days ago.
type bfSeed struct {
	t        *testing.T
	db       *postgres.DB
	admin    *sql.DB
	ctx      context.Context
	store    *subscription.PostgresStore
	tenantID string
	clockID  string
	now      time.Time
	frozen   time.Time
}

func newBFSeed(t *testing.T, suffix string) *bfSeed {
	t.Helper()
	db := testutil.SetupTestDB(t)
	// One owner-role pool for the whole test: AdminPool opens a new pool on
	// every call and closes it only when the top-level test ends.
	admin := testutil.AdminPool(t)
	s := &bfSeed{
		t: t, db: db, admin: admin,
		ctx:      leadertest.Token(t, admin, postgres.WithLivemode(context.Background(), false), leader.RoleBilling),
		store:    subscription.NewPostgresStore(db),
		tenantID: "vlx_ten_bf" + suffix,
		clockID:  "vlx_tclk_bf" + suffix,
		now:      time.Now().UTC().Truncate(time.Microsecond),
	}
	s.frozen = s.now.Add(-10 * 24 * time.Hour)
	s.exec(`INSERT INTO tenants (id, name, status) VALUES ($1, 'BillingFailure', 'active')`, s.tenantID)
	s.exec(`INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode)
		VALUES ($1, $2, 'bf', $3, 'ready', false)`, s.clockID, s.tenantID, s.frozen)
	s.exec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email, created_at, updated_at)
		VALUES ($1, $2, 'bf', 'BillingFailure', '', $3, $3)`, "cus_bf"+suffix, s.tenantID, s.now)
	return s
}

// exec runs one statement as the table owner in the test-mode partition.
func (s *bfSeed) exec(q string, args ...any) {
	s.t.Helper()
	tx, err := s.db.BeginTx(s.ctx, postgres.TxBypass, "")
	if err != nil {
		s.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(s.ctx, `SELECT set_config('app.livemode', 'off', true)`); err != nil {
		s.t.Fatalf("livemode: %v", err)
	}
	if _, err := tx.ExecContext(s.ctx, q, args...); err != nil {
		s.t.Fatalf("seed: %v\n%s", err, q)
	}
	if err := tx.Commit(); err != nil {
		s.t.Fatalf("commit: %v", err)
	}
}

func (s *bfSeed) sub(r bfRow) string {
	s.t.Helper()
	if r.status == "" {
		r.status = "active"
	}
	var trialStart any
	if r.trialEnd != nil {
		trialStart = r.trialEnd.Add(-14 * 24 * time.Hour)
	}
	var failedAt any
	var lastErr any
	if r.failedFor != nil {
		failedAt, lastErr = s.now.Add(-time.Hour), "seeded failure"
	}
	s.exec(`INSERT INTO subscriptions (
			id, tenant_id, code, display_name, customer_id, status, billing_time,
			current_billing_period_start, current_billing_period_end, next_billing_at,
			cancel_at, trial_start_at, trial_end_at,
			livemode, test_clock_id,
			billing_failed_for, billing_failure_count, billing_failed_at, billing_next_attempt_at, billing_last_error,
			created_at, updated_at
		) VALUES ($1, $2, $1, $1, (SELECT id FROM customers WHERE tenant_id = $2 LIMIT 1), $3, 'calendar',
		          $4, $5, $5, $6, $7, $8, false, $9, $10, $11, $12, $13, $14, $4, $4)`,
		r.id, s.tenantID, r.status, r.nextBilling.Add(-30*24*time.Hour), r.nextBilling,
		postgres.NullableTime(r.cancelAt), trialStart, postgres.NullableTime(r.trialEnd), r.clockID,
		postgres.NullableTime(r.failedFor), r.count, failedAt, postgres.NullableTime(r.nextAttempt), lastErr)
	return r.id
}

// sqlFailing evaluates the shared SQL predicate for one row.
func (s *bfSeed) sqlFailing(id string) bool {
	s.t.Helper()
	var failing bool
	err := s.admin.QueryRowContext(s.ctx,
		`SELECT `+domain.SubscriptionBillingFailingSQL("s")+` FROM subscriptions s WHERE s.id = $1`, id).Scan(&failing)
	if err != nil {
		s.t.Fatalf("predicate for %s: %v", id, err)
	}
	return failing
}

// get reads the row through the store and asserts that the Go rule
// (Get().BillingFailure) and the SQL predicate agree about it.
func (s *bfSeed) get(id string) domain.Subscription {
	s.t.Helper()
	sub, err := s.store.Get(s.ctx, s.tenantID, id)
	if err != nil {
		s.t.Fatalf("get %s: %v", id, err)
	}
	if sqlSays := s.sqlFailing(id); sqlSays != (sub.BillingFailure != nil) {
		s.t.Fatalf("%s: SQL predicate says failing=%v but Get().BillingFailure=%+v — the two forms of the rule disagree", id, sqlSays, sub.BillingFailure)
	}
	return sub
}

// rawRecord reads the five columns as text.
func (s *bfSeed) rawRecord(id string) string {
	s.t.Helper()
	var out string
	err := s.admin.QueryRowContext(s.ctx, `
		SELECT concat_ws('|', COALESCE(billing_failed_for::text,'-'), billing_failure_count::text,
		       COALESCE(billing_failed_at::text,'-'), COALESCE(billing_next_attempt_at::text,'-'), COALESCE(billing_last_error,'-'))
		  FROM subscriptions WHERE id = $1`, id).Scan(&out)
	if err != nil {
		s.t.Fatalf("raw record of %s: %v", id, err)
	}
	return out
}

func ptr(t time.Time) *time.Time { return &t }

// TestDueScans_WaitingAndParkedAreSkippedBeforeLimit locks the billing-failure
// gate in the three wall-clock scans (ADR-119). The oldest due sub is waiting
// and the second is parked; each scan, asked for ONE row, must return the
// third. Once the run's instant passes the wait, the first comes back. The
// clock scan is the control: it carries no gate and returns a row with a
// parked record.
//
// Mutation seam: drop domain.SubscriptionBillingGateSQL from any gated scan
// and its subtest fails.
func TestDueScans_WaitingAndParkedAreSkippedBeforeLimit(t *testing.T) {
	s := newBFSeed(t, "gate")
	old := func(days int) time.Time { return s.now.Add(-time.Duration(days) * 24 * time.Hour) }

	s.sub(bfRow{id: "sub_bf_waiting", nextBilling: old(3000), failedFor: ptr(old(3000)), count: 2, nextAttempt: ptr(s.now.Add(time.Hour))})
	s.sub(bfRow{id: "sub_bf_parked", nextBilling: old(2999), failedFor: ptr(old(2999)), count: 8})
	s.sub(bfRow{id: "sub_bf_healthy", nextBilling: old(2998)})

	s.sub(bfRow{id: "sub_bf_trial_parked", status: "trialing", nextBilling: old(1), trialEnd: ptr(old(3000)), failedFor: ptr(old(1)), count: 8})
	s.sub(bfRow{id: "sub_bf_trial_healthy", status: "trialing", nextBilling: old(1), trialEnd: ptr(old(2999))})
	// Parked like the first, but a cancel is scheduled for the trial end. The
	// free trial-end cancel is not the work that failed, so the trial scan
	// must still hand this row to it.
	s.sub(bfRow{id: "sub_bf_trial_parked_cancel", status: "trialing", nextBilling: old(1), trialEnd: ptr(old(2998)), cancelAt: ptr(old(2998)), failedFor: ptr(old(1)), count: 8})

	s.sub(bfRow{id: "sub_bf_clk_parked", clockID: s.clockID, nextBilling: s.frozen.Add(-time.Hour), failedFor: ptr(s.frozen.Add(-time.Hour)), count: 8})

	// The trialing rows are due for billing too (next_billing_at a day ago),
	// so the billing scans are asked about the three active rows by name.
	active := func(subs []domain.Subscription) []string {
		var out []string
		for _, sub := range subs {
			if sub.Status == domain.SubscriptionActive {
				out = append(out, sub.ID)
			}
		}
		return out
	}

	scans := []struct {
		name string
		scan func(at time.Time, limit int) ([]domain.Subscription, error)
	}{
		{"GetDueBilling", func(at time.Time, limit int) ([]domain.Subscription, error) {
			return s.store.GetDueBilling(s.ctx, at, at, nil, limit)
		}},
		{"GetDueBillingForTenant", func(at time.Time, limit int) ([]domain.Subscription, error) {
			return s.store.GetDueBillingForTenant(s.ctx, s.tenantID, at, at, nil, limit)
		}},
	}
	for _, c := range scans {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.scan(s.now, 1)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if len(got) != 1 || got[0].ID != "sub_bf_healthy" {
				t.Fatalf("limit 1 at now: got %v, want [sub_bf_healthy] — the waiting and parked subs must be skipped before the LIMIT", ids(got))
			}
			got, err = c.scan(s.now, 50)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if a := active(got); !slices.Equal(a, []string{"sub_bf_healthy"}) {
				t.Fatalf("full page at now: active rows %v, want only sub_bf_healthy", a)
			}
			// The wait has passed: the oldest sub is first again. The parked
			// one never comes back.
			got, err = c.scan(s.now.Add(2*time.Hour), 50)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if a := active(got); !slices.Equal(a, []string{"sub_bf_waiting", "sub_bf_healthy"}) {
				t.Fatalf("after the wait: active rows %v, want [sub_bf_waiting sub_bf_healthy]", a)
			}
			// A next attempt exactly at the run's instant is due (not after it).
			got, err = c.scan(s.now.Add(time.Hour), 1)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if len(got) != 1 || got[0].ID != "sub_bf_waiting" {
				t.Fatalf("at exactly next_attempt_at: got %v, want [sub_bf_waiting]", ids(got))
			}
		})
	}

	t.Run("ListExpiredTrials", func(t *testing.T) {
		got, err := s.store.ListExpiredTrials(s.ctx, s.now, false, 50)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if g := ids(got); !slices.Equal(g, []string{"sub_bf_trial_healthy", "sub_bf_trial_parked_cancel"}) {
			t.Fatalf("got %v, want [sub_bf_trial_healthy sub_bf_trial_parked_cancel]: a parked trialing sub must not be activated by the trial scan, but one with a cancel due at trial end must still reach the free cancel", g)
		}
		if s.get("sub_bf_trial_parked_cancel").BillingFailure == nil {
			t.Fatal("setup: the cancel-scheduled trialing row must still carry its live record")
		}
	})

	t.Run("control: GetDueBillingForClock carries no gate", func(t *testing.T) {
		got, err := s.store.GetDueBillingForClock(s.ctx, s.tenantID, s.clockID, nil, 50)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if g := ids(got); !slices.Equal(g, []string{"sub_bf_clk_parked"}) {
			t.Fatalf("got %v, want [sub_bf_clk_parked]: a record on a clock-pinned row must not hide it from Advance", g)
		}
		if bf := s.get("sub_bf_clk_parked").BillingFailure; bf != nil {
			t.Fatalf("a clock-pinned row read back a billing failure: %+v", bf)
		}
	})
}

// TestDueScans_RecordForAnotherDueInstantDoesNotGate proves that work which
// never failed cannot be skipped: a parked record saved for a due instant the
// row no longer has gates nothing. Two ways the due instant moves without the
// record being touched: the period advanced, and a mid-period cancel that was
// failing was un-scheduled.
//
// Mutation seam: replace the key test in domain.SubscriptionBillingFailingSQL
// with `billing_failure_count > 0` and every subtest fails.
func TestDueScans_RecordForAnotherDueInstantDoesNotGate(t *testing.T) {
	s := newBFSeed(t, "key")
	old := func(days int) time.Time { return s.now.Add(-time.Duration(days) * 24 * time.Hour) }

	// The record is for the previous close, a month before the one now due.
	s.sub(bfRow{id: "sub_bf_moved", nextBilling: old(10), failedFor: ptr(old(40)), count: 8})
	// The record is for a mid-period cancel at -45d that has since been
	// cleared (cancel_at is NULL); the close at -5d is other work.
	s.sub(bfRow{id: "sub_bf_uncancel", nextBilling: old(5), failedFor: ptr(old(45)), count: 8})
	s.sub(bfRow{id: "sub_bf_trial_moved", status: "trialing", nextBilling: old(1), trialEnd: ptr(old(2)), failedFor: ptr(old(31)), count: 8})

	for _, id := range []string{"sub_bf_moved", "sub_bf_uncancel", "sub_bf_trial_moved"} {
		if bf := s.get(id).BillingFailure; bf != nil {
			t.Fatalf("%s reads a billing failure from a record for another due instant: %+v", id, bf)
		}
	}

	want := []string{"sub_bf_moved", "sub_bf_uncancel", "sub_bf_trial_moved"}
	t.Run("GetDueBilling", func(t *testing.T) {
		got, err := s.store.GetDueBilling(s.ctx, s.now, s.now, nil, 50)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if g := ids(got); !slices.Equal(g, want) {
			t.Fatalf("got %v, want %v", g, want)
		}
	})
	t.Run("GetDueBillingForTenant", func(t *testing.T) {
		got, err := s.store.GetDueBillingForTenant(s.ctx, s.tenantID, s.now, s.now, nil, 50)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if g := ids(got); !slices.Equal(g, want) {
			t.Fatalf("got %v, want %v", g, want)
		}
	})
	t.Run("ListExpiredTrials", func(t *testing.T) {
		got, err := s.store.ListExpiredTrials(s.ctx, s.now, false, 50)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if g := ids(got); !slices.Equal(g, []string{"sub_bf_trial_moved"}) {
			t.Fatalf("got %v, want [sub_bf_trial_moved]", g)
		}
	})
}
