package dunning

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// When nothing is left to move, the clock's pass fails once for each due run
// that an earlier charge attempt of its invoice holds back, so the advance
// does not end ready with that retry not made. A run whose customer may be
// paying on the hosted page is not reported.
func TestProcessDueRunsForClock_ReportsEachRunAChargeAttemptHolds(t *testing.T) {
	store := newMemStore()
	store.held = []HeldRun{
		{RunID: "run_1", InvoiceID: "inv_1", InvoiceNumber: "INV-0001", HeldBy: domain.ChargeHeldByAttempt},
		{RunID: "run_2", InvoiceID: "inv_2", HeldBy: domain.ChargePayingOnHostedPage},
		{RunID: "run_3", InvoiceID: "inv_3", HeldBy: domain.ChargeHeldByAttempt},
	}
	svc := NewService(store, &noopRetrier{}, nil)
	_, errs := svc.ProcessDueRunsForClock(context.Background(), "t1", "clk", time.Now().UTC().Add(time.Hour), 20)
	if len(errs) != 2 {
		t.Fatalf("got %d errors, want one for each run an attempt holds: %v", len(errs), errs)
	}
	for i, ref := range []string{"INV-0001", "inv_3"} {
		var left *leftForNextPass
		if !errors.As(errs[i], &left) || !errors.Is(errs[i], domain.ErrLeftForNextRun) {
			t.Fatalf("error %d (%v) is not left for the next pass", i, errs[i])
		}
		want := "Invoice " + ref + ": a payment retry is waiting for an earlier charge attempt to finish: it cannot be charged twice."
		if !strings.HasPrefix(left.OperatorSafeMessage(), want) || !strings.Contains(left.OperatorSafeMessage(), "Retry advance once the attempt is done.") {
			t.Errorf("error %d says %q; want it to start %q", i, left.OperatorSafeMessage(), want)
		}
	}
}

// A held list that cannot be read fails the pass with that error: the
// advance does not end ready without having looked.
func TestProcessDueRunsForClock_AnUnreadableHeldListFailsThePass(t *testing.T) {
	store := newMemStore()
	store.heldErr = errors.New("connection reset by peer")
	svc := NewService(store, &noopRetrier{}, nil)
	_, errs := svc.ProcessDueRunsForClock(context.Background(), "t1", "clk", time.Now().UTC().Add(time.Hour), 20)
	if len(errs) != 1 || !errors.Is(errs[0], store.heldErr) || errors.Is(errs[0], domain.ErrLeftForNextRun) {
		t.Fatalf("errs %v; want the read's error, not left for the next pass", errs)
	}
}
