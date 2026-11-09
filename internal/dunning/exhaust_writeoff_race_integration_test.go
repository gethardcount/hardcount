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

// When retries run out under a policy that writes the invoice off, the
// write-off and the escalation commit in one transaction (ADR-112 amendment
// 2026-11-09). These tests race that transaction against the other writers of
// the run: a resolver that closes the run first, and a second processor that
// reaches the run after this one's exhaustion lease ran out. All on real
// Postgres, with the real invoice service writing the invoice off.

// resolveBeforeWriteOff is the real write-off, except that a resolver commits
// first. Stripe refusing the amount as below its minimum resolves the run
// this way (payment.Stripe calls ResolveByInvoice), and it can land after
// dunning's late re-check read the invoice open.
type resolveBeforeWriteOff struct {
	writeOff
	svc *dunning.Service
	err error
}

func (m *resolveBeforeWriteOff) MarkUncollectible(ctx context.Context, tenantID, id string, then func(*sql.Tx) error) error {
	m.err = m.svc.ResolveByInvoice(ctx, tenantID, id, domain.ResolutionAmountBelowMinimum)
	return m.writeOff.MarkUncollectible(ctx, tenantID, id, then)
}

// resolveWhileWriteOffWaits is the real write-off, except that it runs while a
// test transaction holds the invoice's row lock, and the resolver commits
// while the write-off waits for that lock. The charge hold was released
// before the write-off, so the tick is already past the hold claim's own
// lock on the invoice.
type resolveWhileWriteOffWaits struct {
	writeOff
	t   *testing.T
	f   *eventFixture
	err error
}

func (m *resolveWhileWriteOffWaits) MarkUncollectible(ctx context.Context, tenantID, id string, then func(*sql.Tx) error) error {
	t := m.t
	admin := testutil.AdminPool(t)
	tx, err := m.f.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM invoices WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatalf("lock invoice: %v", err)
	}

	wrote := make(chan error, 1)
	go func() { wrote <- m.writeOff.MarkUncollectible(ctx, tenantID, id, then) }()
	// The write-off's row lock selects the invoice's full column list, and
	// pg_stat_activity keeps only the first 1 kB of a statement, which ends
	// before its FOR UPDATE. So match the list's start: the hold claim's lock
	// reads "SELECT 1 FROM invoices", and a plain read never waits on a lock.
	testutil.WaitForLockWaiters(t, ctx, admin, "SELECT id, tenant_id, customer_id, COALESCE(subscription_id,'')", 1)

	resolved := make(chan error, 1)
	go func() {
		resolved <- m.f.svc.ResolveByInvoice(ctx, tenantID, id, domain.ResolutionAmountBelowMinimum)
	}()
	// The resolve's run write has committed once its timeline row waits on
	// the held invoice for its foreign key.
	testutil.WaitForLockWaiters(t, ctx, admin, "INSERT INTO invoice_dunning_events", 1)

	postgres.Rollback(tx)
	m.err = <-resolved
	return <-wrote
}

