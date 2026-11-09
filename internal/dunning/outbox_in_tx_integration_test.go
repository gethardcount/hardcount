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
	"github.com/gethardcount/hardcount/internal/email"
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
// (ADR-131). When dunning writes the invoice off itself, the escalation is
// saved in the write-off's transaction, so the write-off is saved with the
// escalation or not at all too (ADR-112 amendment 2026-11-09).
//
// Everything is real Postgres: the dunning store, the invoice store and
// service that write the invoice off, and the webhook and email outboxes.

// stopAtTimelineRow is the real dunning store, except that it ends the
// caller's context when dunning writes a timeline row of type at. Dunning
// writes the resolved row right after the resolve commits, so this stops the
// work at the first step after that commit.
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

// cancelAndWriteOffPolicy cancels the subscription and writes the invoice off
// when retries run out.
var cancelAndWriteOffPolicy = finalActions{name: "cancel and write off", sub: domain.SubActionCancel, inv: domain.InvActionMarkUncollectible}

// eventFixture is an exhaustFixture out of retries (exhaust-on-entry on its
// next tick) under acts, with its webhook events and the customer's dunning
// emails wired as router.go wires them.
type eventFixture struct {
	*exhaustFixture
	stopper *stopAtTimelineRow
	outbox  *faultyOutbox
}

