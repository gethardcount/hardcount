package invoice_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// reliefGranterAdapter mirrors the api-package creditGrantAdapter for the
// ADR-080 relief pair (the production adapter is package-private).
type reliefGranterAdapter struct{ svc *credit.Service }

func (a reliefGranterAdapter) GrantForCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID, cnID string, in creditnote.CreditGrantInput) error {
	_, err := a.svc.GrantForCreditNoteTx(ctx, tx, tenantID, cnID, credit.CreditNoteReturnInput{Currency: in.Currency, CustomerID: in.CustomerID, AmountCents: in.AmountCents, Description: in.Description, InvoiceID: in.InvoiceID, InvoicePaid: in.InvoicePaid, InvoiceTotalCents: in.InvoiceTotalCents})
	return err
}
func (a reliefGranterAdapter) LockTopupGrantAndBalanceTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string, at time.Time) (creditnote.TopupGrant, int64, bool, error) {
	g, spendable, found, err := a.svc.LockTopupGrantAndBalanceTx(ctx, tx, tenantID, invoiceID, at)
	return creditnote.TopupGrant(g), spendable, found, err
}
func (a reliefGranterAdapter) TopupGrantAndBalance(ctx context.Context, tenantID, invoiceID string, at time.Time) (creditnote.TopupGrant, int64, bool, error) {
	g, spendable, found, err := a.svc.TopupGrantAndBalance(ctx, tenantID, invoiceID, at)
	return creditnote.TopupGrant(g), spendable, found, err
}
func (a reliefGranterAdapter) RetireCommitSliceForReliefTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID, cnID, cnNumber string, slice, refundedGross, grossPaid int64) (int64, error) {
	res, err := a.svc.RetireCommitSliceForReliefTx(ctx, tx, tenantID, invoiceID, cnID, cnNumber, slice, refundedGross, grossPaid)
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
	return newCardReliefService(h, nil)
}

// newCardReliefService is newReliefCNService with Stripe's refund call, the
// recording of refunds made in Stripe and the read of one, for a top-up paid
// by card. No unbilled usage until a test sets some.
func newCardReliefService(h *commitHarness, refunds creditnote.Refunder) *creditnote.Service {
	cnStore := creditnote.NewPostgresStore(h.db)
	cnSvc := creditnote.NewService(cnStore, h.invStore, refunds, reliefGranterAdapter{h.creditSvc})
	cnSvc.SetNumberGenerator(tenant.NewSettingsStore(h.db))
	cnSvc.SetUnbilledUsageReader(fixedUnbilled{})
	cnSvc.SetStripeRefundRecording(cnStore, h.invStore, webhook.NewOutboxStore(h.db))
	return cnSvc
}

// fixedUnbilled is the usage billing has not invoiced yet, per currency:
// billing's figure, behind the reader creditnote declares.
type fixedUnbilled map[string]int64

func (f fixedUnbilled) UnbilledUsage(context.Context, string, string) (map[string]int64, error) {
	return f, nil
}

// stripeRefunds is Stripe's refund call: each refund succeeds, unless refuse
// is set, when the next one is refused with that Stripe error.
type stripeRefunds struct {
	n          atomic.Int64
	lastAmount atomic.Int64        // the amount of the last refund asked for
	status     domain.RefundStatus // zero: succeeded
	refuse     atomic.Pointer[string]
	// holdSearch, when set, holds the next search for a refund (Retry
	// refund's first Stripe call) until released.
	holdSearch atomic.Pointer[pause]
}

// refundRefused is a refund Stripe answered with a clear no.
type refundRefused string

func (e refundRefused) Error() string      { return string(e) }
func (refundRefused) OutcomeUnknown() bool { return false }

func (r *stripeRefunds) CreateRefund(_ context.Context, _ string, amount int64, _, _ string) (string, domain.RefundStatus, error) {
	r.lastAmount.Store(amount)
	if code := r.refuse.Swap(nil); code != nil {
		return "", "", refundRefused(*code)
	}
	if r.status != "" {
		return fmt.Sprintf("re_relief_%d", r.n.Add(1)), r.status, nil
	}
	return fmt.Sprintf("re_relief_%d", r.n.Add(1)), domain.RefundSucceeded, nil
}

func (r *stripeRefunds) GetRefund(context.Context, string) (domain.RefundStatus, string, error) {
	return domain.RefundFailed, "charge_disputed", nil
}

func (r *stripeRefunds) FindRefundForCreditNote(context.Context, string, string) (string, domain.RefundStatus, error) {
	if p := r.holdSearch.Swap(nil); p != nil {
		p.hold()
	}
	return "", "", nil
}

func (r *stripeRefunds) refuseNext(code string) { r.refuse.Store(&code) }

// payByCard pays the top-up by card, as the card settle does.
func payByCard(t *testing.T, h *commitHarness, ctx context.Context, invoiceID, pi string) {
	t.Helper()
	if _, err := h.invStore.MarkPaid(ctx, h.tenantID, invoiceID, pi, time.Now().UTC()); err != nil {
		t.Fatalf("pay by card: %v", err)
	}
}

// recordStripeRefund records a refund made in Stripe of payment pi, as its
// webhook does, first seen just after it was made.
func recordStripeRefund(t *testing.T, ctx context.Context, svc *creditnote.Service, tenantID, refundID, pi string, cents int64, status domain.StripeRefundStatus, at time.Time) {
	t.Helper()
	if _, err := svc.RecordStripeRefund(ctx, tenantID, domain.StripeRefundSnapshot{
		StripeEventID: "evt_" + refundID + "_" + string(status), EventAt: at, StripeRefundID: refundID,
		StripePaymentIntentID: pi, AmountCents: cents, Currency: "usd", Status: status, CreatedAt: at,
	}); err != nil {
		t.Fatalf("record refund %s (%s): %v", refundID, status, err)
	}
}

