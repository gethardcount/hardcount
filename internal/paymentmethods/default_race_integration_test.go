package paymentmethods_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/paymentmethods"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The tests in this file race two changes to one customer's default card,
// and each must end with no error and exactly one default among the active
// cards. A test transaction holds one card row while both changes
// start, so both queue before either commits. That is the interleaving in
// which two unserialized changes miss each other's writes.
//
// Every writer of a customer's default takes the per-customer lock first
// (lockCustomerCards in store.go), so the second change starts only after
// the first has committed, and reads its result.
//
// Mutation check: drop the lockCustomerCards call from SetDefault, and the
// two SetDefault tests fail on the one-default unique index (a 500 at the
// API). Drop it from DetachAndRebalance, and the removal test leaves an
// active card with no default.

// cardRace is one customer with cards, and the connections a race needs.
type cardRace struct {
	ctx      context.Context
	db       *postgres.DB
	admin    *sql.DB
	store    *paymentmethods.PostgresStore
	tenantID string
	custID   string
}

func newCardRace(t *testing.T) *cardRace {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	t.Cleanup(cancel)
	tenantID := testutil.CreateTestTenant(t, db, "default card race")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_default_race", DisplayName: "Default Race", Email: "race@example.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	return &cardRace{
		ctx: ctx, db: db, admin: testutil.AdminPool(t),
		store: paymentmethods.NewPostgresStore(db), tenantID: tenantID, custID: cust.ID,
	}
}

// addCard saves a card the way the webhook does, without taking the
// default. The customer's first card becomes the default.
func (r *cardRace) addCard(t *testing.T, stripeID string) paymentmethods.PaymentMethod {
	t.Helper()
	pm, _, err := r.store.Upsert(r.ctx, r.tenantID, paymentmethods.PaymentMethod{
		CustomerID: r.custID, StripePaymentMethodID: stripeID, Type: "card",
		CardBrand: "visa", CardLast4: "4242", CardFingerprint: "fp_" + stripeID,
	}, false, "", nil)
	if err != nil {
		t.Fatalf("add card %s: %v", stripeID, err)
	}
	return pm
}

// holdCard opens a transaction that locks the card's row until commit. A
// change that writes that row waits for it.
func (r *cardRace) holdCard(t *testing.T, pmID string) *sql.Tx {
	t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin holding tx: %v", err)
	}
	t.Cleanup(func() { postgres.Rollback(tx) })
	if _, err := tx.ExecContext(r.ctx, `SELECT 1 FROM payment_methods WHERE id = $1 FOR UPDATE`, pmID); err != nil {
		t.Fatalf("lock card %s: %v", pmID, err)
	}
	return tx
}

// start runs change in a goroutine, then waits until `waiting` sessions
// wait on a lock. Without the fix a change waits on the held row; with it,
// the first change waits on the row and the second on the customer lock.
// Both match the empty pattern, which counts every waiting session in the
// test database.
func (r *cardRace) start(t *testing.T, waiting int, change func() error) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- change() }()
	testutil.WaitForLockWaiters(t, r.ctx, r.admin, "", waiting)
	return done
}