func newEventFixture(t *testing.T, db *postgres.DB, name, failType string, acts finalActions) *eventFixture {
	t.Helper()
	stopper := &stopAtTimelineRow{}
	f := newExhaustFixture(t, db, name, acts, 2, func(s dunning.Store) dunning.Store {
		stopper.Store = s
		return stopper
	}, noRetry{t})
	outbox := &faultyOutbox{outbox: webhook.NewOutboxStore(db), failType: failType}
	f.runs.SetOutboxEnqueuer(outbox)
	f.svc.SetEmailNotifier(email.NewOutboxSender(email.NewOutboxStore(db)))
	f.svc.SetCustomerEmailFetcher(fixedEmailFetcher{email: "billing@example.test", name: name})
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

// escalatedRows returns the run's escalated timeline rows.
func (f *eventFixture) escalatedRows(t *testing.T) []domain.InvoiceDunningEvent {
	t.Helper()
	timeline, err := f.runs.ListEvents(f.ctx, f.tenantID, f.runID)
	if err != nil {
		t.Fatalf("list run events: %v", err)
	}
	var out []domain.InvoiceDunningEvent
	for _, e := range timeline {
		if e.EventType == domain.DunningEventEscalated {
			out = append(out, e)
		}
	}
	return out
}

// escalationEmails counts the tenant's queued escalation emails.
func (f *eventFixture) escalationEmails(t *testing.T) int {
	t.Helper()
	return countEmailRows(t, f.db, f.ctx, f.tenantID, email.TypeDunningEscalation)
}

// outboxRows counts the tenant's queued eventType rows, whatever they are
// about, read under its row-level security.
func (f *eventFixture) outboxRows(t *testing.T, eventType string) int {
	t.Helper()
	tx, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(f.ctx, `SELECT count(*) FROM webhook_outbox WHERE event_type = $1`, eventType).Scan(&n); err != nil {
		t.Fatalf("count %s rows: %v", eventType, err)
	}
	return n
}

// appliedOutcome is what exhaustion does to the fixture's invoice under acts.
// Every action applies, because the fixture's subscription is active.
func appliedOutcome(acts finalActions) domain.DunningEscalationOutcome {
	return domain.DunningEscalationOutcome{
		SubscriptionPaused:   acts.sub == domain.SubActionPause,
		SubscriptionCanceled: acts.sub == domain.SubActionCancel,
		InvoiceWrittenOff:    acts.inv == domain.InvActionMarkUncollectible,
	}
}

// assertEscalatedOnce checks the run's one escalation under a policy that
// writes the invoice off: the invoice written off, the run escalated, one
// dunning.escalated carrying the run as GET returns it, no dunning.resolved,
// one escalation email, and one escalated timeline row naming what applied.
func (f *eventFixture) assertEscalatedOnce(t *testing.T, acts finalActions) {
	t.Helper()
	if got := f.invoiceStatus(t); got != domain.InvoiceUncollectible {
		t.Errorf("invoice is %s, want uncollectible: the policy writes it off", got)
	}
	run := f.run(t)
	if run.State != domain.DunningEscalated || run.Resolution != domain.ResolutionRetriesExhausted || run.NextActionAt != nil {
		t.Errorf("run state=%s resolution=%q next_action_at=%v, want escalated / retries_exhausted with none",
			run.State, run.Resolution, run.NextActionAt)
	}
	f.checkOneEvent(t, domain.EventDunningEscalated, map[string]any{
		"final_subscription_action": string(acts.sub),
		"final_invoice_action":      string(acts.inv),
		"applied":                   appliedOutcome(acts).Reason(),
	})
	if n := len(f.events(t, domain.EventDunningResolved)); n != 0 {
		t.Errorf("%d dunning.resolved rows, want 0: the run escalated", n)
	}
	if n := f.escalationEmails(t); n != 1 {
		t.Errorf("%d escalation emails queued, want 1: the customer is told the retries ran out", n)
	}
	rows := f.escalatedRows(t)
	if len(rows) != 1 {
		t.Errorf("%d escalated timeline rows, want 1", len(rows))
		return
	}
	if want := appliedOutcome(acts).Reason(); rows[0].Reason != want {
		t.Errorf("escalated timeline row reads %q, want %q: it names what applied", rows[0].Reason, want)
	}
}

// stopAfterWriteOff is the real write-off, except that it ends the tick's
// context as soon as the write-off returns: the first point after its commit,
// where a deploy, a crash or a lost leader lease stops the tick.
type stopAfterWriteOff struct {
	writeOff
	stop context.CancelFunc
}

func (s *stopAfterWriteOff) MarkUncollectible(ctx context.Context, tenantID, id string, then func(*sql.Tx) error) error {
	err := s.writeOff.MarkUncollectible(ctx, tenantID, id, then)
	if s.stop != nil {
		s.stop()
		s.stop = nil
	}
	return err
}

// TestExhaust_WriteOff_StoppedAfterWriteOff_EscalatesWithEmail: retries run
// out under a policy that writes the invoice off, and the tick stops right
// after the write-off returns. The escalation was saved with the write-off, so
// the run is escalated with its event, its email and its timeline row, and the
// tick after the exhaustion lease changes nothing. With the cancel too, the
// subscription is canceled once.
//
// Mutations that turn it red: escalating after the write-off commits, in its
// own transaction (the stop lands between the two, and the next tick reads
// the write-off as someone else's and resolves the run); writing the
// escalated timeline row after the commit.
func TestExhaust_WriteOff_StoppedAfterWriteOff_EscalatesWithEmail(t *testing.T) {
	db := testutil.SetupTestDB(t)
	for _, acts := range []finalActions{writeOffPolicy, cancelAndWriteOffPolicy} {
		t.Run(acts.name, func(t *testing.T) {
			f := newEventFixture(t, db, "Stopped After Write-off "+acts.name, "", acts)
			f.subStore.SetOutboxEnqueuer(webhook.NewOutboxStore(db))
			ctx, stop := context.WithCancel(f.ctx)
			defer stop()
			f.svc.SetInvoiceUncollectibleMarker(&stopAfterWriteOff{writeOff: writeOff{svc: f.invSvc}, stop: stop})

			if errs := f.tickAt(ctx); len(errs) > 0 {
				t.Logf("tick errors after the stop (not under test): %v", errs)
			}
			// The exhaustion lease runs out, and the next tick looks again.
			f.clk.Advance(16 * time.Minute)
			if errs := f.tickAt(f.ctx); len(errs) > 0 {
				t.Fatalf("tick after the lease: %v", errs)
			}

			f.assertEscalatedOnce(t, acts)
			want := 0
			if acts.sub == domain.SubActionCancel {
				want = 1
			}
			if n := f.outboxRows(t, domain.EventSubscriptionCanceled); n != want {
				t.Errorf("%d subscription.canceled rows, want %d", n, want)
			}
		})
	}
}

// TestDunningEvents_ContextEndsAfterCommit_EventIsQueued: each kind of resolve
// stops right after the run write commits. The event was saved with the
// write: once, with the run GET returns. The escalation's stop is in
// TestExhaust_WriteOff_StoppedAfterWriteOff_EscalatesWithEmail.
func TestDunningEvents_ContextEndsAfterCommit_EventIsQueued(t *testing.T) {
	db := testutil.SetupTestDB(t)

	t.Run("resolved by the operator's write-off", func(t *testing.T) {
		f := newEventFixture(t, db, "Outbox Dunning Write-off", "", writeOffPolicy)
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
		f := newEventFixture(t, db, "Outbox Dunning Paid", "", writeOffPolicy)
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
	f := newEventFixture(t, db, "Outbox Dunning Two Resolvers", "", writeOffPolicy)

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
	f := newEventFixture(t, db, "Outbox Dunning Resolve Fails", domain.EventDunningResolved, writeOffPolicy)

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

// TestDunningEscalated_EventInsertFails_WriteOffRollsBack: the policy writes
// the invoice off, and the insert of dunning.escalated fails. The escalation
// fails with its event, and the write-off it rides rolls back with it: the
// invoice stays open, and the run stays active with its exhaustion lease, not
// marked action_failed. The next tick after the lease writes the invoice off
// and escalates, with one event, one email and one timeline row.
//
// Mutations that turn it red: escalating after the write-off commits (the
// invoice is written off after the first tick, and the second tick resolves
// the run); treating the escalation's failure as a failed write-off (the run
// is marked action_failed and waits a day).
func TestDunningEscalated_EventInsertFails_WriteOffRollsBack(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f := newEventFixture(t, db, "Outbox Dunning Escalation Fails", domain.EventDunningEscalated, writeOffPolicy)

	errs := f.tickAt(f.ctx)
	if !f.outbox.failed.Load() {
		t.Fatal("the dunning.escalated insert was never attempted: the first tick did not reach the escalation")
	}
	if len(errs) != 1 || !errors.Is(errs[0], errOutboxDown) {
		t.Errorf("first tick errors %v, want the outbox error: a failed escalation is reported like any failed run write", errs)
	}
	if got := f.invoiceStatus(t); got != domain.InvoiceFinalized {
		t.Errorf("invoice is %s after the first tick, want finalized: the write-off rolls back with the escalation", got)
	}
	run := f.run(t)
	if run.State != domain.DunningActive || run.Resolution == domain.ResolutionActionFailed ||
		run.NextActionAt == nil || !run.NextActionAt.After(f.clk.Now(f.ctx)) {
		t.Errorf("run state=%s resolution=%q next_action_at=%v, want active, not action_failed, with its exhaustion lease after now (%v)",
			run.State, run.Resolution, run.NextActionAt, f.clk.Now(f.ctx))
	}
	if n := len(f.events(t, domain.EventDunningEscalated)); n != 0 {
		t.Errorf("%d dunning.escalated rows after the failed insert, want 0", n)
	}
	if n := f.escalationEmails(t); n != 0 {
		t.Errorf("%d escalation emails after the failed escalation, want 0", n)
	}
	if n := len(f.escalatedRows(t)); n != 0 {
		t.Errorf("%d escalated timeline rows after the failed escalation, want 0", n)
	}

	// The exhaustion lease runs out, and the next tick looks again.
	f.clk.Advance(16 * time.Minute)
	if errs := f.tickAt(f.ctx); len(errs) > 0 {
		t.Fatalf("second tick: %v", errs)
	}
	f.assertEscalatedOnce(t, writeOffPolicy)
}
