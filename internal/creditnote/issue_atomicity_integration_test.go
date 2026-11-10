package creditnote_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tax"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// failingGranterTx fails the in-tx grant — the exact internal-effect failure the
// Issue() coordinator tx must roll the draft→issued CAS back from (ADR-061).
type failingGranterTx struct{}

func (failingGranterTx) GrantForCreditNoteTx(context.Context, *sql.Tx, string, string, creditnote.CreditGrantInput) error {
	return errors.New("injected in-tx grant failure")
}
func (failingGranterTx) LockCommitGrantForReliefTx(context.Context, *sql.Tx, string, string) (int64, int64, int64, bool, error) {
	return 0, 0, 0, false, nil
}
func (failingGranterTx) RetireCommitSliceForReliefTx(context.Context, *sql.Tx, string, string, string, int64, int64, int64) (int64, error) {
	return 0, errors.New("not a commit test")
}

// TestIssue_GrantFailure_RollsBackCAS is the real-Postgres proof of the ADR-061
// atomicity guarantee: when the INTERNAL credit grant fails inside Issue()'s
// coordinator tx, the draft→issued CAS rolls back WITH it — so the credit note
// stays 'draft' and the formerly-possible issued-but-ungranted orphan cannot
// exist. Pre-PR2 the CAS committed in its OWN tx, so a grant failure left a CN
// shown as completed with no balance credit and no automatic recovery. The
// in-memory double can't model rollback; this proves it against real tx
// semantics, then confirms a clean re-drive (with a working granter) succeeds.
func TestIssue_GrantFailure_RollsBackCAS(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Issue Atomicity")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_issueatom", DisplayName: "Issue Atom",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	now := time.Now().UTC()
	issued := now
	inv, err := invoice.NewPostgresStore(db).CreateWithLineItems(ctx, tenantID, domain.Invoice{
		CustomerID:         cust.ID,
		InvoiceNumber:      "INV-ISSUEATOM-1",
		Status:             domain.InvoicePaid,
		PaymentStatus:      domain.PaymentSucceeded,
		Currency:           "USD",
		SubtotalCents:      10000,
		TotalAmountCents:   10000,
		AmountDueCents:     0,
		AmountPaidCents:    10000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:   now,
		IssuedAt:           &issued,
	}, nil)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	store := creditnote.NewPostgresStore(db)
	cn, err := store.Create(ctx, tenantID, domain.CreditNote{
		InvoiceID:         inv.ID,
		CustomerID:        cust.ID,
		CreditNoteNumber:  "CN-ISSUEATOM-1",
		Status:            domain.CreditNoteDraft,
		Reason:            "fraudulent",
		SubtotalCents:     5000,
		TotalCents:        5000,
		CreditAmountCents: 5000, // credit-type → Issue() takes the in-tx grant path
		Currency:          "USD",
		RefundStatus:      domain.RefundNone,
	})
	if err != nil {
		t.Fatalf("create credit note: %v", err)
	}

	// --- Failure path: the in-tx grant errors → the CAS must roll back. ---
	failSvc := creditnote.NewService(store, invoice.NewPostgresStore(db), nil, failingGranterTx{})
	if _, err := failSvc.Issue(ctx, tenantID, cn.ID); err == nil {
		t.Fatal("expected Issue to fail when the in-tx grant errors")
	}

	got, err := store.Get(ctx, tenantID, cn.ID)
	if err != nil {
		t.Fatalf("get credit note after failed issue: %v", err)
	}
	if got.Status != domain.CreditNoteDraft {
		t.Fatalf("status after grant failure: got %q, want draft — the CAS must roll back with the failed grant (no issued-but-ungranted orphan)", got.Status)
	}
	if got.IssuedAt != nil {
		t.Errorf("issued_at must be nil after rollback, got %v", got.IssuedAt)
	}

	// --- Recovery path: a clean re-drive (working granter) now succeeds. ---
	okSvc := creditnote.NewService(store, invoice.NewPostgresStore(db), nil, okGranterTx{})
	if _, err := okSvc.Issue(ctx, tenantID, cn.ID); err != nil {
		t.Fatalf("re-issue after a clean rollback should succeed: %v", err)
	}
	got, err = store.Get(ctx, tenantID, cn.ID)
	if err != nil {
		t.Fatalf("get credit note after re-issue: %v", err)
	}
	if got.Status != domain.CreditNoteIssued {
		t.Errorf("status after clean re-drive: got %q, want issued", got.Status)
	}
}

