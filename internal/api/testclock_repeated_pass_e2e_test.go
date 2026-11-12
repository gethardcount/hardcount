package api

import (
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testclock/testclocktest"
)

// TestE2E_TestClock_RepeatedPassBillsOnce: the testclock_catchup leader runs
// a pass again when the tick that ran it ended (a shutdown, a lost lease),
// and the pass may have written some or all of its work by then. Running it
// again must bill nothing twice. Here the whole pass ran, then runs again at
// the same time, with real phases: the period invoice and its usage stay one
// each.
func TestE2E_TestClock_RepeatedPassBillsOnce(t *testing.T) {
	r := newPPRig(t, "repeated pass")
	calls := r.meter("calls", "sum")
	plan, ids := r.plan("pro", perUnit(calls, "1", nil))
	frozen := time.Now().UTC().AddDate(0, 0, -60).Truncate(time.Second)
	clk := r.clock("repeat", frozen)
	sub, p0, p1 := r.clockSub("pp-repeat", clk, plan)
	r.ingest(time.Time{}, "pp-repeat", "calls", 10, midpoint(p0, frozen), nil)

	r.advance(clk, p1.Add(time.Hour))
	invoices := func() int {
		return flowCount(t, r.db, `SELECT count(*) FROM invoices WHERE subscription_id = $1`, sub)
	}
	before := invoices()
	if before == 0 {
		t.Fatal("the advance billed no invoice; the test proves nothing")
	}
	wantUsage(t, "after the first pass", r.cycle(sub, p0), map[string]int64{ids[0]: 10})

	// The clock goes back to advancing at the same time, as when its tick
	// ended before the leader could complete it, and the next tick runs the
	// pass again.
	tx, err := r.db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := tx.ExecContext(t.Context(), `UPDATE test_clocks SET status = 'advancing' WHERE id = $1`, clk); err != nil {
		t.Fatalf("mark advancing again: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	testclocktest.Drain(t, r.srv.TestClockSvc)

	if after := invoices(); after != before {
		t.Errorf("invoices after the repeated pass = %d, want %d: the pass billed again", after, before)
	}
	wantUsage(t, "after the repeated pass", r.cycle(sub, p0), map[string]int64{ids[0]: 10})
	if st := flowStr(r.must(time.Time{}, "GET", "/v1/test-clocks/"+clk, nil, 200), "status"); st != "ready" {
		t.Errorf("clock status after the repeated pass = %q, want ready", st)
	}
}
