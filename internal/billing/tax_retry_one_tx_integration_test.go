package billing_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/tax"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/usage"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// P1-29: a tax retry writes the tax and finalizes the draft in one
// transaction (UpdateTaxAtomic's then). These tests drive the real engine,
// invoice service and store against real Postgres, with a tax provider whose
// calculation can run a step of the test's choosing.

// hookedTax is a Stripe Tax stand-in: the manual provider's arithmetic, a
// calculation id per call, and onCalculate run inside each calculation.
type hookedTax struct {
	tax.Provider
	calls       int
	onCalculate func(ctx context.Context)
}

func (p *hookedTax) Name() string { return "stripe_tax" }

func (p *hookedTax) Calculate(ctx context.Context, req tax.Request) (*tax.Result, error) {
	p.calls++
	res, err := p.Provider.Calculate(ctx, req)
	if err != nil {
		return nil, err
	}
	res.CalculationID = fmt.Sprintf("taxcalc_p129_%d", p.calls)
	if f := p.onCalculate; f != nil {
		p.onCalculate = nil
		f(ctx)
	}
	return res, nil
}

// recordingCommitter is the invoice service's Stripe Tax commit: it records
// the commits a finalize's steps make, and the ctx each ran on.
type recordingCommitter struct {
	commits     []string
	ctxErrs     []error
	hasDeadline []bool
}

func (c *recordingCommitter) CommitTax(ctx context.Context, _, invoiceID, _ string) error {
	c.commits = append(c.commits, invoiceID)
	c.ctxErrs = append(c.ctxErrs, ctx.Err())
	_, ok := ctx.Deadline()
	c.hasDeadline = append(c.hasDeadline, ok)
	return nil
}

func (c *recordingCommitter) CheckTaxCalculationUsable(context.Context, string, string, string) error {
	return nil
}

// cutCommitInvoices is the engine's invoice store with the commit-cut seam:
// after a successful UpdateTaxAtomic it cancels the caller's request and, if
// the ctx the write ran on is now done, returns context.Canceled. That is
// pgx committing on the transaction's ctx (stdlib Commit uses the BeginTx
// ctx), emulated at the store.
type cutCommitInvoices struct {
	*invoiceStoreAdapter
	cancelRequest context.CancelFunc
}

func (s *cutCommitInvoices) UpdateTaxAtomic(ctx context.Context, tenantID, invoiceID string, update domain.InvoiceTaxRetryUpdate, lines []domain.InvoiceLineItem,
	then func(ctx context.Context, tx *sql.Tx, taxed domain.Invoice) error,
) (domain.Invoice, error) {
	inv, err := s.invoiceStoreAdapter.UpdateTaxAtomic(ctx, tenantID, invoiceID, update, lines, then)
	if err != nil || s.cancelRequest == nil {
		return inv, err
	}
	s.cancelRequest()
	if ctx.Err() != nil {
		return domain.Invoice{}, context.Canceled
	}
	return inv, nil
}

// failingRelief fails finalize's held-relief step, on the finalize's
// transaction.
type failingRelief struct{}

func (failingRelief) IssueHeldReliefTx(context.Context, *sql.Tx, string, domain.Invoice) (func(context.Context), error) {
	return nil, errors.New("relief credit note insert failed")
}

// stalledRelief waits in finalize's held-relief step until the transaction's
// ctx is done: a lock wait or a slow database that outlasts the write's
// budget.
type stalledRelief struct{}