// okGranterTx is the working-granter counterpart used by the recovery leg.
type okGranterTx struct{}

func (okGranterTx) GrantForCreditNoteTx(context.Context, *sql.Tx, string, string, creditnote.CreditGrantInput) error {
	return nil
}
func (okGranterTx) LockCommitGrantForReliefTx(context.Context, *sql.Tx, string, string) (int64, int64, int64, bool, error) {
	return 0, 0, 0, false, nil
}
func (okGranterTx) RetireCommitSliceForReliefTx(context.Context, *sql.Tx, string, string, string, int64, int64, int64) (int64, error) {
	return 0, nil
}

// reversalFixture is one tenant and customer with the two stores the tax
// reversal tests use.
type reversalFixture struct {
	t        *testing.T
	db       *postgres.DB
	ctx      context.Context
	tenantID string
	custID   string
	invoices *invoice.PostgresStore
	notes    *creditnote.PostgresStore
}

func newReversalFixture(t *testing.T, name string) *reversalFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_" + strings.ReplaceAll(strings.ToLower(name), " ", "_"), DisplayName: name,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	return &reversalFixture{t: t, db: db, ctx: ctx, tenantID: tenantID, custID: cust.ID,
		invoices: invoice.NewPostgresStore(db), notes: creditnote.NewPostgresStore(db)}
}

// invoice makes a paid invoice taxed by provider, with taxCents of tax and,
// when taxTx is set, a recorded upstream tax transaction. A stripe_tax
// invoice carries its calculation id, as every invoice Stripe Tax
// calculated does.
func (f *reversalFixture) invoice(num, provider string, taxCents int64, taxTx string) domain.Invoice {
	f.t.Helper()
	calc := ""
	if provider == "stripe_tax" {
		calc = "taxcalc_" + num
	}
	return f.invoiceWith(num, provider, taxCents, calc, taxTx)
}

// uncalculatedInvoice makes a paid invoice of a Stripe Tax tenant that Stripe
// Tax never calculated: no calculation, no transaction, no tax. Hardcount
// stores it so for an exempt or reverse-charge customer, an invoice with no
// taxable lines, and an invoice that only sells prepaid credit.
func (f *reversalFixture) uncalculatedInvoice(num string) domain.Invoice {
	f.t.Helper()
	return f.invoiceWith(num, "stripe_tax", 0, "", "")
}

func (f *reversalFixture) invoiceWith(num, provider string, taxCents int64, calc, taxTx string) domain.Invoice {
	f.t.Helper()
	now := time.Now().UTC()
	inv, err := f.invoices.CreateWithLineItems(f.ctx, f.tenantID, domain.Invoice{
		CustomerID: f.custID, InvoiceNumber: num,
		Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
		Currency: "USD", SubtotalCents: 10000,
		TaxFacts:         domain.TaxFacts{TaxAmountCents: taxCents, TaxProvider: provider, TaxCalculationID: calc},
		TotalAmountCents: 10000 + taxCents, AmountPaidCents: 10000 + taxCents,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now,
		IssuedAt: &now,
	}, nil)
	if err != nil {
		f.t.Fatalf("create invoice %s: %v", num, err)
	}
	if taxTx != "" {
		if err := f.invoices.SetTaxTransaction(f.ctx, f.tenantID, inv.ID, taxTx); err != nil {
			f.t.Fatalf("stamp tax tx %s: %v", num, err)
		}
	}
	return inv
}

// issue creates a draft credit note of totalCents on inv and issues it through
// the store's issue statement, returning the row as stored.
func (f *reversalFixture) issue(num string, inv domain.Invoice, totalCents int64) domain.CreditNote {
	f.t.Helper()
	cn, err := f.notes.Create(f.ctx, f.tenantID, domain.CreditNote{
		InvoiceID: inv.ID, CustomerID: f.custID, CreditNoteNumber: num,
		Status: domain.CreditNoteDraft, Reason: "fraudulent",
		SubtotalCents: totalCents, TotalCents: totalCents,
		Currency: "USD", RefundStatus: domain.RefundNone,
	})
	if err != nil {
		f.t.Fatalf("create credit note %s: %v", num, err)
	}
	won, err := f.notes.TransitionStatus(f.ctx, f.tenantID, cn.ID, domain.CreditNoteDraft, domain.CreditNoteIssued)
	if err != nil || !won {
		f.t.Fatalf("issue %s: won=%v err=%v", num, won, err)
	}
	return f.note(cn.ID)
}