// reliefNotes counts the credit notes on the invoice.
func reliefNotes(t *testing.T, ctx context.Context, svc *creditnote.Service, tenantID, invoiceID string) []domain.CreditNote {
	t.Helper()
	cns, err := svc.List(ctx, creditnote.ListFilter{TenantID: tenantID, InvoiceID: invoiceID})
	if err != nil {
		t.Fatalf("list credit notes: %v", err)
	}
	return cns
}

// payOffline marks the top-up paid out-of-band, as Record offline payment
// does: the payment issues it and grants its credit.
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
	inv := h.createTopup(t, ctx, "USD", 9000, 10000)
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
	}); !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("repeat relief: err = %v, want 409: no unused credit", err)
	}
}

// TestCommitRelief_PartialsTelescope: any partial sequence sums to exactly
// f(total retired) — per-slice independent rounding would over-refund.
func TestCommitRelief_PartialsTelescope(t *testing.T) {
	h, ctx := newCommitHarness(t, "Relief Telescoping")
	inv := h.createTopup(t, ctx, "USD", 9000, 10000)
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

	// Unpaid top-up → 409, it granted nothing.
	inv := h.createTopup(t, ctx, "USD", 9000, 10000)
	if _, err := cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{
		InvoiceID: inv.ID, RetireAll: true,
	}); !errors.Is(err, errs.ErrInvalidState) || !strings.Contains(err.Error(), "withdraw it") {
		t.Fatalf("unpaid relief: err = %v, want 409 naming the withdrawal", err)
	}

	// Ordinary line-based CN on the PAID top-up → routes to relief.
	payOffline(t, h, ctx, inv.ID)
	if _, err := cnSvc.Create(ctx, h.tenantID, creditnote.CreateInput{
		InvoiceID: inv.ID, Reason: "refund",
		Lines: []creditnote.CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 100}},
	}); !errors.Is(err, errs.ErrInvalidState) || !strings.Contains(err.Error(), "commit_relief") {
		t.Fatalf("line CN on paid commit: err = %v, want 409 naming commit_relief", err)
	}

	// Explicit r beyond what is unused → 422 with the live figure; no CN row.
	drain(t, h, ctx, 9500, 3)
	if _, err := cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{
		InvoiceID: inv.ID, RetireCents: 1000,
	}); !errors.Is(err, errs.ErrValidation) || !strings.Contains(err.Error(), "Only $5.00 of the $10.00 of credit asked for is unused") {
		t.Fatalf("over-retire: err = %v, want 422 naming the $5.00 unused", err)
	}
	cns, err := cnSvc.List(ctx, creditnote.ListFilter{TenantID: h.tenantID, InvoiceID: inv.ID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(cns) != 0 {
		t.Fatalf("failed relief must leave no CN row; got %d", len(cns))
	}

	// Relief is for a top-up. An invoice that is not one is refused before
	// its lines are looked at: a plain invoice, and a commit line on an
	// invoice of another billing reason. No writer makes the second, so the
	// reason is changed on a paid top-up.
	plain := h.seedPayable(t, ctx, h.custID, 1000, 4)
	notTopup := h.createTopup(t, ctx, "USD", 9000, 10000)
	payOffline(t, h, ctx, notTopup.ID)
	execBypassStamp(t, h.db, `UPDATE invoices SET billing_reason = 'manual' WHERE id = $1`, notTopup.ID)
	for name, id := range map[string]string{"plain invoice": plain, "commit line, not a top-up": notTopup.ID} {
		_, err := cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{InvoiceID: id, RetireAll: true})
		if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "invoice_id" {
			t.Errorf("%s: err = %v, want 422 on invoice_id: relief is for a top-up", name, err)
		}
		if n := len(reliefNotes(t, ctx, cnSvc, h.tenantID, id)); n != 0 {
			t.Errorf("%s: %d credit notes, want none", name, n)
		}
	}
}

// TestCommitRelief_ConcurrentDrawdown: requirement (c) — a drawdown racing
// the relief serializes on the customer advisory lock + grant row lock;
// invariants hold in both interleavings (consumed never exceeds amount,
// relief cash always matches the f-slice of what was actually retired).
func TestCommitRelief_ConcurrentDrawdown(t *testing.T) {
	h, ctx := newCommitHarness(t, "Relief Race")
	inv := h.createTopup(t, ctx, "USD", 9000, 10000)
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

// TestCommitRelief_ExpiredGrant: credit is live at its expires_at, judged at
// the request's instant, whether or not the expiry sweep has run. At that
// instant the relief takes the unused credit; a microsecond later it takes
// none, swept or not: term lapse is contractual breakage. The expiry is set on
// the ledger an hour ahead, since a top-up's own is days away.
func TestCommitRelief_ExpiredGrant(t *testing.T) {
	for _, tc := range []struct {
		name     string
		after    time.Duration
		sweep    bool
		wantCash int64
	}{
		{"at expires_at", 0, false, 5400},
		{"a microsecond after, before the sweep", time.Microsecond, false, 0},
		{"after the sweep", time.Microsecond, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, ctx := newCommitHarness(t, "Relief Expired")
			inv := h.createTopup(t, ctx, "USD", 9000, 10000)
			payOffline(t, h, ctx, inv.ID)
			drain(t, h, ctx, 4000, 1)
			expires := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
			execBypassStamp(t, h.db, `UPDATE customer_credit_ledger SET expires_at = $2 WHERE grant_kind = 'commit' AND source_invoice_id = $1`, inv.ID, expires)
			if tc.sweep {
				execBypassStamp(t, h.db, `UPDATE customer_credit_ledger SET expires_at = NOW() - INTERVAL '1 hour' WHERE grant_kind = 'commit' AND source_invoice_id = $1`, inv.ID)
				if _, errsList := h.creditSvc.ExpireCredits(ctx); len(errsList) > 0 {
					t.Fatalf("expiry sweep: %v", errsList)
				}
			}

			cnSvc := newReliefCNService(h)
			at := clock.WithEffectiveNow(ctx, expires.Add(tc.after))
			cn, err := cnSvc.CreateAndIssueCommitRelief(at, h.tenantID, creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireAll: true})
			if tc.wantCash > 0 {
				if err != nil || cn.TotalCents != tc.wantCash || cn.CommitRetiredCents != 6000 {
					t.Fatalf("relief: cash %d for %d credit, err %v; want %d for 6000", cn.TotalCents, cn.CommitRetiredCents, err, tc.wantCash)
				}
				return
			}
			if !errors.Is(err, errs.ErrInvalidState) {
				t.Fatalf("relief of expired credit: cash %d for %d credit, err %v; want 409", cn.TotalCents, cn.CommitRetiredCents, err)
			}
			if n := len(reliefNotes(t, ctx, cnSvc, h.tenantID, inv.ID)); n != 0 {
				t.Errorf("%d credit notes after a refused relief, want none", n)
			}
		})
	}
}