func (stalledRelief) IssueHeldReliefTx(ctx context.Context, _ *sql.Tx, _ string, _ domain.Invoice) (func(context.Context), error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type oneTxRig struct {
	t         *testing.T
	ctx       context.Context
	db        *postgres.DB
	tenantID  string
	custID    string
	store     *invoice.PostgresStore
	invoices  *cutCommitInvoices
	engine    *billing.Engine
	svc       *invoice.Service
	tax       *hookedTax
	committer *recordingCommitter
}

func newOneTxRig(t *testing.T, name string) *oneTxRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	customers := customer.NewPostgresStore(db)
	cust, err := customers.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_p129", DisplayName: "P129 Buyer"})
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	settings := tenant.NewSettingsStore(db)
	store := invoice.NewPostgresStore(db)
	store.SetOutboxEnqueuer(webhook.NewOutboxStore(db))
	invs := &cutCommitInvoices{invoiceStoreAdapter: &invoiceStoreAdapter{store}}
	subs := subscription.NewPostgresStore(db)
	ht := &hookedTax{Provider: tax.NewManualProvider(10, "VAT")}
	engine := billing.NewEngine(
		&subStoreAdapter{subs}, &usageStoreAdapter{usage.NewPostgresStore(db)},
		&pricingStoreAdapter{pricing.NewPostgresStore(db)}, invs,
		credit.NewService(credit.NewPostgresStore(db)), settings, testPaymentSetupsNoPM{}, testChargerSentinel{},
		clock.Real(), customers,
	)
	engine.SetTaxProviderResolver(fixedTaxResolver{ht})
	committer := &recordingCommitter{}
	svc := invoice.NewService(store, clock.Real(), settings)
	svc.SetTaxRetrier(engine)
	svc.SetTaxCommitter(committer)
	svc.SetAuditLogger(audit.NewLogger(db))
	return &oneTxRig{t: t, ctx: ctx, db: db, tenantID: tenantID, custID: cust.ID,
		store: store, invoices: invs, engine: engine, svc: svc, tax: ht, committer: committer}
}

// draft seeds a billing draft (no subscription) whose Stripe Tax
// calculation is parked on a provider outage: the row the tax_retry sweep
// lists and a retry finalizes.
func (r *oneTxRig) draft(num string) domain.Invoice {
	r.t.Helper()
	now := time.Now().UTC()
	d, err := r.store.CreateWithLineItems(r.ctx, r.tenantID, domain.Invoice{
		CustomerID: r.custID, InvoiceNumber: num, Status: domain.InvoiceDraft, PaymentStatus: domain.PaymentPending,
		BillingReason: domain.BillingReasonSubscriptionCycle, Currency: "USD",
		SubtotalCents: 10000, TotalAmountCents: 10000, AmountDueCents: 10000,
		BillingPeriodStart: now.AddDate(0, -1, 0), BillingPeriodEnd: now,
		TaxFacts: domain.TaxFacts{TaxProvider: "stripe_tax", TaxStatus: domain.InvoiceTaxPending, TaxErrorCode: "provider_outage"},
	}, []domain.InvoiceLineItem{{
		LineType: domain.LineTypeBaseFee, Description: "Pro", Quantity: 1,
		UnitAmountCents: 10000, AmountCents: 10000, TotalAmountCents: 10000,
	}})
	if err != nil {
		r.t.Fatalf("seed draft: %v", err)
	}
	return d
}

func (r *oneTxRig) get(id string) domain.Invoice {
	r.t.Helper()
	inv, err := r.store.Get(r.ctx, r.tenantID, id)
	if err != nil {
		r.t.Fatalf("get: %v", err)
	}
	return inv
}

func (r *oneTxRig) count(query string, args ...any) int {
	r.t.Helper()
	var n int
	if err := r.db.WithTenantTx(r.ctx, r.tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.ctx, query, args...).Scan(&n)
	}); err != nil {
		r.t.Fatalf("count: %v", err)
	}
	return n
}

// finalizeTrail counts what a finalize leaves: finalize audit rows and
// invoice.finalized events.
func (r *oneTxRig) finalizeTrail(id string) (auditRows, events int) {
	r.t.Helper()
	auditRows = r.count(`SELECT count(*) FROM audit_log WHERE resource_id = $1 AND action = $2`, id, domain.AuditActionFinalize)
	events = r.count(`SELECT count(*) FROM webhook_outbox WHERE event_type = $1 AND payload->'invoice'->>'id' = $2`, domain.EventInvoiceFinalized, id)
	return auditRows, events
}

// listed reports whether the tax_retry sweep's query lists the row now.
func (r *oneTxRig) listed(id string) bool {
	r.t.Helper()
	rows, err := r.store.ListPendingTaxRetry(r.ctx, 100000, []string{"provider_outage"}, domain.MaxTaxRetryAttempts, false)
	if err != nil {
		r.t.Fatalf("list pending tax retry: %v", err)
	}
	for _, inv := range rows {
		if inv.ID == id {
			return true
		}
	}
	return false
}

