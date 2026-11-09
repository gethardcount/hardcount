package dunning_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// Every dunning.* event is queued in the transaction of the run write it
// reports (ADR-040), so the run's change and its event commit together. Each
// test here stops the work right after that commit, the way a deploy, a crash
// or a lost leader lease does, or fails the event's insert, and checks that
// the change and its event were saved together or not at all. The event
// carries the run as GET /v1/dunning/runs/{id} returns it after the commit
// (ADR-131).
//
// Everything is real Postgres: the dunning store, the invoice store and
// service that write the invoice off, and the webhook outbox.

// stopAtTimelineRow is the real dunning store, except that it ends the
// caller's context when dunning writes a timeline row of type at. Dunning
// writes the escalated and resolved rows right after the run write commits,
// so this stops the work at the first step after that commit.
type stopAtTimelineRow struct {
	dunning.Store
	mu   sync.Mutex
	at   domain.DunningEventType
	stop context.CancelFunc
}

func (s *stopAtTimelineRow) set(at domain.DunningEventType, stop context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.at, s.stop = at, stop
}

func (s *stopAtTimelineRow) CreateEvent(ctx context.Context, tenantID string, e domain.InvoiceDunningEvent) (domain.InvoiceDunningEvent, error) {
	s.mu.Lock()
	if s.stop != nil && e.EventType == s.at {
		s.stop()
	}
	s.mu.Unlock()
	return s.Store.CreateEvent(ctx, tenantID, e)
}

var errOutboxDown = errors.New("webhook outbox insert failed: connection reset")

// faultyOutbox is the real webhook outbox, except that the first insert of
// failType fails, as a dropped connection does at that moment. An empty
// failType never fails.
type faultyOutbox struct {
	outbox   *webhook.OutboxStore
	failType string
	failed   atomic.Bool
}

func (o *faultyOutbox) fails(eventType string) bool {
	return o.failType != "" && eventType == o.failType && o.failed.CompareAndSwap(false, true)
}

func (o *faultyOutbox) Enqueue(ctx context.Context, tx *sql.Tx, tenantID, eventType string, payload map[string]any) (string, error) {
	if o.fails(eventType) {
		return "", errOutboxDown
	}
	return o.outbox.Enqueue(ctx, tx, tenantID, eventType, payload)
}

// writeOffPolicy writes the invoice off when retries run out, and leaves the
// subscription alone.
var writeOffPolicy = finalActions{name: "write off", sub: domain.SubActionNone, inv: domain.InvActionMarkUncollectible}

// eventFixture is an exhaustFixture out of retries (exhaust-on-entry on its
// next tick), with its webhook events wired as router.go wires them.
type eventFixture struct {
	*exhaustFixture
	stopper *stopAtTimelineRow
	outbox  *faultyOutbox
}

func newEventFixture(t *testing.T, db *postgres.DB, name, failType string) *eventFixture {
	t.Helper()
	stopper := &stopAtTimelineRow{}
	f := newExhaustFixture(t, db, name, writeOffPolicy, 2, func(s dunning.Store) dunning.Store {
		stopper.Store = s
		return stopper
	}, noRetry{t})
	outbox := &faultyOutbox{outbox: webhook.NewOutboxStore(db), failType: failType}
	f.runs.SetOutboxEnqueuer(outbox)
	return &eventFixture{exhaustFixture: f, stopper: stopper, outbox: outbox}
}

// stopped returns a context that ends when dunning writes a timeline row of
// type at, and its cancel.
func (f *eventFixture) stopped(at domain.DunningEventType) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(f.ctx)
	f.stopper.set(at, cancel)
	return ctx, func() {
		f.stopper.set("", nil)
		cancel()
	}
}

// events returns the data of the tenant's eventType outbox rows about the
// fixture's run, read under its row-level security.
func (f *eventFixture) events(t *testing.T, eventType string) []map[string]any {
	t.Helper()
	tx, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(f.ctx, `SELECT payload FROM webhook_outbox WHERE event_type = $1`, eventType)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatalf("decode outbox payload: %v", err)
		}
		run, _ := data[domain.WebhookResourceDunningRun].(map[string]any)
		if run["id"] == f.runID {
			out = append(out, data)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	return out
}

