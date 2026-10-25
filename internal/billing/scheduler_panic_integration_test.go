package billing_test

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// tickStep is a scheduler step double (trial expiry, pause resume, credit
// expiry, token cleanup) that records the mode of every call. panicLive makes
// it panic in the live pass.
type tickStep struct {
	mu        sync.Mutex
	modes     []string
	panicLive bool
}

func (s *tickStep) record(ctx context.Context) {
	mode := "test"
	if testutil.MustLivemode(ctx) {
		mode = "live"
	}
	s.mu.Lock()
	s.modes = append(s.modes, mode)
	s.mu.Unlock()
	if s.panicLive && mode == "live" {
		panic("trial expiry: nil plan on an unusual row")
	}
}

func (s *tickStep) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.modes...)
}

func (s *tickStep) ProcessExpiredTrials(ctx context.Context, _ int) (int, []error) {
	s.record(ctx)
	return 0, nil
}

func (s *tickStep) ProcessExpiredPauseCollections(ctx context.Context, _ int) (int, []error) {
	s.record(ctx)
	return 0, nil
}

func (s *tickStep) ExpireCredits(ctx context.Context) (int, []error) {
	s.record(ctx)
	return 0, nil
}

func (s *tickStep) Cleanup(ctx context.Context) (int, error) {
	s.record(ctx)
	return 0, nil
}

// TestScheduler_StepPanic_RestOfTheTickRuns (real Postgres): trial expiry
// panics in the live pass, the first mode the tick runs. The real trigger is
// a nil dereference on one unusual row (flexprice #1615). The panic used to
// end the tick there: the billing run, the test-mode pass and the cleanup
// never ran, and the next tick panicked on the same row. Now the due
// subscription (test mode) is billed in the same tick, every later step runs
// in both modes, the cleanup runs, and the panic is raised again at the end.
func TestScheduler_StepPanic_RestOfTheTickRuns(t *testing.T) {
	env := newBillFailureEnv(t, "Tick Step Panic")
	sub := env.dueSub(t, "sub-healthy", env.planOK, env.periodEnd)

	trials := &tickStep{panicLive: true}
	pauses, credits, tokens := &tickStep{}, &tickStep{}, &tickStep{}
	s := billing.NewScheduler(env.engine, time.Hour, 50, nil, nil, env.clock, credits)
	s.SetTrialExpirer(trials)
	s.SetPauseResumer(pauses)
	s.SetTokenCleaner(tokens)

	recovered := func() (r any) {
		defer func() { r = recover() }()
		billing.RunBillingHalf(s, env.ctx)
		return nil
	}()

	if recovered != "trial expiry: nil plan on an unusual row" {
		t.Fatalf("recovered %v, want the trial expiry panic raised again at the end of the tick", recovered)
	}
	if n := env.invoiceCount(t, sub.ID); n != 1 {
		t.Fatalf("the due subscription has %d invoices after the tick, want 1", n)
	}
	both := []string{"live", "test"}
	for name, step := range map[string]*tickStep{"trial expiry": trials, "pause resume": pauses, "credit expiry": credits} {
		if got := step.seen(); !reflect.DeepEqual(got, both) {
			t.Errorf("%s ran in %v, want %v", name, got, both)
		}
	}
	if got := tokens.seen(); len(got) != 1 {
		t.Errorf("token cleanup ran %d times, want once", len(got))
	}
}
