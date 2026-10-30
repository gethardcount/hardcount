package invoice_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
)

// reliefGranterAdapter mirrors the api-package creditGrantAdapter for the
// ADR-080 relief pair (the production adapter is package-private).
type reliefGranterAdapter struct{ svc *credit.Service }

func (a reliefGranterAdapter) GrantForCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID, cnID string, in creditnote.CreditGrantInput) error {
	_, err := a.svc.GrantForCreditNoteTx(ctx, tx, tenantID, cnID, credit.CreditNoteReturnInput{Currency: in.Currency, CustomerID: in.CustomerID, AmountCents: in.AmountCents, Description: in.Description, InvoiceID: in.InvoiceID, InvoicePaid: in.InvoicePaid, InvoiceTotalCents: in.InvoiceTotalCents})
	return err
}
func (a reliefGranterAdapter) LockCommitGrantForReliefTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string) (int64, int64, int64, bool, error) {
	return a.svc.LockCommitGrantForReliefTx(ctx, tx, tenantID, invoiceID)
}
func (a reliefGranterAdapter) RetireCommitSliceForReliefTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID, cnID string, slice, refundedGross, grossPaid int64) (int64, error) {
	res, err := a.svc.RetireCommitSliceForReliefTx(ctx, tx, tenantID, invoiceID, cnID, slice, refundedGross, grossPaid)
	if err != nil {
		return 0, err
	}
	return res.RemainingAfterCents, nil
}

// newReliefCNService wires the real creditnote service against the
// harness's stores — the production seam set (numbers from tenant
// settings, credit granter through the adapter, no Stripe refunder: the
// migration-month commits are offline-paid, so the out_of_band channel is
// the default and no external refund leg fires).
func newReliefCNService(h *commitHarness) *creditnote.Service {
	cnSvc := creditnote.NewService(creditnote.NewPostgresStore(h.db), h.invStore, nil, reliefGranterAdapter{h.creditSvc})
	cnSvc.SetNumberGenerator(tenant.NewSettingsStore(h.db))
	return cnSvc
}

// payOffline marks the commit invoice paid out-of-band (the migration norm),
// as Record offline payment does.
func payOffline(t *testing.T, h *commitHarness, ctx context.Context, invoiceID string) {
	t.Helper()
	if _, err := h.invStore.MarkPaidOffline(ctx, h.tenantID, invoiceID, time.Now().UTC(), nil); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
}

// drain consumes `cents` of the customer's credits against a seeded
// payable invoice (the real drawdown path).
func drain(t *testing.T, h *commitHarness, ctx context.Context, cents int64, round int) {
	t.Helper()
	target := h.seedPayable(t, ctx, h.custID, cents, round)
	applied, err := credit.NewPostgresStore(h.db).ApplyToInvoiceAtomic(ctx, h.tenantID, h.custID, target, "drain", cents, time.Now().UTC())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if applied != cents {
		t.Fatalf("drain applied %d, want %d", applied, cents)
	}
}

// TestCommitRelief_WorkedExample is the panel's canonical case: pay 9000,
// granted 10000, consume 4000 → the max refundable CASH is exactly 5400
// (the 4000 consumed credits were bought at 0.9 cash/credit, so they
// account for 3600 of the cash; NOT 6000 face, NOT 5000 face-minus-face).
func TestCommitRelief_WorkedExample(t *testing.T) {
	h, ctx := newCommitHarness(t, "Relief Worked Example")
	inv := h.createCommitInvoice(t, ctx, 9000, 10000, nil)
	if _, err := h.invSvc.Finalize(ctx, h.tenantID, inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	payOffline(t, h, ctx, inv.ID)
	drain(t, h, ctx, 4000, 1)

	cnSvc := newReliefCNService(h)
	cn, err := cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{
		InvoiceID: inv.ID, RetireAll: true, Reason: "customer churned",
	})
	if err != nil {
		t.Fatalf("relief: %v", err)
	}
	if cn.TotalCents != 5400 {
		t.Fatalf("relief cash: got %d, want 5400 (price-ratio, not face)", cn.TotalCents)
	}
	if cn.Status != domain.CreditNoteIssued {
		t.Fatalf("status: got %s, want issued (single-tx create-and-issue)", cn.Status)
	}
	if cn.CommitRetiredCents != 6000 {
		t.Fatalf("commit_retired_cents: got %d, want 6000", cn.CommitRetiredCents)
	}
	if cn.CreditAmountCents != 0 {
		t.Fatalf("credit channel must be zero on relief (got %d)", cn.CreditAmountCents)
	}
	// Offline-paid: allocation defaults to out_of_band, no Stripe leg.
	if cn.OutOfBandAmountCents != 5400 || cn.RefundAmountCents != 0 {
		t.Fatalf("allocation: refund=%d oob=%d, want 0/5400 for an offline-paid commit", cn.RefundAmountCents, cn.OutOfBandAmountCents)
	}

	// Grant fully retired; balance zero.
	grant, ok := h.commitGrant(t, ctx, inv.ID)
	if !ok || grant.ConsumedCents != grant.AmountCents {
		t.Fatalf("grant must be fully consumed after full relief: %+v", grant)
	}
	if bal := h.balance(t, ctx); bal != 0 {
		t.Fatalf("balance after relief: got %d, want 0", bal)
	}
	// The retire event rode the tx.
	if n := h.countEvents(t, ctx, h.custID, domain.EventCreditCommitRetired); n != 1 {
		t.Fatalf("credit.commit_retired events: got %d, want 1", n)
	}

	// Second relief: nothing refundable.
	if _, err := cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{
		InvoiceID: inv.ID, RetireAll: true,
	}); err == nil || !strings.Contains(err.Error(), "fully consumed") {
		t.Fatalf("repeat relief: err = %v, want fully-consumed 409", err)
	}
}

