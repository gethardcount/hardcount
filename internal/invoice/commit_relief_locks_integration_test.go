package invoice_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The lock order of every note on a top-up against every writer that shares
// a lock with it (playbook item 8, test 5). Relief, the Stripe refund
// recorder and the dispute recorder write such notes; a drawdown, the expiry
// sweep, a top-up's payment and another relief share their locks. Each
// writer is held inside its locks while a note is written, and the note's
// writer is held at its last lock (the number row) while each runs; so are
// record-loss and the linked create, which write such notes too. Nothing
// may deadlock or time out, and a writer that shares no lock with relief
// must not wait for it.

// pause holds a writer at one step until the test lets it go.
type pause struct {
	reached, release chan struct{}
	once             sync.Once
}

func newPause() *pause { return &pause{reached: make(chan struct{}), release: make(chan struct{})} }

func (p *pause) hold() {
	p.once.Do(func() {
		close(p.reached)
		<-p.release
	})
}

// pausedInvoices holds the Stripe refund recorder after its first lock, the
// invoice row.
type pausedInvoices struct {
	creditnote.InvoiceReader
	p *pause
}

func (r pausedInvoices) LockForCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID, id string) (domain.Invoice, error) {
	inv, err := r.InvoiceReader.LockForCreditNoteTx(ctx, tx, tenantID, id)
	if err == nil {
		r.p.hold()
	}
	return inv, err
}

// pausedNumbers holds a relief after its last lock, the tenant's number row.
type pausedNumbers struct {
	creditnote.NumberGenerator
	p *pause
}

func (n pausedNumbers) NextCreditNoteNumberTx(ctx context.Context, tx *sql.Tx, tenantID string) (string, error) {
	num, err := n.NumberGenerator.NextCreditNoteNumberTx(ctx, tx, tenantID)
	if err == nil {
		n.p.hold()
	}
	return num, err
}

// run is one writer running in the background.
type run struct {
	done chan struct{}
	err  error
}

// start runs f, bounded so that a writer stuck behind a lock fails the test
// instead of hanging it.
func start(ctx context.Context, f func(context.Context) error) *run {
	r := &run{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		r.err = f(c)
	}()
	return r
}

// finishedWithin reports whether r ended within d.
func (r *run) finishedWithin(d time.Duration) bool {
	select {
	case <-r.done:
		return true
	case <-time.After(d):
		return false
	}
}

func (r *run) wait(t *testing.T, name string) {
	t.Helper()
	if !r.finishedWithin(30 * time.Second) {
		t.Fatalf("%s never ended", name)
	}
	if postgres.IsDeadlock(r.err) {
		t.Errorf("%s deadlocked: %v", name, r.err)
	} else if r.err != nil {
		t.Errorf("%s: %v", name, r.err)
	}
}

// holdLock takes a row lock on a transaction of the test's own, so that a
// writer that needs the row waits there with its earlier locks held. The
// returned func releases it.
func holdLock(t *testing.T, h *commitHarness, ctx context.Context, q string, args ...any) func() {
	t.Helper()
	tx, err := h.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var one int
	if err := tx.QueryRowContext(ctx, q, args...).Scan(&one); err != nil {
		postgres.Rollback(tx)
		t.Fatalf("hold %s: %v", q, err)
	}
	return func() { postgres.Rollback(tx) }
}

// lockRig is one customer with prepaid credit and the writers relief meets.
type lockRig struct {
	h        *commitHarness
	ctx      context.Context
	admin    *sql.DB
	refunds  *stripeRefunds
	relief   *creditnote.Service // card payments, refunds made in Stripe
	settings *tenant.SettingsStore
	n        int
}

func newLockRig(t *testing.T) *lockRig {
	h, ctx := newCommitHarness(t, "Relief Lock Order")
	refunds := &stripeRefunds{}
	r := &lockRig{h: h, ctx: ctx, admin: testutil.AdminPool(t), refunds: refunds,
		relief: newCardReliefService(h, refunds), settings: tenant.NewSettingsStore(h.db)}
	h.invStore.SetDisputeLossRecorder(r.relief)
	return r
}

// paidTopup is a fresh top-up, paid by card.
func (r *lockRig) paidTopup(t *testing.T) (domain.Invoice, string) {
	t.Helper()
	r.n++
	inv := r.h.createTopup(t, r.ctx, "USD", 9000, 10000)
	pi := fmt.Sprintf("pi_lock_%d", r.n)
	payByCard(t, r.h, r.ctx, inv.ID, pi)
	return inv, pi
}