func (f *reversalFixture) note(id string) domain.CreditNote {
	f.t.Helper()
	cn, err := f.notes.Get(f.ctx, f.tenantID, id)
	if err != nil {
		f.t.Fatalf("get credit note: %v", err)
	}
	return cn
}

func (f *reversalFixture) listed(id string) bool {
	f.t.Helper()
	pending, err := f.notes.ListPendingCreditNoteTaxReversal(f.ctx, 50, false)
	if err != nil {
		f.t.Fatalf("ListPendingCreditNoteTaxReversal: %v", err)
	}
	for _, cn := range pending {
		if cn.ID == id {
			return true
		}
	}
	return false
}

// Issuing a credit note records, in the issue statement itself, that its tax
// reversal is owed. So a process that stops between the issue commit and the
// reversal call leaves a note the sweep lists. Before, the marker was written
// only after a failed call, and a note issued with no call made (or whose
// marker write also failed) was found only by a second arm of the list, for
// 24 hours.
func TestIssue_MarksTheTaxReversalOwed_InTheIssueStatement(t *testing.T) {
	f := newReversalFixture(t, "CN Reversal Marker")

	t.Run("Stripe-taxed invoice with its transaction: marked and listed", func(t *testing.T) {
		cn := f.issue("CN-MARK-1", f.invoice("INV-MARK-1", "stripe_tax", 1000, "tx_upstream_1"), 5500)
		if !cn.TaxReversalPending {
			t.Fatal("an issued note on a Stripe-taxed invoice must be marked as owing its tax reversal")
		}
		if !f.listed(cn.ID) {
			t.Fatal("the marked note must be listed for the reversal sweep")
		}
	})

	t.Run("invoice's own tax transaction not recorded yet: marked, listed once it is", func(t *testing.T) {
		inv := f.invoice("INV-MARK-2", "stripe_tax", 1000, "")
		cn := f.issue("CN-MARK-2", inv, 5500)
		if !cn.TaxReversalPending {
			t.Fatal("the note must be marked although the invoice has no tax transaction yet")
		}
		if f.listed(cn.ID) {
			t.Fatal("with no transaction to reverse against, the note must wait unlisted")
		}
		if err := f.invoices.SetTaxTransaction(f.ctx, f.tenantID, inv.ID, "tx_upstream_late"); err != nil {
			t.Fatalf("stamp the late tax transaction: %v", err)
		}
		if !f.listed(cn.ID) {
			t.Fatal("once the invoice's tax transaction is recorded, the note must be listed")
		}
	})

	t.Run("invoice taxed by the manual provider: not marked", func(t *testing.T) {
		cn := f.issue("CN-MARK-3", f.invoice("INV-MARK-3", "manual", 1000, ""), 5500)
		if cn.TaxReversalPending {
			t.Fatal("a manual-tax invoice has nothing at Stripe to reverse")
		}
	})

	// Stripe Tax was never asked, so the invoice has no calculation and will
	// never have a transaction: nothing to reverse, and a marker would never
	// clear. The provider alone is not the test.
	t.Run("Stripe Tax tenant, invoice Stripe Tax never calculated (exempt, reverse charge, credit only): not marked", func(t *testing.T) {
		cn := f.issue("CN-MARK-7", f.uncalculatedInvoice("INV-MARK-7"), 5500)
		if cn.TaxReversalPending {
			t.Fatal("a note on an invoice Stripe Tax never calculated owes no reversal: the marker would never clear")
		}
	})

	// A transaction with no tax still records the sale in Stripe's tax
	// reports, and Stripe reverses it like any other.
	t.Run("Stripe-taxed invoice with zero tax: marked", func(t *testing.T) {
		cn := f.issue("CN-MARK-4", f.invoice("INV-MARK-4", "stripe_tax", 0, "tx_upstream_zero"), 5500)
		if !cn.TaxReversalPending {
			t.Fatal("a note on an invoice Stripe Tax recorded with no tax owes its reversal too")
		}
	})

	t.Run("a note with no amount: not marked", func(t *testing.T) {
		cn := f.issue("CN-MARK-5", f.invoice("INV-MARK-5", "stripe_tax", 1000, "tx_upstream_5"), 0)
		if cn.TaxReversalPending {
			t.Fatal("a note of zero credits no tax, so it owes no reversal")
		}
	})

	t.Run("a draft that is voided, not issued: not marked", func(t *testing.T) {
		inv := f.invoice("INV-MARK-6", "stripe_tax", 1000, "tx_upstream_6")
		cn, err := f.notes.Create(f.ctx, f.tenantID, domain.CreditNote{
			InvoiceID: inv.ID, CustomerID: f.custID, CreditNoteNumber: "CN-MARK-6",
			Status: domain.CreditNoteDraft, Reason: "fraudulent",
			SubtotalCents: 5500, TotalCents: 5500, Currency: "USD", RefundStatus: domain.RefundNone,
		})
		if err != nil {
			t.Fatalf("create draft: %v", err)
		}
		if won, err := f.notes.TransitionStatus(f.ctx, f.tenantID, cn.ID, domain.CreditNoteDraft, domain.CreditNoteVoided); err != nil || !won {
			t.Fatalf("void the draft: won=%v err=%v", won, err)
		}
		if f.note(cn.ID).TaxReversalPending {
			t.Fatal("only issuing marks a note: a voided draft credited nothing")
		}
	})
}