// TestCommitRelief_UnusedAgainstLiveBalance: usage not yet invoiced is credit
// already used, so unused credit is the smaller of what is left of the grant
// and the spendable balance less that usage.
func TestCommitRelief_UnusedAgainstLiveBalance(t *testing.T) {
	h, ctx := newCommitHarness(t, "Relief Unused Credit")
	inv := h.createTopup(t, ctx, "USD", 9000, 10000)
	payOffline(t, h, ctx, inv.ID)
	cnSvc := newReliefCNService(h)
	cnSvc.SetUnbilledUsageReader(fixedUnbilled{"USD": 7000})

	_, err := cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireCents: 5000})
	if !errors.Is(err, errs.ErrValidation) || !strings.Contains(err.Error(), "30.00") {
		t.Fatalf("retire 50.00 with 30.00 unused: err = %v, want 422 naming the 30.00 unused", err)
	}
	cn, err := cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireAll: true})
	if err != nil || cn.CommitRetiredCents != 3000 || cn.TotalCents != 2700 {
		t.Fatalf("retire_all: %d credit for %d cash, err %v; want 3000 for 2700", cn.CommitRetiredCents, cn.TotalCents, err)
	}

	cnSvc.SetUnbilledUsageReader(fixedUnbilled{"USD": 8000})
	if _, err := cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireAll: true}); !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("unbilled usage above the balance: err = %v, want 409", err)
	}
}

// TestCommitRelief_PendingDrawSaysTheWayOut: while an invoice of the customer
// is still to draw on the credit, its usage is no longer unbilled but the
// credit is still in the balance, so relief refuses with the one way out the
// invoice's state leaves. A period close commits its invoice queued for
// collection and draws the credit after, so queued is the common row. Each row
// is its own customer's 90.00 top-up of 100.00 credit.
func TestCommitRelief_PendingDrawSaysTheWayOut(t *testing.T) {
	h, ctx := newCommitHarness(t, "Relief Pending Draw")
	cnSvc := newReliefCNService(h)
	// cycleInvoice is a June 2026 period invoice of a new customer.
	cycleInvoice := func(t *testing.T, num string, status domain.InvoiceStatus) string {
		cust, err := customer.NewPostgresStore(h.db).Create(ctx, h.tenantID, domain.Customer{ExternalID: "cus_" + num, DisplayName: num})
		if err != nil {
			t.Fatalf("create customer: %v", err)
		}
		inv := domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num, BillingReason: domain.BillingReasonSubscriptionCycle,
			Status: status, PaymentStatus: domain.PaymentPending,
			Currency: "USD", SubtotalCents: 1000, TotalAmountCents: 1000, AmountDueCents: 1000,
			BillingPeriodStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), BillingPeriodEnd: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		}
		if status == domain.InvoiceFinalized {
			now := time.Now().UTC()
			inv.IssuedAt = &now
		}
		if _, err := h.invStore.CreateWithLineItems(ctx, h.tenantID, inv, nil); err != nil {
			t.Fatalf("create invoice: %v", err)
		}
		return cust.ID
	}

	for _, tc := range []struct {
		name    string
		pending func(t *testing.T) (customerID string)
		want    string
	}{
		{"draft", func(t *testing.T) string { return cycleInvoice(t, "HC-DRAFT-1", domain.InvoiceDraft) },
			"Draft invoice HC-DRAFT-1 for Jun 1, 2026 – Jun 30, 2026 is waiting to use this credit; finalize or void it, then try again."},
		{"queued", func(t *testing.T) string { return cycleInvoice(t, "HC-QUEUED-1", domain.InvoiceFinalized) },
			"Invoice HC-QUEUED-1 draws this credit at the next collection pass; try again once that pass has run."},
		{"paused", func(t *testing.T) string {
			inv, subID := seedQueuedSubInvoice(t, h.db, ctx, h.tenantID, "HC-PAUSED-1")
			if _, err := subscription.NewPostgresStore(h.db).SetPauseCollection(ctx, h.tenantID, subID,
				domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft}, nil); err != nil {
				t.Fatalf("pause collection: %v", err)
			}
			return inv.CustomerID
		}, "Invoice HC-PAUSED-1 is waiting to use this credit, but its collection is paused; resume collection or void the invoice, then try again."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := *h
			row.custID = tc.pending(t)
			topup := row.createTopup(t, ctx, "USD", 9000, 10000)
			payOffline(t, &row, ctx, topup.ID)
			_, err := cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{InvoiceID: topup.ID, RetireAll: true})
			if !errors.Is(err, errs.ErrInvalidState) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("relief while a %s invoice waits to draw the credit: err = %v\nwant 409: %s", tc.name, err, tc.want)
			}
		})
	}
}

