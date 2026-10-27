package creditnote_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
// Each test puts more notes on an invoice than the largest page the store
// list returns, and checks a sum that one page alone gets wrong.

// moreThanAPage is one note past the largest page the store list returns
// (it caps Limit at 100), so a sum read from one page of any size is short.
const moreThanAPage = 101

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

// Paid invoice of 100.00 by card. First 101 drafts of 0.02 that are voided
// before they issue, then 101 issued notes that each refund 0.01. The voided
// drafts are the oldest notes, so a page of 100 holds none of them, and only
// 100 of the 101 issued notes.
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
	// the 1.01 already refunded.
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

// raceStore holds two refunds at two points so they race the way the
// credit-note lock has to settle:
//
//   - The default-amount read (InvoiceSums, outside the lock): each refund
//     waits there until both have read, so both size their refund from the
//     same notes.
//   - The cap read (inside the lock, between listing the invoice's notes and
//     inserting the new one): the first refund to get there waits until the
//     second either gets there too or is seen waiting on this invoice's lock.
//     With the lock, the second waits on it, the first writes and commits, and
//     the second's cap read sees the first's note. Without the lock, both
//     check the cap against the same notes and both insert.
type raceStore struct {
	creditnote.Store
	db         *postgres.DB
	lockKey    string // tenant:invoice, as the store keys the lock
	read       sync.WaitGroup
	atCap      atomic.Int32
	secondCap  chan struct{}
	secondOnce sync.Once
	holdErr    atomic.Value // error
}

func (s *raceStore) InvoiceSums(ctx context.Context, tenantID, invoiceID string) (creditnote.InvoiceSums, error) {
	sums, err := s.Store.InvoiceSums(ctx, tenantID, invoiceID)
	s.read.Done()
	s.read.Wait()
	return sums, err
}

func (s *raceStore) CreateUnderInvoiceLockAudited(ctx context.Context, tenantID, invoiceID string, lines []domain.CreditNoteLineItem, build func(tx *sql.Tx, existing []domain.CreditNote) (domain.CreditNote, error), emit func(tx *sql.Tx, created domain.CreditNote) error) (domain.CreditNote, error) {
	held := func(tx *sql.Tx, existing []domain.CreditNote) (domain.CreditNote, error) {
		if s.atCap.Add(1) == 1 {
			s.holdAtCap(ctx)
		} else {
			s.secondOnce.Do(func() { close(s.secondCap) })
		}
		return build(tx, existing)
	}
	return s.Store.CreateUnderInvoiceLockAudited(ctx, tenantID, invoiceID, lines, held, emit)
}