// The recovery through a service issue path, on real Postgres. Issue commits
// the note with its marker; the reversal call after the commit fails; the
// sweep, once the provider answers, stores the reversal and clears the marker.
func TestIssue_ReversalFails_TheSweepMakesIt(t *testing.T) {
	f := newReversalFixture(t, "CN Reversal Through Issue")
	inv := f.invoice("INV-ISSUE-REV", "stripe_tax", 1000, "tx_upstream_issue")
	draft, err := f.notes.Create(f.ctx, f.tenantID, domain.CreditNote{
		InvoiceID: inv.ID, CustomerID: f.custID, CreditNoteNumber: "CN-ISSUE-REV",
		Status: domain.CreditNoteDraft, Reason: "fraudulent",
		SubtotalCents: 5000, TaxAmountCents: 500, TotalCents: 5500,
		CreditAmountCents: 5500, // a credit-type note: Issue takes the in-tx grant path
		Currency:          "USD", RefundStatus: domain.RefundNone,
	})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}

	svc := creditnote.NewService(f.notes, f.invoices, nil, okGranterTx{})
	svc.SetTaxReverser(failingReverser{})
	if _, err := svc.Issue(f.ctx, f.tenantID, draft.ID); err != nil {
		t.Fatalf("Issue: %v (a failed tax reversal must not fail the issue)", err)
	}
	got := f.note(draft.ID)
	if got.Status != domain.CreditNoteIssued || !got.TaxReversalPending || got.TaxTransactionID != "" {
		t.Fatalf("after Issue with the provider down: status=%s pending=%v tx=%q, want issued, marked, no reversal", got.Status, got.TaxReversalPending, got.TaxTransactionID)
	}
	if !f.listed(draft.ID) {
		t.Fatal("the issued note must be listed for the reversal sweep")
	}

	rev := &recordingReverser{id: "tx_reversal_by_sweep"}
	svc.SetTaxReverser(rev)
	if n, sweepErrs := svc.RetryPendingCreditNoteTaxReversal(f.ctx, 50); n != 1 || len(sweepErrs) != 0 {
		t.Fatalf("sweep with the provider back: reversed=%d errs=%v, want 1 and none", n, sweepErrs)
	}
	got = f.note(draft.ID)
	if got.TaxTransactionID != "tx_reversal_by_sweep" || got.TaxReversalPending {
		t.Fatalf("after the sweep: tx=%q pending=%v, want the reversal stored and the marker cleared", got.TaxTransactionID, got.TaxReversalPending)
	}
	if len(rev.requests) != 1 || rev.requests[0].OriginalTransactionID != "tx_upstream_issue" || rev.requests[0].GrossAmountCents != 5500 {
		t.Fatalf("reversal requests = %+v, want one, against the invoice's transaction, for the note's total", rev.requests)
	}
	if f.listed(draft.ID) {
		t.Fatal("a note whose reversal is stored must leave the list")
	}
}

