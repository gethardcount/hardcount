// Package testclocktest runs the test-clock catch-up in tests the way the
// testclock_catchup leader runs it in production (ADR-140).
package testclocktest

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testclock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// maxTicks bounds Drain. One tick runs one pass on every advancing clock, and
// a pass ends its clock in ready or internal_failure, so a second tick finds
// nothing; more means a pass keeps leaving its clock advancing.
const maxTicks = 5

// Drain runs the catch-up until no test clock is advancing, on a ctx that
// holds the testclock_catchup lease (leadertest.Token). Advance and Retry
// advance only mark a clock; a test calls Drain right after one to run the
// catch-up as the leader would. svc must be the service the advance went
// through, so the catch-up runs with the same phases wired. The claim list
// spans every tenant, as the leader's does.
func Drain(t *testing.T, svc *testclock.Service) {
	t.Helper()
	ctx := leadertest.Token(t, testutil.AdminPool(t), context.Background(), leader.RoleTestClockCatchup)
	for range maxTicks {
		n, err := svc.RunAdvancing(ctx)
		if err != nil {
			t.Fatalf("testclocktest.Drain: %v", err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatalf("testclocktest.Drain: test clocks still advancing after %d catch-up ticks", maxTicks)
}

// DrainReady is Drain for one clock that must end ready: it fails the test
// with the clock's failure reason when the catch-up failed it.
func DrainReady(t *testing.T, svc *testclock.Service, tenantID, clockID string) {
	t.Helper()
	Drain(t, svc)
	// Test clocks exist only in test mode.
	clk, err := svc.Get(postgres.WithLivemode(context.Background(), false), tenantID, clockID)
	if err != nil {
		t.Fatalf("testclocktest.DrainReady: read clock %s: %v", clockID, err)
	}
	if clk.Status != domain.TestClockStatusReady {
		t.Fatalf("testclocktest.DrainReady: clock %s is %s after the catch-up: %s", clockID, clk.Status, clk.LastFailureReason)
	}
}
