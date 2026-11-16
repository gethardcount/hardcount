package user

import (
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// The helpers below hold one transaction open inside its hook while a test
// starts a second, and see the second wait on a row lock. They are exported
// for the tests in package user_test too.

// testWait bounds every wait in these tests, so a broken lock or a failed
// request ends the test instead of hanging it.
const testWait = 10 * time.Second

// Gate holds a hook until Open is called. Open is also a cleanup, so a test
// that fails part-way never leaves a hook, and its transaction, blocked.
type Gate struct {
	reached   chan struct{}
	open      chan struct{}
	reachOnce sync.Once
	openOnce  sync.Once
}

func NewGate(t *testing.T) *Gate {
	g := &Gate{reached: make(chan struct{}), open: make(chan struct{})}
	t.Cleanup(g.Open)
	return g
}

// Hold says the hook was reached and waits for Open.
func (g *Gate) Hold() {
	g.reachOnce.Do(func() { close(g.reached) })
	<-g.open
}

// Open lets every Hold return.
func (g *Gate) Open() { g.openOnce.Do(func() { close(g.open) }) }

// WaitReached waits until a hook reached Hold.
func (g *Gate) WaitReached(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(testWait):
		t.Fatal("the hook was never reached")
	}
}

// WaitForLockWait waits until a backend of this test database is waiting on
// a lock while running a statement that matches like (a LIKE pattern).
func WaitForLockWait(t *testing.T, db *postgres.DB, like string) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for {
		var waiting bool
		// The poll's own statement contains the pattern too, but it is never
		// in a lock wait, so it does not match itself.
		if err := db.Pool.QueryRowContext(t.Context(), `
			SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			  AND query LIKE $1)`, like).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no statement like %q waited on a lock", like)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Receive returns the next value from ch, failing the test if none arrives
// in time.
func Receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(testWait):
		t.Fatalf("%s did not finish", what)
		var zero T
		return zero
	}
}