func (r *lockRig) reliefOf(svc *creditnote.Service, invoiceID string) func(context.Context) error {
	return func(ctx context.Context) error {
		_, err := svc.CreateAndIssueCommitRelief(ctx, r.h.tenantID, creditnote.CommitReliefInput{InvoiceID: invoiceID, RetireCents: 1000})
		return err
	}
}

// pausedRelief is a relief service that holds after its number row.
func (r *lockRig) pausedRelief(p *pause) *creditnote.Service {
	svc := newCardReliefService(r.h, r.refunds)
	svc.SetNumberGenerator(pausedNumbers{r.settings, p})
	return svc
}

func (r *lockRig) recorder(svc *creditnote.Service, pi string) func(context.Context) error {
	r.n++
	snap := domain.StripeRefundSnapshot{
		StripeEventID: fmt.Sprintf("evt_lock_re_%d", r.n), EventAt: time.Now().UTC(), StripeRefundID: fmt.Sprintf("re_lock_%d", r.n),
		StripePaymentIntentID: pi, AmountCents: 100, Currency: "usd", Status: domain.StripeRefundSucceeded, CreatedAt: time.Now().UTC(),
	}
	return func(ctx context.Context) error {
		_, err := svc.RecordStripeRefund(ctx, r.h.tenantID, snap)
		return err
	}
}

// dispute records one event of a dispute on the payment, at the given offset
// from now. The rows use an inquiry, which relief does not refuse.
func (r *lockRig) dispute(disputeID, pi string, status domain.DisputeStatus, offset time.Duration) func(context.Context) error {
	at := time.Now().UTC().Truncate(time.Second).Add(offset)
	return func(ctx context.Context) error {
		_, err := r.h.invStore.RecordStripeDispute(ctx, r.h.tenantID, domain.DisputeSnapshot{
			StripeEventID: disputeID + "_" + string(status), EventAt: at, StripeDisputeID: disputeID,
			StripePaymentIntentID: pi, Status: status, AmountCents: 9000, Currency: "usd", OpenedAt: at,
		})
		return err
	}
}

// loss records a lost dispute of 10.00 on the payment through store, whose
// recorder writes the loss's note.
func (r *lockRig) loss(store *invoice.PostgresStore, pi string) func(context.Context) error {
	r.n++
	du := fmt.Sprintf("du_lock_loss_%d", r.n)
	at := time.Now().UTC().Truncate(time.Second)
	return func(ctx context.Context) error {
		_, err := store.RecordStripeDispute(ctx, r.h.tenantID, domain.DisputeSnapshot{
			StripeEventID: du + "_lost", EventAt: at, StripeDisputeID: du, StripePaymentIntentID: pi,
			Status: domain.DisputeLost, AmountCents: 1000, Currency: "usd", OpenedAt: at,
		})
		return err
	}
}

// unrecordedLoss records a lost dispute of 10.00 on the payment without its
// note (the usage not invoiced yet could not be read), and returns its id,
// for record-loss to write the note.
func (r *lockRig) unrecordedLoss(t *testing.T, inv domain.Invoice, pi string) string {
	t.Helper()
	svc := newCardReliefService(r.h, r.refunds)
	failing := &switchableUnbilled{}
	failing.fail.Store(true)
	svc.SetUnbilledUsageReader(failing)
	if err := r.loss(r.lossStore(svc), pi)(r.ctx); err != nil {
		t.Fatalf("loss without its note: %v", err)
	}
	ds, err := r.h.invStore.ListDisputesByInvoice(r.ctx, r.h.tenantID, inv.ID)
	if err != nil || len(ds) != 1 || ds[0].CreditNoteID != "" {
		t.Fatalf("disputes %+v, %v; want one loss without its note", ds, err)
	}
	return ds[0].ID
}