// checkOneEvent checks that exactly one eventType row is queued for the run,
// and that it carries the run as GET returns it now, with extra beside it.
func (f *eventFixture) checkOneEvent(t *testing.T, eventType string, extra map[string]any) {
	t.Helper()
	got := f.events(t, eventType)
	switch {
	case len(got) == 0:
		t.Errorf("0 %s rows, want 1: the run write committed, so its event must be saved with it", eventType)
		return
	case len(got) > 1:
		t.Errorf("%d %s rows, want 1: one run write sends one event", len(got), eventType)
		return
	}
	if err := domain.CheckWebhookData(eventType, got[0]); err != nil {
		t.Error(err)
	}
	run, err := f.runs.GetRun(f.ctx, f.tenantID, f.runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	enc, err := json.Marshal(domain.DunningRunWebhookData(run, extra))
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(enc, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("%s data differs from the run the write committed:\n got %v\nwant %v", eventType, got[0], want)
	}
}

// run reads the fixture's run.
func (f *eventFixture) run(t *testing.T) domain.InvoiceDunningRun {
	t.Helper()
	run, err := f.runs.GetRun(f.ctx, f.tenantID, f.runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	return run
}

// invoiceStatus reads the fixture's invoice status.
func (f *eventFixture) invoiceStatus(t *testing.T) domain.InvoiceStatus {
	t.Helper()
	inv, err := f.invoices.Get(f.ctx, f.tenantID, f.invoiceID)
	if err != nil {
		t.Fatalf("get invoice: %v", err)
	}
	return inv.Status
}

// tickAt runs one dunning tick with ctx and returns its errors.
func (f *eventFixture) tickAt(ctx context.Context) []error {
	_, errs := f.svc.ProcessDueRuns(ctx, f.tenantID, 50)
	return errs
}

// TestDunningEvents_ContextEndsAfterCommit_EventIsQueued: the escalation and
// each kind of resolve stop right after the run write commits. The event was
// saved with the write: once, with the run GET returns.
func TestDunningEvents_ContextEndsAfterCommit_EventIsQueued(t *testing.T) {
	db := testutil.SetupTestDB(t)

	t.Run("escalated", func(t *testing.T) {
		f := newEventFixture(t, db, "Outbox Dunning Escalated", "")
		ctx, stop := f.stopped(domain.DunningEventEscalated)
		errs := f.tickAt(ctx)
		stop()
		if len(errs) > 0 {
			t.Logf("tick errors after the stop (not under test): %v", errs)
		}
		if run := f.run(t); run.State != domain.DunningEscalated || run.Resolution != domain.ResolutionRetriesExhausted {
			t.Fatalf("run state=%s resolution=%q, want escalated / retries_exhausted", run.State, run.Resolution)
		}
		if got := f.invoiceStatus(t); got != domain.InvoiceUncollectible {
			t.Fatalf("invoice is %s, want uncollectible: the policy writes it off", got)
		}
		f.checkOneEvent(t, domain.EventDunningEscalated, map[string]any{
			"final_subscription_action": string(domain.SubActionNone),
			"final_invoice_action":      string(domain.InvActionMarkUncollectible),
			"applied":                   domain.DunningEscalationOutcome{InvoiceWrittenOff: true}.Reason(),
		})
		if n := len(f.events(t, domain.EventDunningResolved)); n != 0 {
			t.Errorf("%d dunning.resolved rows, want 0: the run escalated", n)
		}
	})

	t.Run("resolved by the operator's write-off", func(t *testing.T) {
		f := newEventFixture(t, db, "Outbox Dunning Write-off", "")
		ctx, stop := f.stopped(domain.DunningEventResolved)
		_, err := f.svc.ResolveRun(ctx, f.tenantID, f.runID, domain.ResolutionInvoiceNotCollectible)
		stop()
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if run := f.run(t); run.State != domain.DunningResolved || run.Resolution != domain.ResolutionInvoiceNotCollectible {
			t.Fatalf("run state=%s resolution=%q, want resolved / invoice_not_collectible", run.State, run.Resolution)
		}
		if got := f.invoiceStatus(t); got != domain.InvoiceUncollectible {
			t.Fatalf("invoice is %s, want uncollectible", got)
		}
		f.checkOneEvent(t, domain.EventDunningResolved, nil)
	})

	t.Run("resolved by a payment", func(t *testing.T) {
		f := newEventFixture(t, db, "Outbox Dunning Paid", "")
		ctx, stop := f.stopped(domain.DunningEventResolved)
		err := f.svc.ResolveByInvoice(ctx, f.tenantID, f.invoiceID, domain.ResolutionPaymentRecovered)
		stop()
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if run := f.run(t); run.State != domain.DunningResolved || run.Resolution != domain.ResolutionPaymentRecovered {
			t.Fatalf("run state=%s resolution=%q, want resolved / payment_recovered", run.State, run.Resolution)
		}
		f.checkOneEvent(t, domain.EventDunningResolved, nil)
	})
}

// TestDunningResolved_TwoResolversAtOnce_OneEvent: a payment's resolve and an
// operator's resolve of one run collide. A held row lock makes both read the
// active run before either writes. The resolve's compare-and-set lets one
// win, and only the winner's transaction queues dunning.resolved.
func TestDunningResolved_TwoResolversAtOnce_OneEvent(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f := newEventFixture(t, db, "Outbox Dunning Two Resolvers", "")

	tx, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			postgres.Rollback(tx)
		}
	}()
	if _, err := tx.ExecContext(f.ctx, `SELECT id FROM invoice_dunning_runs WHERE id = $1 FOR UPDATE`, f.runID); err != nil {
		t.Fatalf("lock run: %v", err)
	}

	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = f.svc.ResolveByInvoice(f.ctx, f.tenantID, f.invoiceID, domain.ResolutionPaymentRecovered)
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = f.svc.ResolveRun(f.ctx, f.tenantID, f.runID, domain.ResolutionPaymentRecovered)
	}()
	testutil.WaitForLockWaiters(t, f.ctx, testutil.AdminPool(t), "invoice_dunning_runs", 2)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	committed = true
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("resolver %d: %v", i, err)
		}
	}

	if run := f.run(t); run.State != domain.DunningResolved {
		t.Fatalf("run state=%s, want resolved", run.State)
	}
	f.checkOneEvent(t, domain.EventDunningResolved, nil)
	timeline, err := f.runs.ListEvents(f.ctx, f.tenantID, f.runID)
	if err != nil {
		t.Fatalf("list run events: %v", err)
	}
	resolvedRows := 0
	for _, e := range timeline {
		if e.EventType == domain.DunningEventResolved {
			resolvedRows++
		}
	}
	if resolvedRows != 1 {
		t.Errorf("%d resolved timeline rows, want 1: only the compare-and-set's winner writes one", resolvedRows)
	}
}