// TestCardRefundable_OneFigure: what can still go back to the card that paid
// an invoice is one figure for an ordinary credit note's create, the
// recorder of refunds made in Stripe and relief (the doctor check reads the
// same SQL): what the card paid, less the card refunds of notes not voided,
// failed ones too, and the refunds made in Stripe of that payment that no
// note holds and that moved or may move money. It is nothing once the bank
// won a dispute on the payment. Each row is a 90.00 payment by card, and
// left is what the row leaves on the card.
func TestCardRefundable_OneFigure(t *testing.T) {
	r := newOutsideRig(t, "One Card Figure")
	late := func(t *testing.T, invoiceID, pi string, cents int64) domain.InvoiceStripeRefund {
		r.n++
		at, re := r.next(), fmt.Sprintf("re_late_%d", r.n)
		if _, err := r.svc.RecordStripeRefund(r.ctx, r.h.tenantID, domain.StripeRefundSnapshot{
			StripeEventID: "evt_" + re, EventAt: at, StripeRefundID: re, StripePaymentIntentID: pi,
			AmountCents: cents, Currency: "usd", Status: domain.StripeRefundSucceeded, CreatedAt: at.Add(-48 * time.Hour),
		}); err != nil {
			t.Fatalf("record refund: %v", err)
		}
		return r.refundRow(t, invoiceID, re)
	}
	states := []struct {
		name string
		left int64
		// set brings the invoice paid by pi to the row's state. topup says
		// whether the invoice is a top-up.
		set func(t *testing.T, inv domain.Invoice, pi string, topup bool)
	}{
		{"a refund made in Stripe flagged for review", 4000, func(t *testing.T, inv domain.Invoice, pi string, _ bool) {
			late(t, inv.ID, pi, 5000)
		}},
		{"a refund made in Stripe still in flight", 4000, func(t *testing.T, inv domain.Invoice, pi string, _ bool) {
			r.refund(t, inv.ID, pi, 5000, domain.StripeRefundPending)
		}},
		{"a refund made in Stripe that needs action", 4000, func(t *testing.T, inv domain.Invoice, pi string, _ bool) {
			r.refund(t, inv.ID, pi, 5000, domain.StripeRefundRequiresAction)
		}},
		{"a refund made in Stripe the operator dismissed", 4000, func(t *testing.T, inv domain.Invoice, pi string, _ bool) {
			row := late(t, inv.ID, pi, 5000)
			if _, err := r.svc.DismissStripeRefund(r.ctx, r.h.tenantID, inv.ID, row.StripeRefundID, "handled by hand"); err != nil {
				t.Fatalf("dismiss: %v", err)
			}
		}},
		{"a refund made in Stripe that failed", 9000, func(t *testing.T, inv domain.Invoice, pi string, _ bool) {
			r.refund(t, inv.ID, pi, 5000, domain.StripeRefundFailed)
		}},
		{"a refund made in Stripe of another payment", 9000, func(t *testing.T, inv domain.Invoice, _ string, _ bool) {
			other := "pi_other_" + inv.ID
			if err := r.h.invStore.RecordChargeAttempt(r.ctx, r.h.tenantID, domain.InvoiceChargeAttempt{
				InvoiceID: inv.ID, StripePaymentIntentID: other, Trigger: domain.ChargeTriggerExternal,
				Outcome: domain.ChargeAttemptSucceeded, AmountCents: 9000, OccurredAt: r.at,
			}); err != nil {
				t.Fatalf("second charge: %v", err)
			}
			r.refund(t, inv.ID, other, 5000, domain.StripeRefundSucceeded)
		}},
		{"a refund made in Stripe recorded as a note", 4000, func(t *testing.T, inv domain.Invoice, pi string, _ bool) {
			if row := r.refund(t, inv.ID, pi, 5000, domain.StripeRefundSucceeded); row.Outcome != domain.StripeRefundRecordedAsCreditNote {
				t.Fatalf("refund: %s (%s), want recorded", row.Outcome, row.ReviewReason)
			}
		}},
		{"a note whose card refund failed", 3600, func(t *testing.T, inv domain.Invoice, _ string, topup bool) {
			r.cards.refuseNext("charge_disputed")
			var cn domain.CreditNote
			var err error
			if topup {
				cn, err = r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireCents: 6000})
			} else {
				cn, err = r.svc.Create(r.ctx, r.h.tenantID, creditnote.CreateInput{InvoiceID: inv.ID, Reason: "r", RefundAmountCents: 5400, AutoIssue: true,
					Lines: []creditnote.CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 5400}}})
				if err == nil {
					cn, err = r.svc.Issue(r.ctx, r.h.tenantID, cn.ID)
				}
			}
			if err != nil || cn.RefundStatus != domain.RefundFailed {
				t.Fatalf("note: refund status %s, err %v; want failed", cn.RefundStatus, err)
			}
		}},
		{"a dispute the bank won", 0, func(t *testing.T, inv domain.Invoice, pi string, _ bool) {
			r.dispute(t, "du_fig_"+inv.ID, pi, domain.DisputeLost, 3000, "usd")
		}},
	}

	ordinary := func(t *testing.T) (domain.Invoice, string) {
		r.n++
		id := r.h.seedPayable(t, r.ctx, r.h.custID, 9000, 700+r.n)
		pi := fmt.Sprintf("pi_fig_%d", r.n)
		payByCard(t, r.h, r.ctx, id, pi)
		inv, err := r.h.invStore.Get(r.ctx, r.h.tenantID, id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return inv, pi
	}
	for _, st := range states {
		t.Run("create: "+st.name, func(t *testing.T) {
			inv, pi := ordinary(t)
			st.set(t, inv, pi, false)
			creditable := inv.TotalAmountCents
			for _, cn := range reliefNotes(t, r.ctx, r.svc, r.h.tenantID, inv.ID) {
				creditable -= cn.TotalCents
			}
			create := func(cents int64) error {
				_, err := r.svc.Create(r.ctx, r.h.tenantID, creditnote.CreateInput{InvoiceID: inv.ID, Reason: "r", RefundAmountCents: cents,
					Lines: []creditnote.CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: cents}}})
				return err
			}
			if st.left+1 <= creditable {
				if err := create(st.left + 1); !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "refund_amount_cents" {
					t.Errorf("a card refund of %d with %d left: %v, want 422 on refund_amount_cents", st.left+1, st.left, err)
				}
			}
			if st.left > 0 {
				if err := create(st.left); err != nil {
					t.Errorf("a card refund of the %d left: %v", st.left, err)
				}
			}
		})
		t.Run("recorder: "+st.name, func(t *testing.T) {
			inv, pi := ordinary(t)
			st.set(t, inv, pi, false)
			want := domain.StripeRefundReviewExceedsCardRefundable
			if st.name == "a note whose card refund failed" {
				want = domain.StripeRefundReviewCreditNoteRefundFailed // rule 7 comes first
			}
			if row := r.refund(t, inv.ID, pi, st.left+1, domain.StripeRefundSucceeded); row.ReviewReason != want {
				t.Errorf("a refund of %d with %d left: %s (%s), want needs_review (%s)", st.left+1, st.left, row.Outcome, row.ReviewReason, want)
			}
		})
	}

	// Relief refunds the smaller of the card's figure and what the payment's
	// documents allow, which a refund made in Stripe that no note holds uses
	// up too. It waits while such a refund is not settled, since it may be
	// this money (ADR-134, no path refunds twice).
	for _, st := range states {
		t.Run("relief: "+st.name, func(t *testing.T) {
			inv, pi := r.topup(t, 9000, 10000, 0)
			st.set(t, inv, pi, true)
			cn, err := r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireAll: true})
			switch {
			case strings.Contains(st.name, "flagged") || strings.Contains(st.name, "in flight") || strings.Contains(st.name, "needs action"):
				if errs.Code(err) != "stripe_refund_unresolved" {
					t.Errorf("relief: %v, want 409 stripe_refund_unresolved", err)
				}
			case st.left == 0:
				if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "refund_amount_cents" || !strings.Contains(err.Error(), "dispute") {
					t.Errorf("relief to the card: %v, want 422 on refund_amount_cents naming the dispute", err)
				}
			default:
				if err != nil || cn.RefundAmountCents != st.left {
					t.Errorf("retire_all: card refund %d, err %v; want the %d left", cn.RefundAmountCents, err, st.left)
				}
			}
		})
	}

	// A refund made in Stripe that a note holds counts once, as the note's
	// card refund: a relief whose card refund failed, then the operator
	// attached the refund they made in Stripe to it.
	t.Run("relief: a refund made in Stripe attached to a failed note", func(t *testing.T) {
		inv, pi := r.topup(t, 9000, 10000, 0)
		r.cards.refuseNext("charge_disputed")
		first, err := r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireCents: 2000})
		if err != nil || first.RefundStatus != domain.RefundFailed {
			t.Fatalf("first relief: refund status %s, err %v; want failed", first.RefundStatus, err)
		}
		row := r.refund(t, inv.ID, pi, 1800, domain.StripeRefundSucceeded)
		r.svc.SetRefundReader(fixedRefund{domain.StripeRefundFacts{ID: row.StripeRefundID, PaymentIntentID: pi, AmountCents: 1800, Currency: "usd", Status: domain.StripeRefundSucceeded}})
		if _, err := r.svc.AttachStripeRefund(r.ctx, r.h.tenantID, first.ID, row.StripeRefundID); err != nil {
			t.Fatalf("attach: %v", err)
		}
		cn, err := r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireAll: true})
		if err != nil || cn.RefundAmountCents != 7200 {
			t.Errorf("second relief: card refund %d, err %v; want 7200", cn.RefundAmountCents, err)
		}
	})
}