// TestRetryTax_CancelledDuringCommit_InvoiceIsFinalized: the request is
// cancelled while the tax write's COMMIT is in flight. The write runs on a
// ctx the caller cannot cancel, so the commit stands and is reported as
// such: RetryTax returns the finalized invoice, and the finalize's steps run.
// On main the write ran on the request's ctx: the error came back for a
// commit that had applied.
//
// Mutation: build the write's ctx without WithoutCancel, and the cut commit
// returns context canceled.
func TestRetryTax_CancelledDuringCommit_InvoiceIsFinalized(t *testing.T) {
	r := newOneTxRig(t, "P129 Commit Cut")
	d := r.draft("P129-CUT-1")
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	r.invoices.cancelRequest = cancel

	out, err := r.svc.RetryTax(ctx, r.tenantID, d.ID)
	if err != nil {
		t.Fatalf("RetryTax: %v; want nil, the commit applied", err)
	}
	got := r.get(d.ID)
	if got.Status != domain.InvoiceFinalized || out.Status != domain.InvoiceFinalized || !got.AutoChargePending {
		t.Errorf("status = %s (returned %s), queued %v; want finalized and queued", got.Status, out.Status, got.AutoChargePending)
	}
	if len(r.committer.commits) != 1 {
		t.Fatalf("Stripe Tax commits = %d, want 1: the finalize's steps run after the commit", len(r.committer.commits))
	}
	if r.committer.ctxErrs[0] != nil || !r.committer.hasDeadline[0] {
		t.Errorf("the Stripe Tax commit ran on a ctx with error %v, deadline %v; want a live ctx with a deadline (stepsBudget), not the cancelled request's", r.committer.ctxErrs[0], r.committer.hasDeadline[0])
	}
}

// TestRetryTax_FinalizeFails_TaxStaysPendingAndTheTryIsCounted: the finalize
// riding on the tax write fails (its held-relief step), so the tax write
// rolls back with it. The row is still a draft with tax pending, which is
// true; the try is counted and backed off, so the 8-try cap and the backoff
// hold; and nothing of a finalize is left (no token, event or audit row).
// On main the tax write stayed: a draft with tax ok, listed by no sweep.
//
// Mutations: commit before then; the engine passes nil for then; the hook
// swallows the finalize error; RecordTaxRetryAttempt dropped.
func TestRetryTax_FinalizeFails_TaxStaysPendingAndTheTryIsCounted(t *testing.T) {
	r := newOneTxRig(t, "P129 Finalize Fails")
	r.svc.SetHeldReliefIssuer(failingRelief{})
	d := r.draft("P129-FAIL-1")

	_, err := r.svc.RetryTax(r.ctx, r.tenantID, d.ID)
	if err == nil || !strings.Contains(err.Error(), "could not be finalized") || !strings.Contains(err.Error(), "relief credit note insert failed") {
		t.Fatalf("RetryTax error = %v, want one naming the failed finalize and its cause", err)
	}
	got := r.get(d.ID)
	if got.Status != domain.InvoiceDraft || got.TaxStatus != domain.InvoiceTaxPending || got.TaxErrorCode != "provider_outage" {
		t.Fatalf("status = %s, tax_status = %s, code = %q; want a draft with tax pending and the last tax error", got.Status, got.TaxStatus, got.TaxErrorCode)
	}
	if got.TaxRetryCount != 1 {
		t.Errorf("tax_retry_count = %d, want 1: the failed try is counted", got.TaxRetryCount)
	}
	if got.TaxNextRetryAt == nil || !got.TaxNextRetryAt.After(time.Now()) {
		t.Errorf("tax_next_retry_at = %v, want a time ahead (the backoff)", got.TaxNextRetryAt)
	}
	if r.listed(d.ID) {
		t.Error("the tax_retry sweep lists the row before its next retry time")
	}
	if a, e := r.finalizeTrail(d.ID); got.PublicToken != "" || a != 0 || e != 0 {
		t.Errorf("token %q, %d finalize audit rows, %d invoice.finalized; want none", got.PublicToken, a, e)
	}

	for i := 2; i <= domain.MaxTaxRetryAttempts; i++ {
		if _, err := r.svc.RetryTax(r.ctx, r.tenantID, d.ID); err == nil {
			t.Fatalf("try %d: RetryTax = nil, want the error", i)
		}
	}
	got = r.get(d.ID)
	if got.TaxRetryCount != domain.MaxTaxRetryAttempts || got.TaxNextRetryAt != nil {
		t.Errorf("after %d tries: count = %d, next = %v; want %d and none", domain.MaxTaxRetryAttempts, got.TaxRetryCount, got.TaxNextRetryAt, domain.MaxTaxRetryAttempts)
	}
	if r.listed(d.ID) {
		t.Errorf("the tax_retry sweep still lists the row after %d tries: the cap ends the listing", domain.MaxTaxRetryAttempts)
	}
}