// An invoice Stripe Tax recorded with no tax (an exempt customer, a place
// with no registration) still has a tax transaction, which records the sale
// in Stripe's tax reports. A credit note on it owes a reversal like any
// other: the issue statement marks it, and when the inline reversal fails the
// sweep makes it. Before, the note was not marked, the sweep would have
// cleared a marker on it with no call, and a failed inline reversal was
// never made: the refunded sale stayed on the report.
func TestIssue_ZeroTaxInvoice_ReversalFails_TheSweepMakesIt(t *testing.T) {
	f := newReversalFixture(t, "CN Zero Tax Reversal")
	inv := f.invoice("INV-ZERO-REV", "stripe_tax", 0, "tx_upstream_zero")
	draft, err := f.notes.Create(f.ctx, f.tenantID, domain.CreditNote{
		InvoiceID: inv.ID, CustomerID: f.custID, CreditNoteNumber: "CN-ZERO-REV",
		Status: domain.CreditNoteDraft, Reason: "fraudulent",
		SubtotalCents: 4000, TotalCents: 4000,
		CreditAmountCents: 4000,
		Currency:          "USD", RefundStatus: domain.RefundNone,
	})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}

	svc := creditnote.NewService(f.notes, f.invoices, nil, okGranterTx{})
	svc.SetTaxReverser(failingReverser{})
	if _, err := svc.Issue(f.ctx, f.tenantID, draft.ID); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if got := f.note(draft.ID); !got.TaxReversalPending || got.TaxTransactionID != "" {
		t.Fatalf("after Issue with the provider down: pending=%v tx=%q, want marked and no reversal", got.TaxReversalPending, got.TaxTransactionID)
	}
	if !f.listed(draft.ID) {
		t.Fatal("the note must be listed for the reversal sweep")
	}

	rev := &recordingReverser{id: "tx_reversal_zero"}
	svc.SetTaxReverser(rev)
	if n, sweepErrs := svc.RetryPendingCreditNoteTaxReversal(f.ctx, 50); n != 1 || len(sweepErrs) != 0 {
		t.Fatalf("sweep: reversed=%d errs=%v, want 1 and none", n, sweepErrs)
	}
	if len(rev.requests) != 1 || rev.requests[0].OriginalTransactionID != "tx_upstream_zero" || rev.requests[0].GrossAmountCents != 4000 {
		t.Fatalf("reversal requests = %+v, want one against the invoice's transaction for the note's total", rev.requests)
	}
	if got := f.note(draft.ID); got.TaxTransactionID != "tx_reversal_zero" || got.TaxReversalPending {
		t.Fatalf("after the sweep: tx=%q pending=%v, want the reversal stored and the marker cleared", got.TaxTransactionID, got.TaxReversalPending)
	}
}

// recordingReverser answers every reversal with id and keeps the requests.
type recordingReverser struct {
	id       string
	requests []tax.ReversalRequest
}

func (r *recordingReverser) ReverseTax(_ context.Context, _ string, req tax.ReversalRequest) (*tax.ReversalResult, error) {
	r.requests = append(r.requests, req)
	return &tax.ReversalResult{TransactionID: r.id}, nil
}

