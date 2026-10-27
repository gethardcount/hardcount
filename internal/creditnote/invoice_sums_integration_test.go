package creditnote_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// The four sums below add up an invoice's credit notes: what is credited
// (the credit cap), what is issued (the tax a void still reverses), what was
// owed but never applied (the written-off invoice's warning), and what was
// refunded (the default amount of a direct refund). They used to page through
// the store's list, which returns 50 notes unless asked for more, so an
// invoice with more than 50 credit notes was under-counted.
//
// Each test puts more than one page of notes on an invoice and checks a sum
// that the first page alone gets wrong.

// moreThanAPage is one note past the store list's default page.
const moreThanAPage = 51

// okRefunder reports every refund as succeeded at Stripe. Safe for
// concurrent use.
type okRefunder struct{ n atomic.Int64 }

func (r *okRefunder) CreateRefund(context.Context, string, int64, string, string) (string, domain.RefundStatus, error) {
	return fmt.Sprintf("re_sums_%d", r.n.Add(1)), domain.RefundSucceeded, nil
}

func (r *okRefunder) GetRefund(context.Context, string) (domain.RefundStatus, string, error) {
	return domain.RefundSucceeded, "", nil
}

func (r *okRefunder) FindRefundForCreditNote(context.Context, string, string) (string, domain.RefundStatus, error) {
	return "", "", nil
}

type sumsRig struct {
	db       *postgres.DB
	ctx      context.Context
	tenantID string
	svc      *creditnote.Service
	invoices *invoice.PostgresStore
}

func newSumsRig(t *testing.T, name string) *sumsRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	invoices := invoice.NewPostgresStore(db)
	svc := creditnote.NewService(creditnote.NewPostgresStore(db), invoices, &okRefunder{})
	svc.SetNumberGenerator(tenant.NewSettingsStore(db))
	return &sumsRig{
		db:       db,
		ctx:      postgres.WithLivemode(context.Background(), false),
		tenantID: testutil.CreateTestTenant(t, db, name),
		svc:      svc,
		invoices: invoices,
	}
}

// note creates one credit note of amount cents on the invoice, with the
// whole amount refunded to the card when refund is set, and issues it when
// issue is set.
func (r *sumsRig) note(t *testing.T, invoiceID string, amount int64, refund, issue bool) domain.CreditNote {
	t.Helper()
	in := creditnote.CreateInput{
		InvoiceID: invoiceID,
		Reason:    "adjustment",
		Lines:     []creditnote.CreditLineInput{{Description: "adjustment", Quantity: 1, UnitAmountCents: amount}},
	}
	if refund {
		in.RefundAmountCents = amount
	}
	cn, err := r.svc.Create(r.ctx, r.tenantID, in)
	if err != nil {
		t.Fatalf("create credit note: %v", err)
	}
	if issue {
		if cn, err = r.svc.Issue(r.ctx, r.tenantID, cn.ID); err != nil {
			t.Fatalf("issue credit note: %v", err)
		}
	}
	return cn
}

// nonVoidedRefunds reads what the invoice's notes refund, from the table.
func (r *sumsRig) nonVoidedRefunds(t *testing.T, invoiceID string) (sum int64, notes int) {
	t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	if err := tx.QueryRowContext(r.ctx, `
		SELECT COALESCE(SUM(refund_amount_cents), 0), COUNT(*)
		FROM credit_notes WHERE invoice_id = $1 AND status <> 'voided'`, invoiceID).Scan(&sum, &notes); err != nil {
		t.Fatalf("read refunds: %v", err)
	}
	return sum, notes
}