// fixedRefund is Stripe's read of one refund.
type fixedRefund struct{ facts domain.StripeRefundFacts }

func (r fixedRefund) ReadRefund(context.Context, string) (domain.StripeRefundFacts, error) {
	return r.facts, nil
}

// TestCommitRelief_DisputeGuard: relief is refused while the customer's bank
// holds a chargeback on the payment, appeals included. Once the bank won it,
// nothing more can go back to the card (the document cap's rows are in
// TestCommitRelief_BoundedByTheDocumentCap). An inquiry is not a chargeback:
// its relief may refund the card, which ends the inquiry. A dispute that
// closed for the merchant leaves the money paid.
func TestCommitRelief_DisputeGuard(t *testing.T) {
	h, ctx := newCommitHarness(t, "Relief Dispute Guard")
	cnSvc := newCardReliefService(h, &stripeRefunds{})
	at := time.Now().UTC().Truncate(time.Second)

	for i, tc := range []struct {
		status  domain.DisputeStatus
		allowed bool
	}{
		{domain.DisputeNeedsResponse, false},
		{domain.DisputeUnderReview, false},
		{domain.DisputeAppealNeedsResponse, false},
		{domain.DisputeAppealUnderReview, false},
		{domain.DisputeLost, false},
		{domain.DisputeWarningNeedsResponse, true},
		{domain.DisputeWarningUnderReview, true},
		{domain.DisputeWarningClosed, true},
		{domain.DisputeWon, true},
		{domain.DisputePrevented, true},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			inv := h.createTopup(t, ctx, "USD", 9000, 10000)
			pi := fmt.Sprintf("pi_guard_%d", i)
			payByCard(t, h, ctx, inv.ID, pi)
			if _, err := h.invStore.RecordStripeDispute(ctx, h.tenantID, domain.DisputeSnapshot{
				StripeEventID: fmt.Sprintf("evt_guard_%d", i), EventAt: at, StripeDisputeID: fmt.Sprintf("du_guard_%d", i),
				StripePaymentIntentID: pi, Status: tc.status, AmountCents: 9000, Currency: "usd", OpenedAt: at,
			}); err != nil {
				t.Fatalf("record dispute: %v", err)
			}

			cn, err := cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireCents: 6000})
			if tc.allowed {
				if err != nil || cn.RefundAmountCents != 5400 {
					t.Fatalf("relief: card refund %d, err %v; want 5400 to the card", cn.RefundAmountCents, err)
				}
				return
			}
			switch {
			case tc.status == domain.DisputeLost:
				if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "refund_amount_cents" {
					t.Fatalf("relief to the card after a loss: card refund %d, err %v; want 422 on refund_amount_cents", cn.RefundAmountCents, err)
				}
			case !errors.Is(err, errs.ErrInvalidState) || errs.Code(err) != "payment_disputed":
				t.Fatalf("relief with a %s dispute: card refund %d, err %v; want 409 payment_disputed", tc.status, cn.RefundAmountCents, err)
			}
			if n := len(reliefNotes(t, ctx, cnSvc, h.tenantID, inv.ID)); n != 0 {
				t.Errorf("%d credit notes after a refused relief, want none", n)
			}
		})
	}

	// A top-up charged twice (its link and a collect) keeps the first
	// payment. A lost dispute on the second does not touch the first, so the
	// first payment's credit is still refunded.
	t.Run("a dispute on another payment of the invoice", func(t *testing.T) {
		inv := h.createTopup(t, ctx, "USD", 9000, 10000)
		const pi, other = "pi_guard_kept", "pi_guard_second_charge"
		payByCard(t, h, ctx, inv.ID, pi)
		if err := h.invStore.RecordChargeAttempt(ctx, h.tenantID, domain.InvoiceChargeAttempt{
			InvoiceID: inv.ID, StripePaymentIntentID: other, Trigger: domain.ChargeTriggerExternal,
			Outcome: domain.ChargeAttemptSucceeded, AmountCents: 9000, OccurredAt: at,
		}); err != nil {
			t.Fatalf("second charge: %v", err)
		}
		if _, err := h.invStore.RecordStripeDispute(ctx, h.tenantID, domain.DisputeSnapshot{
			StripeEventID: "evt_guard_other", EventAt: at, StripeDisputeID: "du_guard_other",
			StripePaymentIntentID: other, Status: domain.DisputeLost, AmountCents: 9000, Currency: "usd", OpenedAt: at,
		}); err != nil {
			t.Fatalf("record dispute: %v", err)
		}
		cn, err := cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireCents: 6000})
		if err != nil || cn.RefundAmountCents != 5400 {
			t.Fatalf("relief: card refund %d, err %v; want 5400 to the card", cn.RefundAmountCents, err)
		}
	})
}