// TestExhaust_ResolverCommitsBeforeWriteOff_WriteOffRollsBack: a resolver
// closes the run after dunning's late re-check and before the write-off. The
// escalation inside the write-off finds the run resolved, so the write-off
// rolls back: the invoice stays open beside the resolved run, with one
// dunning.resolved and no escalation, email or escalated row. In the lock
// variant the resolve commits while the write-off waits for the invoice's row
// lock. In the reverse order, the escalation commits first and a payment then
// resolves the escalated run: one event each.
//
// Mutations that turn it red: escalating after the write-off commits (the
// invoice is written off beside the resolved run); the escalation reporting
// success when its write did not apply; dropping state = 'active' from the
// escalation's write (it escalates the resolved run).
func TestExhaust_ResolverCommitsBeforeWriteOff_WriteOffRollsBack(t *testing.T) {
	db := testutil.SetupTestDB(t)

	assertResolvedNotWrittenOff := func(t *testing.T, f *eventFixture) {
		t.Helper()
		if got := f.invoiceStatus(t); got != domain.InvoiceFinalized {
			t.Errorf("invoice is %s, want finalized: a write-off must not land beside a run another path resolved", got)
		}
		run := f.run(t)
		if run.State != domain.DunningResolved || run.Resolution != domain.ResolutionAmountBelowMinimum {
			t.Errorf("run state=%s resolution=%q, want resolved / amount_below_minimum", run.State, run.Resolution)
		}
		f.checkOneEvent(t, domain.EventDunningResolved, nil)
		if n := len(f.events(t, domain.EventDunningEscalated)); n != 0 {
			t.Errorf("%d dunning.escalated rows, want 0: the run was resolved first", n)
		}
		if n := f.escalationEmails(t); n != 0 {
			t.Errorf("%d escalation emails, want 0", n)
		}
		if n := len(f.escalatedRows(t)); n != 0 {
			t.Errorf("%d escalated timeline rows, want 0", n)
		}
	}

	t.Run("the resolve commits before the write-off", func(t *testing.T) {
		f := newEventFixture(t, db, "Resolve Before Write-off", "", writeOffPolicy)
		m := &resolveBeforeWriteOff{writeOff: writeOff{svc: f.invSvc}, svc: f.svc}
		f.svc.SetInvoiceUncollectibleMarker(m)

		if errs := f.tickAt(f.ctx); len(errs) > 0 {
			t.Fatalf("tick: %v", errs)
		}
		if m.err != nil {
			t.Fatalf("resolve: %v", m.err)
		}
		assertResolvedNotWrittenOff(t, f)
	})

	t.Run("the resolve commits while the write-off waits for the invoice lock", func(t *testing.T) {
		f := newEventFixture(t, db, "Resolve While Write-off Waits", "", writeOffPolicy)
		m := &resolveWhileWriteOffWaits{writeOff: writeOff{svc: f.invSvc}, t: t, f: f}
		f.svc.SetInvoiceUncollectibleMarker(m)

		if errs := f.tickAt(f.ctx); len(errs) > 0 {
			t.Fatalf("tick: %v", errs)
		}
		if m.err != nil {
			t.Fatalf("resolve: %v", m.err)
		}
		assertResolvedNotWrittenOff(t, f)
	})

	t.Run("the escalation commits before a payment resolves the run", func(t *testing.T) {
		f := newEventFixture(t, db, "Escalate Then Paid", "", writeOffPolicy)
		if errs := f.tickAt(f.ctx); len(errs) > 0 {
			t.Fatalf("tick: %v", errs)
		}
		if run := f.run(t); run.State != domain.DunningEscalated {
			t.Fatalf("run state=%s after the tick, want escalated", run.State)
		}
		if err := f.svc.ResolveByInvoice(f.ctx, f.tenantID, f.invoiceID, domain.ResolutionPaymentRecovered); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if run := f.run(t); run.State != domain.DunningResolved || run.Resolution != domain.ResolutionPaymentRecovered {
			t.Errorf("run state=%s resolution=%q, want resolved / payment_recovered: a late payment closes an escalated run",
				run.State, run.Resolution)
		}
		if n := len(f.events(t, domain.EventDunningEscalated)); n != 1 {
			t.Errorf("%d dunning.escalated rows, want 1", n)
		}
		f.checkOneEvent(t, domain.EventDunningResolved, nil)
		if n := f.escalationEmails(t); n != 1 {
			t.Errorf("%d escalation emails, want 1", n)
		}
		if n := len(f.escalatedRows(t)); n != 1 {
			t.Errorf("%d escalated timeline rows, want 1", n)
		}
	})
}

// secondTickInside is the real write-off, except that before the first one
// runs, the clock passes this processor's exhaustion lease and a second
// processor's whole tick runs. The run is then due again, as it is when the
// first processor stalls past its lease (a long pause, a slow call). The
// nested tick calls this marker too, and its write-off runs at once.
type secondTickInside struct {
	writeOff
	f     *eventFixture
	calls int
	errs  []error
}

func (m *secondTickInside) MarkUncollectible(ctx context.Context, tenantID, id string, then func(*sql.Tx) error) error {
	m.calls++
	if m.calls == 1 {
		m.f.clk.Advance(16 * time.Minute)
		m.errs = m.f.tickAt(m.f.ctx)
	}
	return m.writeOff.MarkUncollectible(ctx, tenantID, id, then)
}

// TestExhaust_WriteOff_SecondProcessorAfterLease_OneEscalation: processor A
// stalls before its write-off until its exhaustion lease runs out, and
// processor B claims the run, writes the invoice off and escalates. A's
// write-off is then refused (the invoice is already written off), and A's
// action_failed write finds the run escalated and does not apply. The run has
// one escalation: one event, one email, one timeline row. A tick a day later
// changes nothing.
func TestExhaust_WriteOff_SecondProcessorAfterLease_OneEscalation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f := newEventFixture(t, db, "Second Processor After Lease", "", writeOffPolicy)
	m := &secondTickInside{writeOff: writeOff{svc: f.invSvc}, f: f}
	f.svc.SetInvoiceUncollectibleMarker(m)

	if errs := f.tickAt(f.ctx); len(errs) > 0 {
		t.Fatalf("processor A's tick: %v", errs)
	}
	if len(m.errs) > 0 {
		t.Fatalf("processor B's tick: %v", m.errs)
	}
	if m.calls != 2 {
		t.Fatalf("%d write-offs attempted, want 2: B's, then A's", m.calls)
	}
	f.assertEscalatedOnce(t, writeOffPolicy)

	f.clk.Advance(25 * time.Hour)
	if errs := f.tickAt(f.ctx); len(errs) > 0 {
		t.Fatalf("tick a day later: %v", errs)
	}
	if m.calls != 2 {
		t.Errorf("%d write-offs attempted after a day, want still 2: an escalated run is not due", m.calls)
	}
	f.assertEscalatedOnce(t, writeOffPolicy)
}