// flaggedRefund records a refund made in Stripe of 1.00 of the payment that
// Hardcount hears of two days late, so it is flagged for the linked create.
func (r *lockRig) flaggedRefund(t *testing.T, pi string) string {
	t.Helper()
	r.n++
	re := fmt.Sprintf("re_lock_late_%d", r.n)
	now := time.Now().UTC()
	if _, err := r.relief.RecordStripeRefund(r.ctx, r.h.tenantID, domain.StripeRefundSnapshot{
		StripeEventID: "evt_" + re, EventAt: now, StripeRefundID: re, StripePaymentIntentID: pi,
		AmountCents: 100, Currency: "usd", Status: domain.StripeRefundSucceeded, CreatedAt: now.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("flagged refund: %v", err)
	}
	return re
}

// linked records the flagged refund re with the operator's linked create
// through svc.
func (r *lockRig) linked(svc *creditnote.Service, inv domain.Invoice, pi, re string) func(context.Context) error {
	svc.SetRefundReader(fixedRefund{domain.StripeRefundFacts{ID: re, PaymentIntentID: pi, AmountCents: 100, Currency: "usd", Status: domain.StripeRefundSucceeded}})
	return func(ctx context.Context) error {
		_, err := svc.Create(ctx, r.h.tenantID, creditnote.CreateInput{
			InvoiceID: inv.ID, Reason: "Refunded in Stripe", RefundAmountCents: 100, AutoIssue: true, StripeRefundID: re,
			Lines: []creditnote.CreditLineInput{{Description: "Refund made in Stripe (" + re + ")", Quantity: 1, UnitAmountCents: 100}},
		})
		return err
	}
}

// lossStore is an invoice store whose dispute recorder writes a loss's note
// through svc.
func (r *lockRig) lossStore(svc *creditnote.Service) *invoice.PostgresStore {
	store := invoice.NewPostgresStore(r.h.db)
	store.SetDisputeLossRecorder(svc)
	return store
}

// refusalIsAnEnd counts a writer's refusal (422 or 409) as an end: what the
// test checks is that it ends, deadlock-free.
func refusalIsAnEnd(f func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := f(ctx); err != nil && !errors.Is(err, errs.ErrValidation) && !errors.Is(err, errs.ErrInvalidState) {
			return err
		}
		return nil
	}
}

// reachedWithin waits for a held writer to reach its hold.
func reachedWithin(t *testing.T, p *pause, name string) {
	t.Helper()
	select {
	case <-p.reached:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never reached its number row", name)
	}
}

func (r *lockRig) drawdown(t *testing.T) func(context.Context) error {
	r.n++
	usage := r.h.seedPayable(t, r.ctx, r.h.custID, 100, 1000+r.n)
	return func(ctx context.Context) error {
		_, err := credit.NewPostgresStore(r.h.db).ApplyToInvoiceAtomic(ctx, r.h.tenantID, r.h.custID, usage, "usage", 100, time.Now().UTC())
		return err
	}
}

