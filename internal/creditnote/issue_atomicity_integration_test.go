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
// when taxTx is set, a recorded upstream tax transaction.
func (f *reversalFixture) invoice(num, provider string, taxCents int64, taxTx string) domain.Invoice {
	f.t.Helper()
	now := time.Now().UTC()
	inv, err := f.invoices.CreateWithLineItems(f.ctx, f.tenantID, domain.Invoice{
		CustomerID: f.custID, InvoiceNumber: num,
		Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
		Currency: "USD", SubtotalCents: 10000,
		TaxFacts:         domain.TaxFacts{TaxAmountCents: taxCents, TaxProvider: provider},
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

	t.Run("Stripe-taxed invoice with zero tax: not marked", func(t *testing.T) {
		cn := f.issue("CN-MARK-4", f.invoice("INV-MARK-4", "stripe_tax", 0, "tx_upstream_zero"), 5500)
		if cn.TaxReversalPending {
			t.Fatal("a zero-tax transaction has nothing to reverse")
		}
	})
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

	// A failed re-drive moves the note behind the others: its updated_at, the
	// list's order, is stamped. Left at 30 days ago it would head every page.
	if n, sweepErrs := svc.RetryPendingCreditNoteTaxReversal(f.ctx, 50); n != 0 || len(sweepErrs) != 1 {
		t.Fatalf("second sweep with the provider down: reversed=%d errs=%v", n, sweepErrs)
	}
	if got := f.note(cn.ID); !got.TaxReversalPending || got.UpdatedAt.Before(time.Now().Add(-time.Hour)) {
		t.Fatalf("after a failed re-drive: pending=%v updated_at=%v, want still marked and stamped now", got.TaxReversalPending, got.UpdatedAt)
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
