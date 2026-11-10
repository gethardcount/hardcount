package dunning_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The tests in this file race two changes to an account's default dunning
// policy, and each must end with no error and exactly one default policy in
// the mode. A test transaction holds the current default's row while both
// changes start, so both queue before either commits. That is the
// interleaving in which two unserialized changes miss each other's writes.
//
// Every writer of the default takes the per-account, per-mode lock first
// (lockDefaultPolicy in postgres.go), so the second change starts only after
// the first has committed, and reads its result.
//
// Mutation check: drop the lockDefaultPolicy call from SetDefaultPolicy,
// and both tests fail on the one-default unique index (a 500 at the API).
// Drop it from the create path instead, and the create test fails the same
// way.

// policyRace is one account in test mode, and the connections a race needs.
type policyRace struct {
	ctx      context.Context
	db       *postgres.DB
	admin    *sql.DB
	store    *dunning.PostgresStore
	tenantID string
}

func newPolicyRace(t *testing.T, name string) *policyRace {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	t.Cleanup(cancel)
	return &policyRace{
		ctx: ctx, db: db, admin: testutil.AdminPool(t),
		store: dunning.NewPostgresStore(db), tenantID: testutil.CreateTestTenant(t, db, name),
	}
}

func (r *policyRace) create(t *testing.T, name string, enabled bool) domain.DunningPolicy {
	t.Helper()
	p, err := r.store.UpsertPolicy(r.ctx, r.tenantID, racePolicy(name, enabled))
	if err != nil {
		t.Fatalf("create policy %s: %v", name, err)
	}
	return p
}

func racePolicy(name string, enabled bool) domain.DunningPolicy {
	return domain.DunningPolicy{
		Name: name, Enabled: enabled, RetryDays: []int{3, 4},
		FinalSubscriptionAction: domain.SubActionPause, FinalInvoiceAction: domain.InvActionNone,
	}
}

// holdPolicy opens a transaction that locks the policy's row until commit.
// A change that writes that row waits for it.
func (r *policyRace) holdPolicy(t *testing.T, id string) *sql.Tx {
	t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin holding tx: %v", err)
	}
	t.Cleanup(func() { postgres.Rollback(tx) })
	if _, err := tx.ExecContext(r.ctx, `SELECT 1 FROM dunning_policies WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatalf("lock policy %s: %v", id, err)
	}
	return tx
}

// start runs change in a goroutine, then waits until `waiting` sessions
// wait on a lock. Without the fix a change waits on the held row; with it,
// the first change waits on the row and the second on the account lock.
// Both match the empty pattern, which counts every waiting session in the
// test database.
func (r *policyRace) start(t *testing.T, waiting int, change func() error) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- change() }()
	testutil.WaitForLockWaiters(t, r.ctx, r.admin, "", waiting)
	return done
}

func waitNoError(t *testing.T, name string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("%s: %v, want no error", name, err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not return after the held row was released", name)
	}
}

// requireOneDefault fails unless exactly one policy in the mode is the
// default, and returns it.
func (r *policyRace) requireOneDefault(t *testing.T) domain.DunningPolicy {
	t.Helper()
	all, err := r.store.ListPolicies(r.ctx, r.tenantID)
	if err != nil {
		t.Fatalf("list policies: %v", err)
	}
	var defaults []domain.DunningPolicy
	for _, p := range all {
		if p.IsDefault {
			defaults = append(defaults, p)
		}
	}
	if len(defaults) != 1 {
		t.Fatalf("%d policies, %d of them default; want exactly one default", len(all), len(defaults))
	}
	return defaults[0]
}

// TestSetDefaultPolicy_ConcurrentFlips_LeaveOneDefault: two Set as default
// calls for different policies, at once, both succeed, and the policy set
// second is the one default. Without the lock, the second call's demote
// missed the first call's new default, and its promote failed on the unique
// index.
func TestSetDefaultPolicy_ConcurrentFlips_LeaveOneDefault(t *testing.T) {
	r := newPolicyRace(t, "Default policy flip race")
	current := r.create(t, "Current", true)
	if !current.IsDefault {
		t.Fatal("the first policy is not the default; the test needs it to be")
	}
	x := r.create(t, "X", true)
	y := r.create(t, "Y", true)

	hold := r.holdPolicy(t, current.ID)
	toX := r.start(t, 1, func() error { return r.store.SetDefaultPolicy(r.ctx, r.tenantID, x.ID) })
	toY := r.start(t, 2, func() error { return r.store.SetDefaultPolicy(r.ctx, r.tenantID, y.ID) })
	if err := hold.Commit(); err != nil {
		t.Fatalf("release held policy: %v", err)
	}
	waitNoError(t, "SetDefaultPolicy to X", toX)
	waitNoError(t, "SetDefaultPolicy to Y", toY)

	if got := r.requireOneDefault(t); got.ID != y.ID {
		t.Errorf("default is %q, want Y, the policy set second", got.Name)
	}
}

// TestCreatePolicy_RacingAFlip_LeaveOneDefault: the account's first enabled
// policy, which takes the default from the seeded one (ADR-123), created
// while Set as default runs for another policy. Both succeed with one
// default. Without the lock, the flip's demote missed the new policy, and
// its promote failed on the unique index.
func TestCreatePolicy_RacingAFlip_LeaveOneDefault(t *testing.T) {
	r := newPolicyRace(t, "Default policy create race")
	seedAccount(t, r.db, dunning.NewService(r.store, nil, nil), r.tenantID)
	seeded := r.requireOneDefault(t)
	if !seeded.Seeded {
		t.Fatal("the default is not the seeded policy; the test needs it to be")
	}
	// Created disabled, so it does not take the default from the seed.
	other := r.create(t, "Other", false)

	hold := r.holdPolicy(t, seeded.ID)
	create := r.start(t, 1, func() error {
		_, err := r.store.UpsertPolicy(r.ctx, r.tenantID, racePolicy("New", true))
		return err
	})
	toOther := r.start(t, 2, func() error { return r.store.SetDefaultPolicy(r.ctx, r.tenantID, other.ID) })
	if err := hold.Commit(); err != nil {
		t.Fatalf("release held policy: %v", err)
	}
	waitNoError(t, "create the first enabled policy", create)
	waitNoError(t, "SetDefaultPolicy to Other", toOther)

	if got := r.requireOneDefault(t); got.ID != other.ID {
		t.Errorf("default is %q, want Other, the policy set after the create", got.Name)
	}
}