// expiredGrant is a promotional grant past its expiry, not swept yet. Its
// expiry is set on the ledger: time would set it by passing.
func (r *lockRig) expiredGrant(t *testing.T) string {
	t.Helper()
	future := time.Now().Add(time.Hour)
	g, err := r.h.creditSvc.Grant(r.ctx, r.h.tenantID, credit.GrantInput{
		CustomerID: r.h.custID, Currency: "USD", AmountCents: 100, Description: "promo", ExpiresAt: &future, GrantKind: domain.GrantKindPromotional,
	})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	execBypassStamp(t, r.h.db, `UPDATE customer_credit_ledger SET expires_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, g.ID)
	return g.ID
}

func (r *lockRig) sweep(ctx context.Context) error {
	if _, errsList := r.h.creditSvc.ExpireCredits(ctx); len(errsList) > 0 {
		return errsList[0]
	}
	return nil
}

func (r *lockRig) payment(invoiceID, pi string) func(context.Context) error {
	return func(ctx context.Context) error {
		_, err := r.h.invStore.MarkPaid(ctx, r.h.tenantID, invoiceID, pi, time.Now().UTC())
		return err
	}
}

func TestCommitRelief_LockOrder(t *testing.T) {
	r := newLockRig(t)

	// Each writer is held inside its locks while a relief runs on a top-up
	// of the same customer. A writer whose locks relief does not need must
	// not hold it up (waits=false); one that holds the customer's credit
	// lock may (waits=true). Either way both end, with no deadlock.
	held := []struct {
		name  string
		waits bool
		// recorder marks the two recorders, which the recorders' own rows
		// below do not hold against each other.
		recorder bool
		// setup starts the writer and holds it; it returns the release.
		setup func(t *testing.T, inv domain.Invoice, pi string) (release func(), w *run)
	}{
		{"the Stripe refund recorder, holding the invoice row", false, true, func(t *testing.T, inv domain.Invoice, pi string) (func(), *run) {
			p := newPause()
			cnStore := creditnote.NewPostgresStore(r.h.db)
			svc := creditnote.NewService(cnStore, pausedInvoices{r.h.invStore, p}, r.refunds, reliefGranterAdapter{r.h.creditSvc})
			svc.SetNumberGenerator(r.settings)
			svc.SetStripeRefundRecording(cnStore, r.h.invStore, nopOutboxWriter{})
			w := start(r.ctx, r.recorder(svc, pi))
			<-p.reached
			return func() { close(p.release) }, w
		}},
		// The loss then writes its note on the same top-up, so the recorder
		// takes the credit-note lock relief holds.
		{"the dispute recorder recording a loss, holding the invoice row", false, true, func(t *testing.T, inv domain.Invoice, pi string) (func(), *run) {
			du := "du_lock_" + inv.ID
			if err := r.dispute(du, pi, domain.DisputeWarningNeedsResponse, 0)(r.ctx); err != nil {
				t.Fatalf("first dispute event: %v", err)
			}
			release := holdLock(t, r.h, r.ctx, `SELECT 1 FROM invoice_disputes WHERE stripe_dispute_id = $1 FOR UPDATE`, du)
			w := start(r.ctx, func(ctx context.Context) error {
				at := time.Now().UTC().Truncate(time.Second).Add(time.Second)
				_, err := r.h.invStore.RecordStripeDispute(ctx, r.h.tenantID, domain.DisputeSnapshot{
					StripeEventID: du + "_lost", EventAt: at, StripeDisputeID: du,
					StripePaymentIntentID: pi, Status: domain.DisputeLost, AmountCents: 1000, Currency: "usd", OpenedAt: at,
				})
				return err
			})
			testutil.WaitForLockWaiter(t, r.ctx, r.admin, "invoice_disputes")
			return release, w
		}},
		{"a drawdown, holding its invoice and the customer's credit lock", true, false, func(t *testing.T, _ domain.Invoice, _ string) (func(), *run) {
			gate := r.expiredGrant(t)
			release := holdLock(t, r.h, r.ctx, `SELECT 1 FROM customer_credit_ledger WHERE id = $1 FOR UPDATE`, gate)
			w := start(r.ctx, r.drawdown(t))
			testutil.WaitForLockWaiter(t, r.ctx, r.admin, "customer_credit_ledger")
			return release, w
		}},
		{"the expiry sweep, holding the customer's credit lock", true, false, func(t *testing.T, _ domain.Invoice, _ string) (func(), *run) {
			gate := r.expiredGrant(t)
			release := holdLock(t, r.h, r.ctx, `SELECT 1 FROM customer_credit_ledger WHERE id = $1 FOR UPDATE`, gate)
			w := start(r.ctx, r.sweep)
			testutil.WaitForLockWaiter(t, r.ctx, r.admin, "customer_credit_ledger")
			return release, w
		}},
		{"the payment of the customer's other top-up, holding it and the credit lock", true, false, func(t *testing.T, _ domain.Invoice, _ string) (func(), *run) {
			other := r.h.createTopup(t, r.ctx, "USD", 9000, 10000)
			release := holdLock(t, r.h, r.ctx, `SELECT 1 FROM tenant_settings WHERE tenant_id = $1 FOR UPDATE`, r.h.tenantID)
			w := start(r.ctx, r.payment(other.ID, "pi_lock_other_"+other.ID))
			testutil.WaitForLockWaiter(t, r.ctx, r.admin, "tenant_settings")
			return release, w
		}},
		{"a relief of the customer's other top-up, holding the number row", true, false, func(t *testing.T, _ domain.Invoice, _ string) (func(), *run) {
			other, _ := r.paidTopup(t)
			p := newPause()
			w := start(r.ctx, r.reliefOf(r.pausedRelief(p), other.ID))
			<-p.reached
			return func() { close(p.release) }, w
		}},
	}
	for _, tc := range held {
		t.Run("held: "+tc.name, func(t *testing.T) {
			inv, pi := r.paidTopup(t)
			release, w := tc.setup(t, inv, pi)
			relief := start(r.ctx, r.reliefOf(r.relief, inv.ID))
			waited := !relief.finishedWithin(3 * time.Second)
			if waited && tc.waits {
				testutil.WaitForLockWaitersOrDone(t, r.ctx, r.admin, "pg_advisory_xact_lock", 1, relief.done)
			}
			release()
			w.wait(t, tc.name)
			relief.wait(t, "relief")
			if waited && !tc.waits {
				t.Errorf("relief waited for %s, which holds no lock relief needs", tc.name)
			}
		})
	}

	// A relief is held after its number row, its last lock, while each
	// writer runs. A writer that needs none of relief's locks ends while it
	// is held: the dispute recorder, and a second payment of the top-up
	// relief holds (a payment's full row lock runs only on an unpaid top-up,
	// and relief holds only a paid one).
	running := []struct {
		name  string
		waits bool
		w     func(t *testing.T, inv domain.Invoice, pi string) func(context.Context) error
	}{
		{"the Stripe refund recorder", true, func(t *testing.T, _ domain.Invoice, pi string) func(context.Context) error {
			return r.recorder(r.relief, pi)
		}},
		{"the dispute recorder", false, func(t *testing.T, inv domain.Invoice, pi string) func(context.Context) error {
			return r.dispute("du_lock_run_"+inv.ID, pi, domain.DisputeWarningNeedsResponse, 0)
		}},
		{"the dispute recorder recording a loss", true, func(t *testing.T, _ domain.Invoice, pi string) func(context.Context) error {
			return r.loss(r.h.invStore, pi)
		}},
		{"a second payment of the same top-up", false, func(t *testing.T, inv domain.Invoice, pi string) func(context.Context) error {
			return r.payment(inv.ID, pi)
		}},
		{"the payment of the customer's other top-up", true, func(t *testing.T, _ domain.Invoice, _ string) func(context.Context) error {
			other := r.h.createTopup(t, r.ctx, "USD", 9000, 10000)
			return r.payment(other.ID, "pi_lock_run_"+other.ID)
		}},
		{"a drawdown", true, func(t *testing.T, _ domain.Invoice, _ string) func(context.Context) error { return r.drawdown(t) }},
		{"the expiry sweep", true, func(t *testing.T, _ domain.Invoice, _ string) func(context.Context) error {
			r.expiredGrant(t)
			return r.sweep
		}},
		{"another relief of the same top-up", true, func(t *testing.T, inv domain.Invoice, _ string) func(context.Context) error {
			return r.reliefOf(r.relief, inv.ID)
		}},
	}
	for _, tc := range running {
		t.Run("relief held: "+tc.name, func(t *testing.T) {
			inv, pi := r.paidTopup(t)
			writer := tc.w(t, inv, pi)
			p := newPause()
			relief := start(r.ctx, r.reliefOf(r.pausedRelief(p), inv.ID))
			<-p.reached
			w := start(r.ctx, writer)
			waited := !w.finishedWithin(3 * time.Second)
			close(p.release)
			relief.wait(t, "relief")
			w.wait(t, tc.name)
			if waited && !tc.waits {
				t.Errorf("%s waited for the relief, which holds no lock it needs", tc.name)
			}
		})
	}

	// The two recorders, record-loss and the linked create writing a note
	// on a top-up, against the writers that share its locks: each writer
	// held while one runs, then it held at its number row while each writer
	// runs. Each takes the customer's credit lock before the number row, as
	// relief does; numbering first deadlocks with the payment of the
	// customer's other top-up, which holds the credit lock and waits for the
	// number row.
	recorders := []struct {
		name string
		// prepare readies the top-up before any writer is held: the loss
		// record-loss writes, the refund the linked create records.
		prepare func(t *testing.T, inv domain.Invoice, pi string) string
		// note writes the note on the top-up paid by pi; held, it holds
		// after its number row.
		note func(held *pause, inv domain.Invoice, pi, prepared string) func(context.Context) error
	}{
		{"the Stripe refund recorder", nil, func(held *pause, _ domain.Invoice, pi, _ string) func(context.Context) error {
			if held == nil {
				return r.recorder(r.relief, pi)
			}
			return r.recorder(r.pausedRelief(held), pi)
		}},
		{"the dispute recorder", nil, func(held *pause, _ domain.Invoice, pi, _ string) func(context.Context) error {
			if held == nil {
				return r.loss(r.h.invStore, pi)
			}
			return r.loss(r.lossStore(r.pausedRelief(held)), pi)
		}},
		{"record-loss", r.unrecordedLoss, func(held *pause, inv domain.Invoice, _, disputeID string) func(context.Context) error {
			store := r.h.invStore
			if held != nil {
				store = r.lossStore(r.pausedRelief(held))
			}
			return func(ctx context.Context) error {
				_, err := store.RecordDisputeLoss(ctx, r.h.tenantID, inv.ID, disputeID)
				return err
			}
		}},
		{"the linked create", func(t *testing.T, _ domain.Invoice, pi string) string { return r.flaggedRefund(t, pi) },
			func(held *pause, inv domain.Invoice, pi, re string) func(context.Context) error {
				svc := newCardReliefService(r.h, r.refunds)
				if held != nil {
					svc = r.pausedRelief(held)
				}
				return r.linked(svc, inv, pi, re)
			}},
	}
	prepared := func(t *testing.T, prepare func(*testing.T, domain.Invoice, string) string, inv domain.Invoice, pi string) string {
		if prepare == nil {
			return ""
		}
		return prepare(t, inv, pi)
	}
	for _, rec := range recorders {
		for _, tc := range held {
			if tc.recorder {
				continue
			}
			t.Run(rec.name+" while held: "+tc.name, func(t *testing.T) {
				inv, pi := r.paidTopup(t)
				id := prepared(t, rec.prepare, inv, pi)
				release, w := tc.setup(t, inv, pi)
				note := start(r.ctx, rec.note(nil, inv, pi, id))
				if !note.finishedWithin(3 * time.Second) {
					testutil.WaitForLockWaitersOrDone(t, r.ctx, r.admin, "pg_advisory_xact_lock", 1, note.done)
				}
				release()
				w.wait(t, tc.name)
				note.wait(t, rec.name)
			})
		}
		for _, tc := range []struct {
			name string
			w    func(t *testing.T, inv domain.Invoice) func(context.Context) error
		}{
			{"the payment of the customer's other top-up", func(t *testing.T, _ domain.Invoice) func(context.Context) error {
				other := r.h.createTopup(t, r.ctx, "USD", 9000, 10000)
				return r.payment(other.ID, "pi_lock_rec_"+other.ID)
			}},
			{"a drawdown", func(t *testing.T, _ domain.Invoice) func(context.Context) error { return r.drawdown(t) }},
			{"the expiry sweep", func(t *testing.T, _ domain.Invoice) func(context.Context) error {
				r.expiredGrant(t)
				return r.sweep
			}},
			{"a relief of the customer's other top-up", func(t *testing.T, _ domain.Invoice) func(context.Context) error {
				other, _ := r.paidTopup(t)
				return r.reliefOf(r.relief, other.ID)
			}},
			// After a loss nothing more goes to the card, and a flagged refund
			// or a loss without its note stops relief, so this relief may be
			// refused, under the same locks.
			{"a relief of the same top-up", func(t *testing.T, inv domain.Invoice) func(context.Context) error {
				return refusalIsAnEnd(r.reliefOf(r.relief, inv.ID))
			}},
		} {
			t.Run(rec.name+" held: "+tc.name, func(t *testing.T) {
				inv, pi := r.paidTopup(t)
				id := prepared(t, rec.prepare, inv, pi)
				writer := tc.w(t, inv)
				p := newPause()
				note := start(r.ctx, rec.note(p, inv, pi, id))
				reachedWithin(t, p, rec.name)
				w := start(r.ctx, writer)
				w.finishedWithin(3 * time.Second)
				close(p.release)
				note.wait(t, rec.name)
				w.wait(t, tc.name)
			})
		}
	}

	// Every writer at once, round after round, with no hold.
	for round := 0; round < 10; round++ {
		inv, pi := r.paidTopup(t)
		other, otherPI := r.paidTopup(t)
		_, lostPI := r.paidTopup(t)
		unpaid := r.h.createTopup(t, r.ctx, "USD", 9000, 10000)
		r.expiredGrant(t)
		writers := map[string]func(context.Context) error{
			"relief":                   r.reliefOf(r.relief, inv.ID),
			"relief of the other":      r.reliefOf(r.relief, other.ID),
			"Stripe refund recorder":   r.recorder(r.relief, pi),
			"dispute recorder":         r.dispute(fmt.Sprintf("du_lock_burst_%d", round), pi, domain.DisputeWarningNeedsResponse, 0),
			"loss of a third":          r.loss(r.h.invStore, lostPI),
			"drawdown":                 r.drawdown(t),
			"expiry sweep":             r.sweep,
			"payment of an unpaid one": r.payment(unpaid.ID, "pi_lock_burst_"+unpaid.ID),
			"payment of the other":     r.payment(other.ID, otherPI),
		}
		runs := map[string]*run{}
		for name, f := range writers {
			runs[name] = start(r.ctx, f)
		}
		for name, w := range runs {
			w.wait(t, fmt.Sprintf("round %d: %s", round, name))
		}
	}
}

// nopOutboxWriter queues nothing.
type nopOutboxWriter struct{}

func (nopOutboxWriter) Enqueue(context.Context, *sql.Tx, string, string, map[string]any) (string, error) {
	return "evt_lock", nil
}