// Paid invoice of 100.00 by card. First 51 drafts of 0.02 that are voided
// before they issue, then 51 issued notes that each refund 0.01. The voided
// drafts are the oldest notes, so the first page of 50 holds none of them,
// and only 50 of the 51 issued notes.
func TestInvoiceSums_CountEveryNote(t *testing.T) {
	r := newSumsRig(t, "CN Sums Every Note")
	_, inv := seedInvoiceForCN(t, r.db, r.tenantID, "sums", domain.InvoicePaid, domain.PaymentSucceeded, "pi_sums")

	for range moreThanAPage {
		cn := r.note(t, inv.ID, 2, false, false)
		if _, err := r.svc.Void(r.ctx, r.tenantID, cn.ID); err != nil {
			t.Fatalf("void draft: %v", err)
		}
	}
	for range moreThanAPage {
		r.note(t, inv.ID, 1, true, true)
	}

	credited, err := r.svc.CreditedCents(r.ctx, r.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("CreditedCents: %v", err)
	}
	if credited != moreThanAPage {
		t.Errorf("CreditedCents = %d, want %d: every note not voided counts toward the credit cap", credited, moreThanAPage)
	}
	issued, err := r.svc.IssuedCreditedCents(r.ctx, r.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("IssuedCreditedCents: %v", err)
	}
	if issued != moreThanAPage {
		t.Errorf("IssuedCreditedCents = %d, want %d: every issued note reversed its own tax", issued, moreThanAPage)
	}
	unrelieved, err := r.svc.UnreliefedClawbackCents(r.ctx, r.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("UnreliefedClawbackCents: %v", err)
	}
	if unrelieved != 2*moreThanAPage {
		t.Errorf("UnreliefedClawbackCents = %d, want %d: every note voided before it issued", unrelieved, 2*moreThanAPage)
	}

	// A direct refund with no amount refunds what is left: 100.00 paid less
	// the 0.51 already refunded.
	cn, err := r.svc.CreateRefund(r.ctx, r.tenantID, creditnote.RefundInput{InvoiceID: inv.ID, Reason: "requested_by_customer"})
	if err != nil {
		t.Fatalf("CreateRefund with no amount: %v", err)
	}
	if want := inv.AmountPaidCents - moreThanAPage; cn.RefundAmountCents != want || cn.TotalCents != want {
		t.Errorf("refund note total %d refund %d, want %d each", cn.TotalCents, cn.RefundAmountCents, want)
	}
	if sum, _ := r.nonVoidedRefunds(t, inv.ID); sum != inv.AmountPaidCents {
		t.Errorf("refunded in all %d, want exactly the %d paid", sum, inv.AmountPaidCents)
	}
}

// Two direct refunds with no amount, sent together, on an invoice that
// already has 51 refund notes. Both size their refund from the notes before
// either commits; the credit-note lock lets one through and refuses the
// other, so the invoice is refunded exactly what was paid.
func TestCreateRefund_RacingFullRefunds_RefundOnce(t *testing.T) {
	r := newSumsRig(t, "CN Sums Refund Race")
	_, inv := seedInvoiceForCN(t, r.db, r.tenantID, "sumsrace", domain.InvoicePaid, domain.PaymentSucceeded, "pi_sums_race")
	for range moreThanAPage {
		r.note(t, inv.ID, 1, true, true)
	}

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make([]error, 2)
	)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = r.svc.CreateRefund(r.ctx, r.tenantID, creditnote.RefundInput{InvoiceID: inv.ID, Reason: "requested_by_customer"})
		}()
	}
	close(start)
	wg.Wait()

	ok := 0
	for _, err := range results {
		if err == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("%d of 2 racing full refunds went through (errors %v), want exactly 1", ok, results)
	}
	sum, notes := r.nonVoidedRefunds(t, inv.ID)
	if sum != inv.AmountPaidCents || notes != moreThanAPage+1 {
		t.Errorf("refunded %d over %d notes, want exactly the %d paid over %d notes", sum, notes, inv.AmountPaidCents, moreThanAPage+1)
	}
}

// reversalRecorder records the tax reversals an invoice void asks for.
type reversalRecorder struct{ reqs []tax.ReversalRequest }

func (f *reversalRecorder) ReverseTax(_ context.Context, _ string, req tax.ReversalRequest) (*tax.ReversalResult, error) {
	f.reqs = append(f.reqs, req)
	return &tax.ReversalResult{TransactionID: "tax_rev_sums"}, nil
}

// Voiding an unpaid invoice reverses the tax of the part no issued credit
// note reversed. With 51 issued notes of 0.01 on 100.00, that is 99.49.
func TestVoid_ReversesTaxNoIssuedNoteCovered(t *testing.T) {
	r := newSumsRig(t, "CN Sums Void Tax")
	_, inv := seedInvoiceForCN(t, r.db, r.tenantID, "sumsvoid", domain.InvoiceFinalized, domain.PaymentPending, "")
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(r.ctx, `UPDATE invoices SET tax_transaction_id = 'tax_sums' WHERE id = $1`, inv.ID); err != nil {
		t.Fatalf("stamp tax transaction: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for range moreThanAPage {
		r.note(t, inv.ID, 1, false, true)
	}

	rec := &reversalRecorder{}
	invSvc := invoice.NewService(r.invoices, clock.Real(), nil)
	invSvc.SetTaxReverser(rec)
	invSvc.SetCreditNoteTotaler(r.svc)
	if _, err := invSvc.Void(r.ctx, r.tenantID, inv.ID); err != nil {
		t.Fatalf("void: %v", err)
	}
	if len(rec.reqs) != 1 {
		t.Fatalf("tax reversals = %d, want 1", len(rec.reqs))
	}
	got := rec.reqs[0]
	if want := inv.TotalAmountCents - moreThanAPage; got.Mode != tax.ReversalModePartial || got.GrossAmountCents != want {
		t.Errorf("void reversed %s %d, want partial %d: the issued notes reversed the rest", got.Mode, got.GrossAmountCents, want)
	}
}