// auditRows records the audit rows written on a transaction.
type auditRows struct {
	mu   sync.Mutex
	rows []audit.Entry
}

func (a *auditRows) LogInTx(_ context.Context, _ *sql.Tx, e audit.Entry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, e)
	return nil
}

func (a *auditRows) action(resourceID, action string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.rows {
		if e.ResourceID == resourceID && e.Metadata["action"] == action {
			return true
		}
	}
	return false
}

// TestCommitRelief_FailedCardRefundReachesAnEnd: a card refund Stripe refused
// (charge_disputed, when a chargeback opened during the relief) is a dead end
// unless the operator says the money was returned another way. That moves
// the refund to out_of_band, audited, and the refund is no longer failed. It
// is refused while the refund is pending, succeeded or never ran. An ordinary
// credit note has the same dead end and the same way out, and Retry refund
// names it where the refund can never succeed.
func TestCommitRelief_FailedCardRefundReachesAnEnd(t *testing.T) {
	h, ctx := newCommitHarness(t, "Relief Refund Dead End")
	refunds := &stripeRefunds{}
	cnSvc := newCardReliefService(h, refunds)
	rows := &auditRows{}
	cnSvc.SetAuditLogger(rows)

	relief := func(t *testing.T, pi string) domain.CreditNote {
		t.Helper()
		inv := h.createTopup(t, ctx, "USD", 9000, 10000)
		if pi == "" {
			payOffline(t, h, ctx, inv.ID)
		} else {
			payByCard(t, h, ctx, inv.ID, pi)
		}
		cn, err := cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireCents: 6000})
		if err != nil {
			t.Fatalf("relief: %v", err)
		}
		return cn
	}
	ordinary := func(t *testing.T, pi string) domain.CreditNote {
		t.Helper()
		invID := h.seedPayable(t, ctx, h.custID, 5000, int(refunds.n.Load())+100)
		if _, err := h.invStore.MarkPaid(ctx, h.tenantID, invID, pi, time.Now().UTC()); err != nil {
			t.Fatalf("pay: %v", err)
		}
		cn, err := cnSvc.Create(ctx, h.tenantID, creditnote.CreateInput{
			InvoiceID: invID, Reason: "requested_by_customer", RefundAmountCents: 5000,
			Lines: []creditnote.CreditLineInput{{Description: "refund", Quantity: 1, UnitAmountCents: 5000}},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if cn, err = cnSvc.Issue(ctx, h.tenantID, cn.ID); err != nil {
			t.Fatalf("issue: %v", err)
		}
		return cn
	}

	for _, kind := range []struct {
		name string
		note func(*testing.T, string) domain.CreditNote
	}{{"relief", relief}, {"ordinary note", ordinary}} {
		t.Run(kind.name, func(t *testing.T) {
			refunds.refuseNext("charge_disputed")
			cn := kind.note(t, "pi_end_failed_"+kind.name)
			if cn.RefundStatus != domain.RefundFailed || cn.RefundAmountCents == 0 {
				t.Fatalf("refund status %s of %d, want failed", cn.RefundStatus, cn.RefundAmountCents)
			}
			total := cn.RefundAmountCents + cn.OutOfBandAmountCents
			got, err := cnSvc.MarkRefundReturnedAnotherWay(ctx, h.tenantID, cn.ID)
			if err != nil {
				t.Fatalf("returned another way: %v", err)
			}
			if got.RefundAmountCents != 0 || got.OutOfBandAmountCents != total || got.RefundStatus == domain.RefundFailed {
				t.Errorf("after: refund %d (%s), out_of_band %d; want 0, not failed, out_of_band %d",
					got.RefundAmountCents, got.RefundStatus, got.OutOfBandAmountCents, total)
			}
			if !rows.action(cn.ID, "refund_returned_another_way") {
				t.Error("no audit row for returned another way")
			}
			const already = "This card refund is already recorded as returned another way."
			for name, call := range map[string]func() error{
				"a second mark": func() error { _, err := cnSvc.MarkRefundReturnedAnotherWay(ctx, h.tenantID, cn.ID); return err },
				"retry refund":  func() error { _, err := cnSvc.RetryRefund(ctx, h.tenantID, cn.ID); return err },
			} {
				var de *errs.DomainError
				if err := call(); !errors.As(err, &de) || !errors.Is(err, errs.ErrInvalidState) || de.Message != already {
					t.Errorf("%s after the mark: err = %v, want 409 %q", name, err, already)
				}
			}
		})
	}

	for _, tc := range []struct {
		name   string
		status domain.RefundStatus
		note   func(*testing.T) domain.CreditNote
	}{
		{"pending", domain.RefundPending, func(t *testing.T) domain.CreditNote {
			refunds.status = domain.RefundPending
			defer func() { refunds.status = "" }()
			return relief(t, "pi_end_pending")
		}},
		{"succeeded", domain.RefundSucceeded, func(t *testing.T) domain.CreditNote { return relief(t, "pi_end_ok") }},
		{"never ran: paid offline", domain.RefundNone, func(t *testing.T) domain.CreditNote { return relief(t, "") }},
	} {
		t.Run("refused while "+tc.name, func(t *testing.T) {
			cn := tc.note(t)
			if cn.RefundStatus != tc.status {
				t.Fatalf("refund status %s, want %s", cn.RefundStatus, tc.status)
			}
			if _, err := cnSvc.MarkRefundReturnedAnotherWay(ctx, h.tenantID, cn.ID); !errors.Is(err, errs.ErrInvalidState) {
				t.Errorf("err = %v, want 409", err)
			}
			if after, _ := cnSvc.Get(ctx, h.tenantID, cn.ID); after.RefundAmountCents != cn.RefundAmountCents || after.OutOfBandAmountCents != cn.OutOfBandAmountCents {
				t.Errorf("split changed: %d/%d, was %d/%d", after.RefundAmountCents, after.OutOfBandAmountCents, cn.RefundAmountCents, cn.OutOfBandAmountCents)
			}
		})
	}

	// Retry refund claims the attempt before it calls Stripe, so a mark that
	// arrives while the retry is running is refused: otherwise the retry's
	// refund could succeed on a note already moved to out_of_band.
	t.Run("refused while a retry is running", func(t *testing.T) {
		refunds.refuseNext("charge_disputed")
		cn := relief(t, "pi_end_retry")
		hold := newPause()
		refunds.holdSearch.Store(hold)
		retry := start(ctx, func(c context.Context) error { _, err := cnSvc.RetryRefund(c, h.tenantID, cn.ID); return err })
		<-hold.reached
		_, markErr := cnSvc.MarkRefundReturnedAnotherWay(ctx, h.tenantID, cn.ID)
		close(hold.release)
		retry.wait(t, "retry refund")
		if !errors.Is(markErr, errs.ErrInvalidState) {
			t.Errorf("mark during the retry: err = %v, want 409", markErr)
		}
		after, _ := cnSvc.Get(ctx, h.tenantID, cn.ID)
		if after.RefundStatus != domain.RefundSucceeded || after.RefundAmountCents != cn.RefundAmountCents || after.OutOfBandAmountCents != cn.OutOfBandAmountCents {
			t.Errorf("after: refund %d (%s), out_of_band %d; want the retried card refund %d succeeded and out_of_band %d",
				after.RefundAmountCents, after.RefundStatus, after.OutOfBandAmountCents, cn.RefundAmountCents, cn.OutOfBandAmountCents)
		}
	})

	// While the bank holds a chargeback on the payment, the operator waits
	// for its outcome. After a loss the bank returned the money, so the
	// refund is marked without paying; a win or an inquiry does not block.
	for i, tc := range []struct {
		status  domain.DisputeStatus
		allowed bool
	}{
		{domain.DisputeNeedsResponse, false},
		{domain.DisputeLost, true},
		{domain.DisputeWon, true},
		{domain.DisputeWarningNeedsResponse, true},
	} {
		t.Run("dispute "+string(tc.status), func(t *testing.T) {
			pi := fmt.Sprintf("pi_end_dispute_%d", i)
			refunds.refuseNext("charge_disputed")
			cn := relief(t, pi)
			at := time.Now().UTC().Truncate(time.Second)
			if _, err := h.invStore.RecordStripeDispute(ctx, h.tenantID, domain.DisputeSnapshot{
				StripeEventID: fmt.Sprintf("evt_end_dispute_%d", i), EventAt: at, StripeDisputeID: fmt.Sprintf("du_end_%d", i),
				StripePaymentIntentID: pi, Status: tc.status, AmountCents: 9000, Currency: "usd", OpenedAt: at,
			}); err != nil {
				t.Fatalf("record dispute: %v", err)
			}
			_, err := cnSvc.MarkRefundReturnedAnotherWay(ctx, h.tenantID, cn.ID)
			if tc.allowed && err != nil {
				t.Errorf("err = %v, want the mark allowed", err)
			}
			if !tc.allowed && (!errors.Is(err, errs.ErrInvalidState) || errs.Code(err) != "payment_disputed") {
				t.Errorf("err = %v, want 409 payment_disputed", err)
			}
		})
	}

	// A refund the operator made in Stripe and Hardcount has not recorded may
	// be this refund's money. Marking the note now would say the money went
	// back another way and leave that refund no failed refund to attach to.
	t.Run("refused while a refund made in Stripe is not recorded", func(t *testing.T) {
		refunds.refuseNext("charge_disputed")
		cn := relief(t, "pi_end_unrecorded")
		recordStripeRefund(t, ctx, cnSvc, h.tenantID, "re_end_unrecorded", "pi_end_unrecorded", cn.RefundAmountCents,
			domain.StripeRefundSucceeded, time.Now().UTC().Truncate(time.Second))
		if _, err := cnSvc.MarkRefundReturnedAnotherWay(ctx, h.tenantID, cn.ID); !errors.Is(err, errs.ErrInvalidState) || errs.Code(err) != "stripe_refund_unresolved" {
			t.Errorf("err = %v, want 409 stripe_refund_unresolved", err)
		}
		if after, _ := cnSvc.Get(ctx, h.tenantID, cn.ID); after.RefundStatus != domain.RefundFailed || after.RefundAmountCents != cn.RefundAmountCents {
			t.Errorf("refund %d (%s), want it still failed at %d", after.RefundAmountCents, after.RefundStatus, cn.RefundAmountCents)
		}
	})

	// A refund Stripe failed after taking it has an id, and Stripe may still
	// deliver an older event for it. Once returned another way, the note
	// has no card refund for that event to move.
	t.Run("a late event for the failed refund changes nothing", func(t *testing.T) {
		refunds.status = domain.RefundPending
		cn := relief(t, "pi_end_late_event")
		refunds.status = ""
		if err := cnSvc.ApplyRefundWebhook(ctx, h.tenantID, cn.StripeRefundID, domain.RefundFailed, "expired_or_canceled_card"); err != nil {
			t.Fatalf("refund failed: %v", err)
		}
		if _, err := cnSvc.MarkRefundReturnedAnotherWay(ctx, h.tenantID, cn.ID); err != nil {
			t.Fatalf("returned another way: %v", err)
		}
		if err := cnSvc.ApplyRefundWebhook(ctx, h.tenantID, cn.StripeRefundID, domain.RefundPending, ""); err != nil {
			t.Fatalf("late event: %v", err)
		}
		if got, _ := cnSvc.Get(ctx, h.tenantID, cn.ID); got.RefundStatus != domain.RefundNone || got.RefundAmountCents != 0 {
			t.Errorf("after a late pending event: refund %d (%s), want 0 (none)", got.RefundAmountCents, got.RefundStatus)
		}
	})

	// A refund on an invoice paid outside Stripe can never succeed. Such a
	// note predates the card cap, so the row is written directly.
	t.Run("retry refund names the way out", func(t *testing.T) {
		cn := relief(t, "")
		execBypassStamp(t, h.db, `UPDATE credit_notes SET refund_amount_cents = out_of_band_amount_cents, out_of_band_amount_cents = 0, refund_status = 'failed' WHERE id = $1`, cn.ID)
		_, err := cnSvc.RetryRefund(ctx, h.tenantID, cn.ID)
		var de *errs.DomainError
		if !errors.As(err, &de) || !strings.Contains(de.Message, "returned another way") || strings.Contains(de.Message, "void") {
			t.Errorf("retry refund: %v; want it to name returned another way, never a void", err)
		}
	})
}

// TestCommitRelief_Words: what the customer and the operator read. The note's
// line and reason say what was refunded in plain words, never a code, and the
// ledger entry names the note and the credit it took back.
func TestCommitRelief_Words(t *testing.T) {
	h, ctx := newCommitHarness(t, "Relief Words")
	inv := h.createTopup(t, ctx, "USD", 9000, 10000)
	payOffline(t, h, ctx, inv.ID)
	drain(t, h, ctx, 4000, 1)
	cnSvc := newReliefCNService(h)

	cn, err := cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireAll: true})
	if err != nil {
		t.Fatalf("relief: %v", err)
	}
	if want := "Unused prepaid credit refunded"; cn.Reason != want {
		t.Errorf("reason %q, want %q", cn.Reason, want)
	}
	_, lines, err := cnSvc.GetWithLineItems(ctx, h.tenantID, cn.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(lines) != 1 || lines[0].Description != "Refund for $60.00 of unused prepaid credit" {
		t.Errorf("lines %+v, want one: Refund for $60.00 of unused prepaid credit", lines)
	}
	entries, err := h.creditSvc.ListEntries(ctx, credit.ListFilter{TenantID: h.tenantID, CustomerID: h.custID})
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	want := "Refunded by credit note " + cn.CreditNoteNumber + " — $60.00 of bought credit"
	found := false
	for _, e := range entries {
		found = found || (e.EntryType == domain.CreditAdjustment && e.AmountCents == -6000 && e.Description == want)
	}
	if !found {
		t.Errorf("no ledger entry of -6000 reading %q in %+v", want, entries)
	}
}