// A note whose tax reversal keeps failing must still grow old for the
// refunds-needing-attention rule, which reads credit_notes.updated_at as the
// note's age (a refund pending for 72 hours is listed for the operator). A
// re-drive that wrote the note on every failure kept it young for ever, and a
// stuck refund on it was never listed.
func TestCreditNoteTaxReversal_AFailedReDriveKeepsTheNotesAge(t *testing.T) {
	f := newReversalFixture(t, "CN Reversal Keeps Age")
	cn := f.issue("CN-AGE", f.invoice("INV-AGE", "stripe_tax", 1000, "tx_upstream_age"), 5500)

	exec := func(sql string, args ...any) {
		t.Helper()
		tx, err := f.db.BeginTx(f.ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(f.ctx, sql, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	needsAttention := func() bool {
		t.Helper()
		tx, err := f.db.BeginTx(f.ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var n int
		if err := tx.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM credit_notes WHERE id = $1 AND `+domain.CreditNoteRefundNeedsAttentionSQL, cn.ID).Scan(&n); err != nil {
			t.Fatalf("needs-attention query: %v", err)
		}
		return n == 1
	}

	// Its refund has been pending for 80 hours.
	exec(`UPDATE credit_notes SET refund_status = 'pending', refund_amount_cents = 5500, updated_at = now() - interval '80 hours' WHERE id = $1`, cn.ID)
	if !needsAttention() {
		t.Fatal("setup: a refund pending for 80 hours must be listed as needing attention")
	}

	svc := creditnote.NewService(f.notes, f.invoices, nil, nil)
	svc.SetTaxReverser(failingReverser{})
	if n, sweepErrs := svc.RetryPendingCreditNoteTaxReversal(f.ctx, 50); n != 0 || len(sweepErrs) != 1 {
		t.Fatalf("sweep with the provider down: reversed=%d errs=%v, want 0 and the re-drive error", n, sweepErrs)
	}
	if !needsAttention() {
		t.Fatal("a failed tax-reversal re-drive made the note young again: its stuck refund left the needs-attention list")
	}
}

// failingReverser is the always-failing provider.
type failingReverser struct{}

func (failingReverser) ReverseTax(_ context.Context, _ string, _ tax.ReversalRequest) (*tax.ReversalResult, error) {
	return nil, errors.New("stripe: 503 service unavailable")
}

// An owed reversal stays listed until it is made: there is no age limit on the
// list. The provider is down for a month, every re-drive fails, and the note
// is still listed; the write that stores the reversal then clears the marker.
func TestCreditNoteTaxReversal_OwedReversalStaysListedUntilItIsMade(t *testing.T) {
	f := newReversalFixture(t, "CN Reversal Unbounded")
	cn := f.issue("CN-UNBOUNDED", f.invoice("INV-UNBOUNDED", "stripe_tax", 1000, "tx_upstream_unbounded"), 5500)

	svc := creditnote.NewService(f.notes, f.invoices, nil, nil)
	svc.SetTaxReverser(failingReverser{})
	if n, sweepErrs := svc.RetryPendingCreditNoteTaxReversal(f.ctx, 50); n != 0 || len(sweepErrs) != 1 {
		t.Fatalf("sweep with the provider down: reversed=%d errs=%v — want 0 and exactly the re-drive error", n, sweepErrs)
	}

	tx, err := f.db.BeginTx(f.ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(f.ctx, `UPDATE credit_notes SET updated_at = now() - interval '30 days' WHERE id = $1`, cn.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !f.listed(cn.ID) {
		t.Fatal("a reversal still owed after 30 days fell out of the list")
	}

	// A failed re-drive writes nothing to the note: it stays marked, and its
	// updated_at, which other rules read as the note's age, does not move.
	if n, sweepErrs := svc.RetryPendingCreditNoteTaxReversal(f.ctx, 50); n != 0 || len(sweepErrs) != 1 {
		t.Fatalf("second sweep with the provider down: reversed=%d errs=%v", n, sweepErrs)
	}
	if got := f.note(cn.ID); !got.TaxReversalPending || got.UpdatedAt.After(time.Now().Add(-29*24*time.Hour)) {
		t.Fatalf("after a failed re-drive: pending=%v updated_at=%v, want still marked and still 30 days old", got.TaxReversalPending, got.UpdatedAt)
	}

	if err := f.notes.SetTaxTransaction(f.ctx, f.tenantID, cn.ID, "tx_reversal_made"); err != nil {
		t.Fatalf("store the reversal: %v", err)
	}
	if got := f.note(cn.ID); got.TaxReversalPending || got.TaxTransactionID != "tx_reversal_made" {
		t.Fatalf("after the reversal is stored: pending=%v tx=%q, want the marker cleared by the same write", got.TaxReversalPending, got.TaxTransactionID)
	}
	if f.listed(cn.ID) {
		t.Fatal("a note whose reversal is stored must leave the list")
	}
}