// holdAtCap returns once the second refund reaches its cap read or waits on
// this invoice's credit-note lock (an advisory lock keyed by
// hashtextextended(tenant:invoice, 0), split by pg_locks into classid, the
// high 32 bits, and objid, the low 32).
func (s *raceStore) holdAtCap(ctx context.Context) {
	deadline := time.Now().Add(15 * time.Second)
	for {
		select {
		case <-s.secondCap:
			return
		default:
		}
		var waiting int
		if err := s.db.Pool.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_locks l, (SELECT hashtextextended($1, 0) AS k) h
			WHERE l.locktype = 'advisory' AND NOT l.granted AND l.objsubid = 1
			  AND l.classid = ((h.k >> 32) & 4294967295)::oid
			  AND l.objid = (h.k & 4294967295)::oid`, s.lockKey).Scan(&waiting); err != nil {
			s.holdErr.Store(fmt.Errorf("read pg_locks: %w", err))
			return
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			s.holdErr.Store(fmt.Errorf("the second refund neither reached its cap read nor waited on the invoice lock within 15s"))
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Two direct refunds with no amount on an invoice that already has refund
// notes past a page. Both read what is left before either writes, and the
// first holds inside the lock until the second is queued on it (or, with no
// lock, has checked the cap against the same notes). The credit-note lock lets
// one through and refuses the other, so the invoice is refunded exactly what
// was paid.
func TestCreateRefund_RacingFullRefunds_RefundOnce(t *testing.T) {
	r := newSumsRig(t, "CN Sums Refund Race")
	_, inv := seedInvoiceForCN(t, r.db, r.tenantID, "sumsrace", domain.InvoicePaid, domain.PaymentSucceeded, "pi_sums_race")
	for range moreThanAPage {
		r.note(t, inv.ID, 1, true, true)
	}

	race := &raceStore{
		Store:     creditnote.NewPostgresStore(r.db),
		db:        r.db,
		lockKey:   r.tenantID + ":" + inv.ID,
		secondCap: make(chan struct{}),
	}
	race.read.Add(2)
	svc := creditnote.NewService(race, r.invoices, &okRefunder{})
	svc.SetNumberGenerator(tenant.NewSettingsStore(r.db))

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = svc.CreateRefund(r.ctx, r.tenantID, creditnote.RefundInput{InvoiceID: inv.ID, Reason: "requested_by_customer"})
		}()
	}
	wg.Wait()
	if err, _ := race.holdErr.Load().(error); err != nil {
		t.Fatalf("hold at the cap read: %v", err)
	}

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
// note reversed. With 101 issued notes of 0.01 on 100.00, that is 98.99.
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

// A refund with no amount refunds what was paid by card less the refunds on
// its notes, but no more than the invoice can still be credited. Paid
// invoice of 100.00 by card, with one issued note of 10.00 settled outside
// Stripe: 90.00 is left to credit, and all of it can go back to the card.
func TestCreateRefund_NoAmount_StopsAtWhatIsLeftToCredit(t *testing.T) {
	r := newSumsRig(t, "CN Sums Refund After Credit")
	_, inv := seedInvoiceForCN(t, r.db, r.tenantID, "sumsoob", domain.InvoicePaid, domain.PaymentSucceeded, "pi_sums_oob")
	cn, err := r.svc.Create(r.ctx, r.tenantID, creditnote.CreateInput{
		InvoiceID:            inv.ID,
		Reason:               "adjustment",
		OutOfBandAmountCents: 1000,
		Lines:                []creditnote.CreditLineInput{{Description: "adjustment", Quantity: 1, UnitAmountCents: 1000}},
	})
	if err != nil {
		t.Fatalf("create out-of-band note: %v", err)
	}
	if _, err := r.svc.Issue(r.ctx, r.tenantID, cn.ID); err != nil {
		t.Fatalf("issue out-of-band note: %v", err)
	}

	refund, err := r.svc.CreateRefund(r.ctx, r.tenantID, creditnote.RefundInput{InvoiceID: inv.ID, Reason: "requested_by_customer"})
	if err != nil {
		t.Fatalf("CreateRefund with no amount: %v", err)
	}
	if refund.RefundAmountCents != 9000 || refund.TotalCents != 9000 {
		t.Errorf("refund note total %d refund %d, want 9000 each: 100.00 less the 10.00 already credited", refund.TotalCents, refund.RefundAmountCents)
	}

	// Nothing is left to credit, though 10.00 paid by card is not refunded.
	if _, err := r.svc.CreateRefund(r.ctx, r.tenantID, creditnote.RefundInput{InvoiceID: inv.ID, Reason: "requested_by_customer"}); err == nil || err.Error() != "invoice has already been fully credited" {
		t.Errorf("second refund with no amount: err %v, want the invoice is fully credited", err)
	}
	if sum, notes := r.nonVoidedRefunds(t, inv.ID); sum != 9000 || notes != 2 {
		t.Errorf("refunded %d over %d notes, want 9000 over 2", sum, notes)
	}
}

// A refund with no amount goes back only to the card: on a 100.00 invoice
// paid 40.00 by the customer's credit balance and 60.00 by card, it refunds
// 60.00, though 100.00 is left to credit.
func TestCreateRefund_NoAmount_StopsAtWhatWasPaidByCard(t *testing.T) {
	r := newSumsRig(t, "CN Sums Refund Card Part")
	_, inv := seedInvoiceForCN(t, r.db, r.tenantID, "sumscard", domain.InvoicePaid, domain.PaymentSucceeded, "pi_sums_card")
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(r.ctx, `UPDATE invoices SET amount_paid_cents = 6000, credits_applied_cents = 4000 WHERE id = $1`, inv.ID); err != nil {
		t.Fatalf("split the payment: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	refund, err := r.svc.CreateRefund(r.ctx, r.tenantID, creditnote.RefundInput{InvoiceID: inv.ID, Reason: "requested_by_customer"})
	if err != nil {
		t.Fatalf("CreateRefund with no amount: %v", err)
	}
	if refund.RefundAmountCents != 6000 || refund.TotalCents != 6000 {
		t.Errorf("refund note total %d refund %d, want 6000 each: the part paid by card", refund.TotalCents, refund.RefundAmountCents)
	}
}

// refusedErr is a refund Stripe answered with a clear no.
type refusedErr struct{}

func (refusedErr) Error() string        { return "card_declined" }
func (refusedErr) OutcomeUnknown() bool { return false }

// refuseFirstRefunder refuses the first refund and lets the rest through.
type refuseFirstRefunder struct {
	okRefunder
	refused atomic.Bool
}

func (r *refuseFirstRefunder) CreateRefund(ctx context.Context, pi string, amount int64, key, cnID string) (string, domain.RefundStatus, error) {
	if r.refused.CompareAndSwap(false, true) {
		return "", "", refusedErr{}
	}
	return r.okRefunder.CreateRefund(ctx, pi, amount, key, cnID)
}

// A refund note whose Stripe refund failed still holds its amount: the
// customer gets it with retry-refund on that note, so a refund with no amount
// refunds what was paid by card less it. The API docs say so; a default that
// skipped failed refunds would ask for that money twice. The invoice is paid
// 60.00 by card and 40.00 by credit balance, so the card part, not the invoice
// total, sets the amount.
func TestCreateRefund_NoAmount_CountsAFailedRefund(t *testing.T) {
	r := newSumsRig(t, "CN Sums Refund After Failed")
	r.svc = creditnote.NewService(creditnote.NewPostgresStore(r.db), r.invoices, &refuseFirstRefunder{})
	r.svc.SetNumberGenerator(tenant.NewSettingsStore(r.db))
	_, inv := seedInvoiceForCN(t, r.db, r.tenantID, "sumsfail", domain.InvoicePaid, domain.PaymentSucceeded, "pi_sums_fail")
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(r.ctx, `UPDATE invoices SET amount_paid_cents = 6000, credits_applied_cents = 4000 WHERE id = $1`, inv.ID); err != nil {
		t.Fatalf("split the payment: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	failed := r.note(t, inv.ID, 1000, true, true)
	if failed.RefundStatus != domain.RefundFailed {
		t.Fatalf("first refund status %q, want failed", failed.RefundStatus)
	}

	refund, err := r.svc.CreateRefund(r.ctx, r.tenantID, creditnote.RefundInput{InvoiceID: inv.ID, Reason: "requested_by_customer"})
	if err != nil {
		t.Fatalf("CreateRefund with no amount: %v", err)
	}
	if refund.RefundAmountCents != 5000 {
		t.Errorf("refund %d, want 5000: 60.00 paid by card less the 10.00 failed refund still on a note", refund.RefundAmountCents)
	}
}