// TestCommitRelief_PartialsTelescope: any partial sequence sums to exactly
// f(total retired) — per-slice independent rounding would over-refund.
func TestCommitRelief_PartialsTelescope(t *testing.T) {
	h, ctx := newCommitHarness(t, "Relief Telescoping")
	inv := h.createCommitInvoice(t, ctx, 9000, 10000, nil)
	if _, err := h.invSvc.Finalize(ctx, h.tenantID, inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	payOffline(t, h, ctx, inv.ID)
	cnSvc := newReliefCNService(h)

	// r=2000 → f(2000)-f(0) = 1800.
	cn1, err := cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{
		InvoiceID: inv.ID, RetireCents: 2000,
	})
	if err != nil {
		t.Fatalf("partial 1: %v", err)
	}
	if cn1.TotalCents != 1800 {
		t.Fatalf("partial 1 cash: got %d, want 1800", cn1.TotalCents)
	}
	// Drain 3000, then relieve the remaining 5000: f(10000)-f(2000)... the
	// drained 3000 are consumed, so remaining = 10000-2000-3000 = 5000 and
	// the relief pays f(2000+5000)-f(2000) = 6300-1800 = 4500.
	drain(t, h, ctx, 3000, 2)
	cn2, err := cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{
		InvoiceID: inv.ID, RetireAll: true,
	})
	if err != nil {
		t.Fatalf("partial 2: %v", err)
	}
	if cn2.TotalCents != 4500 {
		t.Fatalf("partial 2 cash: got %d, want 4500 (f(7000)-f(2000))", cn2.TotalCents)
	}
	// Total cash out 1800+4500 = 6300 = f(7000): the 3000 drained credits
	// carried 2700 of value — identity holds: 6300 + 2700 = 9000 paid.
	if cn1.TotalCents+cn2.TotalCents != 6300 {
		t.Fatalf("telescoped sum: got %d, want 6300", cn1.TotalCents+cn2.TotalCents)
	}
}

// TestCommitRelief_Gates: state and shape gates route correctly.
func TestCommitRelief_Gates(t *testing.T) {
	h, ctx := newCommitHarness(t, "Relief Gates")
	cnSvc := newReliefCNService(h)

	// Unpaid commit invoice → route to void.
	inv := h.createCommitInvoice(t, ctx, 9000, 10000, nil)
	if _, err := h.invSvc.Finalize(ctx, h.tenantID, inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if _, err := cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{
		InvoiceID: inv.ID, RetireAll: true,
	}); err == nil || !strings.Contains(err.Error(), "void the unpaid invoice") {
		t.Fatalf("unpaid relief: err = %v, want route-to-void", err)
	}

	// Ordinary line-based CN on the PAID commit invoice → routes to relief.
	payOffline(t, h, ctx, inv.ID)
	if _, err := cnSvc.Create(ctx, h.tenantID, creditnote.CreateInput{
		InvoiceID: inv.ID, Reason: "refund",
		Lines: []creditnote.CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 100}},
	}); err == nil || !strings.Contains(err.Error(), "commit relief") {
		t.Fatalf("line CN on paid commit: err = %v, want route-to-relief", err)
	}

	// Explicit r beyond remaining → typed 409 with live numbers; no CN row.
	drain(t, h, ctx, 9500, 3)
	if _, err := cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{
		InvoiceID: inv.ID, RetireCents: 1000,
	}); err == nil || !strings.Contains(err.Error(), "only 500 remain") {
		t.Fatalf("over-retire: err = %v, want live-remaining 409", err)
	}
	cns, err := cnSvc.List(ctx, creditnote.ListFilter{TenantID: h.tenantID, InvoiceID: inv.ID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(cns) != 0 {
		t.Fatalf("failed relief must leave no CN row; got %d", len(cns))
	}

	// Non-commit invoice with a commit_relief request → 400.
	plain := h.seedPayable(t, ctx, h.custID, 1000, 4)
	if _, err := cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{
		InvoiceID: plain, RetireAll: true,
	}); err == nil || !strings.Contains(err.Error(), "not a commit invoice") {
		t.Fatalf("relief on plain invoice: err = %v, want not-a-commit 400", err)
	}
}

