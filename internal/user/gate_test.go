package user

import (
	"sync"
	"testing"
	"time"
)

// The helpers below hold one transaction open inside its hook while a test
// starts a second; testutil.WaitForLockWaiter then sees the second wait on a
// row lock. They are exported for the tests in package user_test too.

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