// TestRetryTax_CancelledDuringCalculation_WritesNothing: a caller cancelled
// while Stripe Tax calculates writes nothing and counts no try, as on main,
// where the write failed on the dead ctx. The write now runs on a ctx the
// caller cannot cancel, so the engine checks the caller's ctx first.
//
// Mutation: drop the ctx.Err check, and the cancelled call writes the tax
// and finalizes.
func TestRetryTax_CancelledDuringCalculation_WritesNothing(t *testing.T) {
	r := newOneTxRig(t, "P129 Calc Cancelled")
	d := r.draft("P129-CALC-1")
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	r.tax.onCalculate = func(context.Context) { cancel() }

	if _, err := r.svc.RetryTax(ctx, r.tenantID, d.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("RetryTax error = %v, want context canceled", err)
	}
	got := r.get(d.ID)
	if got.Status != domain.InvoiceDraft || got.TaxStatus != domain.InvoiceTaxPending || got.TaxRetryCount != 0 || got.TaxNextRetryAt != nil {
		t.Errorf("status = %s, tax_status = %s, tries = %d, next = %v; want the row unchanged", got.Status, got.TaxStatus, got.TaxRetryCount, got.TaxNextRetryAt)
	}
}

// TestRetryTax_RivalFinalizesDuringTheCalculation_AnswersConflict: a second
// retry of the same row (the connect fan-out and the tax_retry sweep, or an
// operator's click) runs to the end while the first is calculating. The
// first one's write finds the invoice finalized and is refused with a 409,
// the answer the engine's own draft check gives a moment earlier. The
// winner's finalize is the only one: one token, one invoice.finalized, one
// finalize audit row, one Stripe Tax commit. The loser counts no try.
//
// Mutation: RecordTaxRetryAttempt on every write error does not show here
// (the row is no longer a draft); TestRetryTax_WriteRefusedForAnotherReason
// catches it.
func TestRetryTax_RivalFinalizesDuringTheCalculation_AnswersConflict(t *testing.T) {
	r := newOneTxRig(t, "P129 Rival")
	d := r.draft("P129-RIVAL-1")
	var winner domain.Invoice
	r.tax.onCalculate = func(context.Context) {
		var err error
		if winner, err = r.svc.RetryTax(r.ctx, r.tenantID, d.ID); err != nil {
			t.Errorf("rival RetryTax: %v", err)
		}
	}

	_, err := r.svc.RetryTax(r.ctx, r.tenantID, d.ID)
	if !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("RetryTax error = %v, want an invalid-state refusal (409): a rival finalized the invoice", err)
	}
	got := r.get(d.ID)
	if got.Status != domain.InvoiceFinalized || got.PublicToken == "" || got.PublicToken != winner.PublicToken {
		t.Errorf("status = %s, token %q (winner's %q); want the winner's finalize", got.Status, got.PublicToken, winner.PublicToken)
	}
	if a, e := r.finalizeTrail(d.ID); a != 1 || e != 1 {
		t.Errorf("%d finalize audit rows, %d invoice.finalized; want 1 and 1", a, e)
	}
	if len(r.committer.commits) != 1 {
		t.Errorf("Stripe Tax commits = %d, want 1 (the winner's)", len(r.committer.commits))
	}
	if got.TaxRetryCount != 1 {
		t.Errorf("tax_retry_count = %d, want 1 (the winner's try; the loser counts none)", got.TaxRetryCount)
	}
}