// TestCommitRelief_ConcurrentDrawdown: requirement (c) — a drawdown racing
// the relief serializes on the customer advisory lock + grant row lock;
// invariants hold in both interleavings (consumed never exceeds amount,
// relief cash always matches the f-slice of what was actually retired).
func TestCommitRelief_ConcurrentDrawdown(t *testing.T) {
	h, ctx := newCommitHarness(t, "Relief Race")
	inv := h.createCommitInvoice(t, ctx, 9000, 10000, nil)
	if _, err := h.invSvc.Finalize(ctx, h.tenantID, inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	payOffline(t, h, ctx, inv.ID)
	cnSvc := newReliefCNService(h)

	target := h.seedPayable(t, ctx, h.custID, 4000, 5)
	var wg sync.WaitGroup
	var reliefCN domain.CreditNote
	var reliefErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = credit.NewPostgresStore(h.db).ApplyToInvoiceAtomic(ctx, h.tenantID, h.custID, target, "race drain", 4000, time.Now().UTC())
	}()
	go func() {
		defer wg.Done()
		reliefCN, reliefErr = cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{
			InvoiceID: inv.ID, RetireAll: true,
		})
	}()
	wg.Wait()
	if reliefErr != nil {
		t.Fatalf("retire_all relief must absorb the race (shrink, not fail): %v", reliefErr)
	}

	grant, ok := h.commitGrant(t, ctx, inv.ID)
	if !ok {
		t.Fatal("grant missing")
	}
	if grant.ConsumedCents > grant.AmountCents {
		t.Fatalf("invariant violated: consumed %d > amount %d", grant.ConsumedCents, grant.AmountCents)
	}
	// Whichever order won, the relief cash must equal the f-slice of the
	// credits it retired: relief-first → f(10000)=9000; drain-first →
	// f(6000)... but K=0 either way, so total = RoundHalfToEven(9000*r,10000).
	wantCash := reliefCN.CommitRetiredCents * 9000 / 10000 // exact for these round numbers
	if reliefCN.TotalCents != wantCash {
		t.Fatalf("relief cash %d does not match retired slice %d (want %d)", reliefCN.TotalCents, reliefCN.CommitRetiredCents, wantCash)
	}
	if grant.ConsumedCents != grant.AmountCents {
		t.Fatalf("retire_all must leave the grant exhausted; consumed=%d amount=%d", grant.ConsumedCents, grant.AmountCents)
	}
}

// TestCommitRelief_ExpiredGrant: term lapse is contractual breakage — the
// swept grant has nothing refundable.
func TestCommitRelief_ExpiredGrant(t *testing.T) {
	h, ctx := newCommitHarness(t, "Relief Expired")
	exp := time.Now().UTC().Add(time.Hour)
	inv := h.createCommitInvoice(t, ctx, 9000, 10000, &exp)
	if _, err := h.invSvc.Finalize(ctx, h.tenantID, inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	payOffline(t, h, ctx, inv.ID)

	// Force-expire: backdate expires_at, then run the expiry sweep.
	tx, err := h.db.BeginTx(ctx, postgres.TxTenant, h.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE customer_credit_ledger SET expires_at = NOW() - INTERVAL '1 hour' WHERE grant_kind = 'commit' AND source_invoice_id = $1`, inv.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, errsList := h.creditSvc.ExpireCredits(ctx); len(errsList) > 0 {
		t.Fatalf("expiry sweep: %v", errsList)
	}

	cnSvc := newReliefCNService(h)
	_, err = cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{
		InvoiceID: inv.ID, RetireAll: true,
	})
	if err == nil || !errors.Is(err, errs.ErrInvalidState) && !strings.Contains(err.Error(), "fully consumed") {
		t.Fatalf("expired-grant relief: err = %v, want fully-consumed/lapsed 409", err)
	}
}