// TestDunningResolved_EventInsertFails_ResolveRollsBack: the insert of
// dunning.resolved fails. The resolve fails with it and the run stays active,
// so the caller sees the error and the resolve can run again. The second
// resolve commits the run and its one event.
func TestDunningResolved_EventInsertFails_ResolveRollsBack(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f := newEventFixture(t, db, "Outbox Dunning Resolve Fails", domain.EventDunningResolved)

	err := f.svc.ResolveByInvoice(f.ctx, f.tenantID, f.invoiceID, domain.ResolutionPaymentRecovered)
	if !errors.Is(err, errOutboxDown) {
		t.Errorf("resolve: err=%v, want the outbox error: the run and its event commit together or not at all", err)
	}
	if run := f.run(t); run.State != domain.DunningActive {
		t.Errorf("run state=%s after the failed resolve, want active: its event was not saved, so neither is the resolve", run.State)
	}
	if n := len(f.events(t, domain.EventDunningResolved)); n != 0 {
		t.Errorf("%d dunning.resolved rows after the failed insert, want 0", n)
	}

	if err := f.svc.ResolveByInvoice(f.ctx, f.tenantID, f.invoiceID, domain.ResolutionPaymentRecovered); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if run := f.run(t); run.State != domain.DunningResolved {
		t.Errorf("run state=%s after the second resolve, want resolved", run.State)
	}
	f.checkOneEvent(t, domain.EventDunningResolved, nil)
}

// TestDunningEscalated_EventInsertFailsAfterWriteOff_OneTerminalEvent: the
// policy writes the invoice off, and then the insert of dunning.escalated
// fails. The write-off has committed, but the escalation has not: it fails
// with its event. The next tick after the exhaustion lease finds the invoice
// uncollectible and resolves the run as not collectible. The run ends with
// exactly one terminal event, never none and never both.
//
// The escalation email is not sent on this path. That gap is older than this
// test: any failure of the escalation write after the write-off ends the
// same way (ADR-112, "Deferred, with triggers").
func TestDunningEscalated_EventInsertFailsAfterWriteOff_OneTerminalEvent(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f := newEventFixture(t, db, "Outbox Dunning Escalation Fails", domain.EventDunningEscalated)

	if errs := f.tickAt(f.ctx); len(errs) > 0 {
		t.Logf("first tick: %v", errs)
	}
	if !f.outbox.failed.Load() {
		t.Fatal("the dunning.escalated insert was never attempted: the first tick did not reach the escalation")
	}
	if got := f.invoiceStatus(t); got != domain.InvoiceUncollectible {
		t.Fatalf("invoice is %s after the first tick, want uncollectible: the write-off commits before the escalation", got)
	}

	// The exhaustion lease runs out, and the next tick looks again.
	f.clk.Advance(16 * time.Minute)
	if errs := f.tickAt(f.ctx); len(errs) > 0 {
		t.Fatalf("second tick: %v", errs)
	}

	escalated := f.events(t, domain.EventDunningEscalated)
	resolved := f.events(t, domain.EventDunningResolved)
	if len(escalated)+len(resolved) != 1 {
		t.Fatalf("%d dunning.escalated and %d dunning.resolved rows, want exactly one terminal event between them",
			len(escalated), len(resolved))
	}
	run := f.run(t)
	switch {
	case len(resolved) == 1:
		if run.State != domain.DunningResolved || run.Resolution != domain.ResolutionInvoiceNotCollectible {
			t.Errorf("run state=%s resolution=%q beside dunning.resolved, want resolved / invoice_not_collectible", run.State, run.Resolution)
		}
		f.checkOneEvent(t, domain.EventDunningResolved, nil)
	default:
		if run.State != domain.DunningEscalated {
			t.Errorf("run state=%s beside dunning.escalated, want escalated", run.State)
		}
	}
}
