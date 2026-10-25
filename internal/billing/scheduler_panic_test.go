package billing

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/sagarsuperuser/velox/internal/testutil"
)

// A panic on one unusual row (the real trigger: a nil dereference, as in
// flexprice #1615 and the nil Stripe client #1299 had to guard) used to end the
// whole tick. Every later step, the test-mode pass and the cleanup never ran,
// and the next tick panicked on the same row again, so no tenant was billed
// or charged until a hotfix. Now each step runs, and the first panic is raised
// again once the tick has run, so the tick still ends as panicked and pages.

func modeName(ctx context.Context) string {
	if testutil.MustLivemode(ctx) {
		return "live"
	}
	return "test"
}

// panickingPayments is a payment reconciler that panics on every run, naming
// the mode it panicked in.
type panickingPayments struct{}

func (panickingPayments) Run(ctx context.Context, _ int) (int, []error) {
	panic("payment reconciler: nil invoice (" + modeName(ctx) + ")")
}

// TestScheduler_ReconcilerPanic_LaterStepsStillRun: the first recovery sweep
// panics in both modes. The sweeps after it still run in both modes, and the
// panic raised at the end is the first one (live).
func TestScheduler_ReconcilerPanic_LaterStepsStillRun(t *testing.T) {
	order := []string{}
	s := &Scheduler{
		engine:            tenantRunEngine(subsMock()),
		batch:             50,
		paymentReconciler: panickingPayments{},
		taxRetrier:        &fakeTaxRetrier{order: &order},
		clawbackRetrier:   &fakeClawbackRetrier{order: &order},
	}

	recovered := func() (r any) {
		defer func() { r = recover() }()
		s.runBillingHalf(context.Background())
		return nil
	}()

	if recovered != "payment reconciler: nil invoice (live)" {
		t.Fatalf("recovered %v, want the first panic (live) raised again at the end of the tick", recovered)
	}
	pass := []string{"tax_retry", "tax_commit", "tax_reversal", "clawback_issue", "cn_tax_reversal"}
	if want := append(append([]string{}, pass...), pass...); !reflect.DeepEqual(order, want) {
		t.Fatalf("sweeps run after the panic: %v, want every later sweep in both modes %v", order, want)
	}
}

// panickyDunning panics for one tenant and records every call.
type panickyDunning struct {
	mu       sync.Mutex
	panicFor string
	calls    []string
}

func (d *panickyDunning) ProcessDueRuns(ctx context.Context, tenantID string, _ int) (int, []error) {
	d.mu.Lock()
	d.calls = append(d.calls, tenantID+"/"+modeName(ctx))
	d.mu.Unlock()
	if tenantID == d.panicFor {
		panic("dunning: nil policy for " + tenantID + " (" + modeName(ctx) + ")")
	}
	return 0, nil
}

// TestScheduler_DunningPanic_OtherTenantsStillRun: one tenant's dunning panics.
// The other tenant is still processed in both modes, and the first panic is
// raised again at the end.
func TestScheduler_DunningPanic_OtherTenantsStillRun(t *testing.T) {
	d := &panickyDunning{panicFor: "t_a"}
	s := &Scheduler{engine: &Engine{}, dunning: d, tenants: &fixedTenants{ids: []string{"t_a", "t_b"}}, batch: 1}

	recovered := func() (r any) {
		defer func() { r = recover() }()
		s.runDunningHalf(context.Background())
		return nil
	}()

	if recovered != "dunning: nil policy for t_a (live)" {
		t.Fatalf("recovered %v, want the first panic (t_a, live) raised again at the end of the tick", recovered)
	}
	want := []string{"t_a/live", "t_b/live", "t_a/test", "t_b/test"}
	if !reflect.DeepEqual(d.calls, want) {
		t.Fatalf("dunning calls %v, want %v: the other tenant must still run in both modes", d.calls, want)
	}
}