// A commit-relief credit note is issued through the same issue statement as
// any other. On a Stripe Tax tenant, an invoice that only sells prepaid credit
// is stored with tax_provider 'stripe_tax' but Stripe Tax never calculates it
// (no calculation, no transaction), so its relief owes no tax reversal. Keyed
// on the provider alone, the note carried tax_reversal_pending for good, with
// nothing to reverse.
func TestCommitRelief_OnAStripeTaxTenant_OwesNoTaxReversal(t *testing.T) {
	h, ctx := newCommitHarness(t, "Relief Stripe Tax Tenant")
	inv := h.createTopup(t, ctx, "USD", 9000, 10000)
	execBypassStamp(t, h.db, `UPDATE invoices SET tax_provider = 'stripe_tax', tax_calculation_id = '', tax_transaction_id = NULL WHERE id = $1`, inv.ID)
	payOffline(t, h, ctx, inv.ID)
	drain(t, h, ctx, 4000, 1)

	cn, err := newReliefCNService(h).CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{
		InvoiceID: inv.ID, RetireAll: true, Reason: "customer churned",
	})
	if err != nil {
		t.Fatalf("relief: %v", err)
	}
	if cn.Status != domain.CreditNoteIssued {
		t.Fatalf("status = %s, want issued", cn.Status)
	}
	if cn.TaxReversalPending {
		t.Fatal("the relief of a credit-only invoice Stripe Tax never calculated is marked as owing a tax reversal; nothing would ever clear it")
	}
}