func wait(t *testing.T, name string, done <-chan error) {
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

// requireOneDefault fails unless exactly one active card is the default, and
// returns it.
func (r *cardRace) requireOneDefault(t *testing.T) paymentmethods.PaymentMethod {
	t.Helper()
	active, err := r.store.List(r.ctx, r.tenantID, r.custID)
	if err != nil {
		t.Fatalf("list cards: %v", err)
	}
	var defaults []paymentmethods.PaymentMethod
	for _, pm := range active {
		if pm.IsDefault {
			defaults = append(defaults, pm)
		}
	}
	if len(defaults) != 1 {
		t.Fatalf("%d active cards, %d of them default; want exactly one default", len(active), len(defaults))
	}
	return defaults[0]
}

// TestSetDefault_ConcurrentFlips_LeaveOneDefault: two Set as default calls
// for different cards, at once, both succeed, and the card set second is
// the one default. Without the lock, the second call's demote missed the
// first call's new default, and its promote failed on the unique index.
func TestSetDefault_ConcurrentFlips_LeaveOneDefault(t *testing.T) {
	r := newCardRace(t)
	first := r.addCard(t, "pm_first")
	x := r.addCard(t, "pm_x")
	y := r.addCard(t, "pm_y")

	hold := r.holdCard(t, first.ID)
	toX := r.start(t, 1, func() error {
		_, err := r.store.SetDefault(r.ctx, r.tenantID, r.custID, x.ID)
		return err
	})
	toY := r.start(t, 2, func() error {
		_, err := r.store.SetDefault(r.ctx, r.tenantID, r.custID, y.ID)
		return err
	})
	if err := hold.Commit(); err != nil {
		t.Fatalf("release held card: %v", err)
	}
	wait(t, "SetDefault to x", toX)
	wait(t, "SetDefault to y", toY)

	if got := r.requireOneDefault(t); got.ID != y.ID {
		t.Errorf("default is %s, want y (%s), the card set second", got.StripePaymentMethodID, y.ID)
	}
}

// TestSetDefault_RacingAnAttachThatTakesTheDefault: Set as default racing
// a setup-flow attach, which also takes the default, both succeed with one
// default. The attach already took the lock; Set as default did not.
func TestSetDefault_RacingAnAttachThatTakesTheDefault(t *testing.T) {
	r := newCardRace(t)
	first := r.addCard(t, "pm_first")
	x := r.addCard(t, "pm_x")

	hold := r.holdCard(t, first.ID)
	attach := r.start(t, 1, func() error {
		_, _, err := r.store.Upsert(r.ctx, r.tenantID, paymentmethods.PaymentMethod{
			CustomerID: r.custID, StripePaymentMethodID: "pm_setup", Type: "card",
			CardBrand: "visa", CardLast4: "1881", CardFingerprint: "fp_pm_setup",
		}, true, "", nil)
		return err
	})
	toX := r.start(t, 2, func() error {
		_, err := r.store.SetDefault(r.ctx, r.tenantID, r.custID, x.ID)
		return err
	})
	if err := hold.Commit(); err != nil {
		t.Fatalf("release held card: %v", err)
	}
	wait(t, "attach that takes the default", attach)
	wait(t, "SetDefault to x", toX)

	if got := r.requireOneDefault(t); got.ID != x.ID {
		t.Errorf("default is %s, want x (%s), the card set after the attach", got.StripePaymentMethodID, x.ID)
	}
}

// TestDetachAndRebalance_ConcurrentRemovals_LeaveADefault: removing the
// default card and the newest card at once leaves the remaining card as
// the default. Without the lock, the default's removal chose the newest
// card to promote, found it removed once the other removal committed, and
// committed with no default. Auto-charge then found no card, and dunning
// started with no_payment_method.
func TestDetachAndRebalance_ConcurrentRemovals_LeaveADefault(t *testing.T) {
	r := newCardRace(t)
	def := r.addCard(t, "pm_default")
	remaining := r.addCard(t, "pm_remaining")
	newest := r.addCard(t, "pm_newest")

	hold := r.holdCard(t, newest.ID)
	removeNewest := r.start(t, 1, func() error {
		_, _, err := r.store.DetachAndRebalance(r.ctx, r.tenantID, r.custID, newest.ID)
		return err
	})
	removeDefault := r.start(t, 2, func() error {
		_, _, err := r.store.DetachAndRebalance(r.ctx, r.tenantID, r.custID, def.ID)
		return err
	})
	if err := hold.Commit(); err != nil {
		t.Fatalf("release held card: %v", err)
	}
	wait(t, "remove the newest card", removeNewest)
	wait(t, "remove the default card", removeDefault)

	if got := r.requireOneDefault(t); got.ID != remaining.ID {
		t.Errorf("default is %s, want the one card left (%s)", got.StripePaymentMethodID, remaining.ID)
	}
}