// TestRetryTax_WriteRefusedForAnotherReason_TheTryIsNotCounted: only a
// finalize failure counts a try. A write refused because the invoice changed
// during the calculation (invoice_lines_changed) is not a failed try: the
// row is as it was and the next retry computes again.
//
// Mutation: RecordTaxRetryAttempt on every write error, and the count moves.
func TestRetryTax_WriteRefusedForAnotherReason_TheTryIsNotCounted(t *testing.T) {
	r := newOneTxRig(t, "P129 Lines Changed")
	d := r.draft("P129-LINES-1")
	r.tax.onCalculate = func(context.Context) {
		if err := r.db.WithTenantTx(r.ctx, r.tenantID, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(r.ctx, `UPDATE invoices SET subtotal_cents = subtotal_cents + 1 WHERE id = $1`, d.ID)
			return err
		}); err != nil {
			t.Errorf("change the invoice during the calculation: %v", err)
		}
	}

	if _, err := r.svc.RetryTax(r.ctx, r.tenantID, d.ID); errs.Code(err) != "invoice_lines_changed" {
		t.Fatalf("RetryTax error = %v, want invoice_lines_changed", err)
	}
	got := r.get(d.ID)
	if got.Status != domain.InvoiceDraft || got.TaxStatus != domain.InvoiceTaxPending || got.TaxRetryCount != 0 {
		t.Errorf("status = %s, tax_status = %s, tries = %d; want a draft with tax pending and no try counted", got.Status, got.TaxStatus, got.TaxRetryCount)
	}
}

// TestRetryTaxForInvoice_PassesTheHook: the engine runs the caller's hook on
// the tax write's transaction, with the taxed row. A hook error rolls the
// write back, is returned, and counts the try.
//
// Mutation: the engine passes nil for then.
func TestRetryTaxForInvoice_PassesTheHook(t *testing.T) {
	r := newOneTxRig(t, "P129 Engine Hook")

	t.Run("the hook sees the taxed row on the transaction", func(t *testing.T) {
		d := r.draft("P129-HOOK-1")
		var seen domain.Invoice
		var gotTx bool
		out, err := r.engine.RetryTaxForInvoice(r.ctx, r.tenantID, d.ID, func(ctx context.Context, tx *sql.Tx, taxed domain.Invoice) error {
			seen, gotTx = taxed, tx != nil
			_, err := tx.ExecContext(ctx, `UPDATE invoices SET memo = 'hook ran' WHERE id = $1`, taxed.ID)
			return err
		})
		if err != nil {
			t.Fatalf("RetryTaxForInvoice: %v", err)
		}
		if !gotTx || seen.TaxStatus != domain.InvoiceTaxOK || seen.TaxCalculationID == "" {
			t.Errorf("hook ran with tx %v, tax_status %s, calculation %q; want a tx and the taxed row", gotTx, seen.TaxStatus, seen.TaxCalculationID)
		}
		if out.Memo != "hook ran" {
			t.Errorf("returned memo = %q, want the hook's write", out.Memo)
		}
	})

	t.Run("a hook error rolls the write back and counts the try", func(t *testing.T) {
		d := r.draft("P129-HOOK-2")
		hookErr := errors.New("finalize failed")
		if _, err := r.engine.RetryTaxForInvoice(r.ctx, r.tenantID, d.ID, func(context.Context, *sql.Tx, domain.Invoice) error {
			return hookErr
		}); !errors.Is(err, hookErr) {
			t.Fatalf("RetryTaxForInvoice error = %v, want the hook's", err)
		}
		got := r.get(d.ID)
		if got.TaxStatus != domain.InvoiceTaxPending || got.TaxCalculationID != "" || got.TaxRetryCount != 1 {
			t.Errorf("tax_status = %s, calculation %q, tries = %d; want pending, none, 1", got.TaxStatus, got.TaxCalculationID, got.TaxRetryCount)
		}
	})
}

// TestRetryTax_FinalizeOutlastsItsBudget_TheTryIsStillCounted: the finalize
// stalls until the write's budget runs out, so the transaction fails on its
// deadline. The try is counted on a ctx of its own, so the backoff and the
// cap still hold; counted on the spent ctx, it would fail at once and the
// sweep would list the row every tick with no cap.
//
// Mutation: count the try on the write's ctx, and the count stays 0.
func TestRetryTax_FinalizeOutlastsItsBudget_TheTryIsStillCounted(t *testing.T) {
	billing.ShortenCommitBudget(t, 300*time.Millisecond)
	r := newOneTxRig(t, "P129 Budget Spent")
	r.svc.SetHeldReliefIssuer(stalledRelief{})
	d := r.draft("P129-BUDGET-1")

	if _, err := r.svc.RetryTax(r.ctx, r.tenantID, d.ID); err == nil {
		t.Fatal("RetryTax = nil with a finalize past its budget, want the error")
	}
	got := r.get(d.ID)
	if got.Status != domain.InvoiceDraft || got.TaxStatus != domain.InvoiceTaxPending {
		t.Fatalf("status = %s, tax_status = %s; want a draft with tax pending", got.Status, got.TaxStatus)
	}
	if got.TaxRetryCount != 1 || got.TaxNextRetryAt == nil || !got.TaxNextRetryAt.After(time.Now()) {
		t.Errorf("tries = %d, next retry %v; want 1 and a time ahead: the try is counted on its own ctx", got.TaxRetryCount, got.TaxNextRetryAt)
	}
}

// blockingFinalize lets the first FinalizeDraftTx run, then holds its
// transaction open (the invoice row locked) until release is closed.
type blockingFinalize struct {
	*invoice.PostgresStore
	once     sync.Once
	finished chan struct{}
	release  chan struct{}
}

func (s *blockingFinalize) FinalizeDraftTx(ctx context.Context, tx *sql.Tx, tenantID, id string, onFinalized func(*sql.Tx, domain.Invoice) error) (domain.Invoice, error) {
	inv, err := s.PostgresStore.FinalizeDraftTx(ctx, tx, tenantID, id, onFinalized)
	s.once.Do(func() {
		close(s.finished)
		<-s.release
	})
	return inv, err
}

// TestRetryTax_TwoRetriesContendOnTheRow_TheSecondAnswersConflict: two
// retries of one row both get past the calculation. A writes its tax and
// finalizes, and holds the row; B's write waits on the row lock. When A
// commits, B finds the invoice finalized and answers 409, writing nothing:
// the row keeps A's calculation, with one finalize audit row and one
// invoice.finalized, and B counts no try. When the finalize was a second
// transaction, B rewrote the tax after A's tax write and then lost the
// finalize, answering nil.
func TestRetryTax_TwoRetriesContendOnTheRow_TheSecondAnswersConflict(t *testing.T) {
	r := newOneTxRig(t, "P129 Contention")
	d := r.draft("P129-CONTEND-1")
	blocking := &blockingFinalize{PostgresStore: r.store, finished: make(chan struct{}), release: make(chan struct{})}
	svc := invoice.NewService(blocking, clock.Real(), tenant.NewSettingsStore(r.db))
	svc.SetTaxRetrier(r.engine)
	svc.SetTaxCommitter(r.committer)
	svc.SetAuditLogger(audit.NewLogger(r.db))

	aDone := make(chan error, 1)
	go func() {
		_, err := svc.RetryTax(r.ctx, r.tenantID, d.ID)
		aDone <- err
	}()
	<-blocking.finished
	bDone := make(chan error, 1)
	go func() {
		_, err := svc.RetryTax(r.ctx, r.tenantID, d.ID)
		bDone <- err
	}()
	testutil.WaitForLockWaiter(t, r.ctx, testutil.AdminPool(t), "%FROM invoices WHERE id = $1 FOR UPDATE%")
	close(blocking.release)

	if err := <-aDone; err != nil {
		t.Fatalf("A: RetryTax = %v, want nil", err)
	}
	if err := <-bDone; !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("B: RetryTax error = %v, want invalid_state (409): A finalized the invoice", err)
	}
	got := r.get(d.ID)
	if got.Status != domain.InvoiceFinalized || got.TaxCalculationID != "taxcalc_p129_1" {
		t.Errorf("status = %s, calculation %q; want finalized with A's calculation taxcalc_p129_1", got.Status, got.TaxCalculationID)
	}
	if a, e := r.finalizeTrail(d.ID); a != 1 || e != 1 {
		t.Errorf("%d finalize audit rows, %d invoice.finalized; want 1 and 1", a, e)
	}
	if got.TaxRetryCount != 1 {
		t.Errorf("tax_retry_count = %d, want 1 (A's try; B counts none)", got.TaxRetryCount)
	}
	if r.tax.calls != 2 {
		t.Errorf("tax calculations = %d, want 2 (both retries got past the calculation)", r.tax.calls)
	}
}
