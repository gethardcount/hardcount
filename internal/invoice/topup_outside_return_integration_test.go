package invoice_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// Money that goes back to the customer outside Hardcount on a top-up's own
// payment, a refund made in Stripe or a lost dispute, is recorded as one
// credit note for the amount, and the note takes back the unused credit the
// amount paid for. Each row is a fresh customer with one top-up paid by card.

// outsideRig is one customer, the credit-note service with Stripe's refund
// call, and the dispute recorder writing a loss's note through it.
type outsideRig struct {
	h        *commitHarness
	ctx      context.Context
	svc      *creditnote.Service
	cards    *stripeRefunds
	unbilled *switchableUnbilled
	at       time.Time
	n        int
}

// switchableUnbilled is billing's unbilled usage, which can fail on demand.
type switchableUnbilled struct{ fail atomic.Bool }

func (u *switchableUnbilled) UnbilledUsage(context.Context, string, string) (map[string]int64, error) {
	if u.fail.Load() {
		return nil, errors.New("live balance: one subscription could not be rated")
	}
	return map[string]int64{}, nil
}

func newOutsideRig(t *testing.T, name string) *outsideRig {
	t.Helper()
	h, ctx := newCommitHarness(t, name)
	cards := &stripeRefunds{}
	svc := newCardReliefService(h, cards)
	unbilled := &switchableUnbilled{}
	svc.SetUnbilledUsageReader(unbilled)
	h.invStore.SetDisputeLossRecorder(svc)
	h.invSvc.SetDisputeReader(h.invStore)
	h.invSvc.SetStripeRefunds(svc)
	return &outsideRig{h: h, ctx: ctx, svc: svc, cards: cards, unbilled: unbilled, at: time.Now().UTC().Truncate(time.Second)}
}

// topup buys granted credit for price by card, then uses `used` of it.
func (r *outsideRig) topup(t *testing.T, price, granted, used int64) (domain.Invoice, string) {
	t.Helper()
	r.n++
	inv := r.h.createTopup(t, r.ctx, "USD", price, granted)
	pi := fmt.Sprintf("pi_out_%d", r.n)
	payByCard(t, r.h, r.ctx, inv.ID, pi)
	if used > 0 {
		drain(t, r.h, r.ctx, used, 500+r.n)
	}
	return inv, pi
}

// refund records a refund made in Stripe of payment pi and returns its row.
func (r *outsideRig) refund(t *testing.T, invoiceID, pi string, cents int64, status domain.StripeRefundStatus) domain.InvoiceStripeRefund {
	t.Helper()
	r.n++
	re := fmt.Sprintf("re_out_%d", r.n)
	recordStripeRefund(t, r.ctx, r.svc, r.h.tenantID, re, pi, cents, status, r.next())
	return r.refundRow(t, invoiceID, re)
}

func (r *outsideRig) refundRow(t *testing.T, invoiceID, refundID string) domain.InvoiceStripeRefund {
	t.Helper()
	rows, err := r.svc.ListStripeRefunds(r.ctx, r.h.tenantID, invoiceID)
	if err != nil {
		t.Fatalf("list refunds made in Stripe: %v", err)
	}
	for _, row := range rows {
		if row.StripeRefundID == refundID {
			return row
		}
	}
	t.Fatalf("no row for refund %s", refundID)
	return domain.InvoiceStripeRefund{}
}

// note is the credit note on the invoice with this id, or the zero note.
func (r *outsideRig) note(t *testing.T, invoiceID, id string) domain.CreditNote {
	t.Helper()
	for _, cn := range reliefNotes(t, r.ctx, r.svc, r.h.tenantID, invoiceID) {
		if cn.ID == id {
			return cn
		}
	}
	return domain.CreditNote{}
}

// dispute records one event of dispute du on payment pi.
func (r *outsideRig) dispute(t *testing.T, du, pi string, status domain.DisputeStatus, cents int64, currency string) domain.InvoiceDispute {
	t.Helper()
	at := r.next()
	res, err := r.h.invStore.RecordStripeDispute(r.ctx, r.h.tenantID, domain.DisputeSnapshot{
		StripeEventID: fmt.Sprintf("evt_%s_%d", du, at.Unix()), EventAt: at, StripeDisputeID: du,
		StripePaymentIntentID: pi, Status: status, AmountCents: cents, Currency: currency, OpenedAt: r.at,
	})
	if err != nil {
		t.Fatalf("record dispute %s (%s): %v", du, status, err)
	}
	return res.Dispute
}

func (r *outsideRig) next() time.Time {
	r.at = r.at.Add(time.Second)
	return r.at
}

// retired is the credit every note took back from the top-up's grant
// (cn_retired_cents), and what is left of it.
func (r *outsideRig) retired(t *testing.T, invoiceID string) (retired, left int64) {
	t.Helper()
	tx, err := r.h.db.BeginTx(r.ctx, postgres.TxTenant, r.h.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if err := tx.QueryRowContext(r.ctx, `
		SELECT cn_retired_cents, amount_cents - consumed_cents FROM customer_credit_ledger
		WHERE source_invoice_id = $1 AND grant_kind = 'commit'`, invoiceID).Scan(&retired, &left); err != nil {
		t.Fatalf("read the grant: %v", err)
	}
	return retired, left
}

func (r *outsideRig) relief(in creditnote.CommitReliefInput) (domain.CreditNote, error) {
	return r.svc.CreateAndIssueCommitRelief(r.ctx, r.h.tenantID, in)
}

// TestTopupStripeRefund_TakesBackTheCreditItPaidFor: a refund made in Stripe
// on a top-up is recorded as on any invoice, a note for its amount whose card
// refund is that refund, and the note takes back the unused credit the
// amount paid for, at the price paid: 45.00 of a 90.00 top-up of 100.00 is
// worth 50.00 of credit, no more than is unused.
func TestTopupStripeRefund_TakesBackTheCreditItPaidFor(t *testing.T) {
	for _, tc := range []struct {
		name               string
		used, cents        int64
		wantTaken, wantBal int64
		pendingDraw        bool // an issued invoice is still to draw on the credit
	}{
		{"all of it unused", 0, 4500, 5000, 5000, false},
		{"part of it used", 7000, 4500, 3000, 0, false},
		{"all of it used", 10000, 4500, 0, 0, false},
		{"the whole payment", 0, 9000, 10000, 0, false},
		// Relief waits for that invoice; money already gone back does not, and
		// the invoice then charges the card for what the credit no longer
		// covers.
		{"an invoice still to draw on the credit", 0, 4500, 5000, 5000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newOutsideRig(t, "Outside Refund "+tc.name)
			r.h.armLowThreshold(t, r.ctx, 1)
			if tc.pendingDraw {
				now := time.Now().UTC()
				if _, err := r.h.invStore.CreateWithLineItems(r.ctx, r.h.tenantID, domain.Invoice{
					CustomerID: r.h.custID, InvoiceNumber: "HC-QUEUED-OUT", BillingReason: domain.BillingReasonSubscriptionCycle,
					Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending, IssuedAt: &now,
					Currency: "USD", SubtotalCents: 1000, TotalAmountCents: 1000, AmountDueCents: 1000,
					BillingPeriodStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), BillingPeriodEnd: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
				}, nil); err != nil {
					t.Fatalf("issued period invoice: %v", err)
				}
			}
			inv, pi := r.topup(t, 9000, 10000, tc.used)

			row := r.refund(t, inv.ID, pi, tc.cents, domain.StripeRefundSucceeded)
			if row.Outcome != domain.StripeRefundRecordedAsCreditNote {
				t.Fatalf("refund outcome %s (%s), want credit_note", row.Outcome, row.ReviewReason)
			}
			cn := r.note(t, inv.ID, row.CreditNoteID)
			if cn.Status != domain.CreditNoteIssued || cn.TotalCents != tc.cents || cn.RefundAmountCents != tc.cents ||
				cn.StripeRefundID != row.StripeRefundID || !cn.RecordedFromStripe || cn.RefundStatus != domain.RefundSucceeded {
				t.Errorf("note = %+v, want an issued note of %d whose card refund is %s, succeeded", cn, tc.cents, row.StripeRefundID)
			}
			if cn.CommitRetiredCents != tc.wantTaken {
				t.Errorf("the note took back %d credit, want %d", cn.CommitRetiredCents, tc.wantTaken)
			}
			if retired, _ := r.retired(t, inv.ID); retired != tc.wantTaken {
				t.Errorf("the grant has %d taken back, want %d", retired, tc.wantTaken)
			}
			if bal := r.h.balance(t, r.ctx); bal != tc.wantBal {
				t.Errorf("balance %d, want %d", bal, tc.wantBal)
			}
			if tc.wantBal == 0 && tc.wantTaken > 0 {
				events := outboxPayloads(t, r.h, r.ctx, domain.EventCreditBalanceDepleted)
				if len(events) != 1 || events[0]["reason"] != "refund" {
					t.Errorf("credit.balance_depleted events = %v, want one with reason refund", events)
				}
			}
			if tc.pendingDraw {
				if _, err := r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireAll: true}); !errors.Is(err, errs.ErrInvalidState) ||
					!strings.Contains(err.Error(), "HC-QUEUED-OUT draws this credit") {
					t.Errorf("relief on the same top-up: %v, want 409 naming the invoice still to draw", err)
				}
			}
		})
	}
}

// TestTopupStripeRefund_PartialsTelescope: each refund stands alone, at its
// own amount, and two partial refunds take back what one refund of their sum
// would. A note that uses up what the payment's documents allow takes all the
// unused credit, whatever the price says: 150.00 paid for 100.00 of credit,
// refunded 49.99 then 100.01, leaves no credit behind.
func TestTopupStripeRefund_PartialsTelescope(t *testing.T) {
	for _, tc := range []struct {
		name           string
		price, granted int64
		refunds, taken []int64
	}{
		{"at a discount", 9000, 10000, []int64{2700, 1800}, []int64{3000, 2000}},
		{"above face", 15000, 10000, []int64{4999, 10001}, []int64{3332, 6668}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newOutsideRig(t, "Outside Partials "+tc.name)
			inv, pi := r.topup(t, tc.price, tc.granted, 0)
			for i, cents := range tc.refunds {
				row := r.refund(t, inv.ID, pi, cents, domain.StripeRefundSucceeded)
				cn := r.note(t, inv.ID, row.CreditNoteID)
				if cn.TotalCents != cents || cn.CommitRetiredCents != tc.taken[i] {
					t.Errorf("refund %d of %d: note of %d took %d credit, want a note of %d taking %d",
						i+1, cents, cn.TotalCents, cn.CommitRetiredCents, cents, tc.taken[i])
				}
			}
			if n := len(reliefNotes(t, r.ctx, r.svc, r.h.tenantID, inv.ID)); n != len(tc.refunds) {
				t.Errorf("%d notes, want one per refund", n)
			}
			if tc.name == "above face" {
				if _, left := r.retired(t, inv.ID); left != 0 {
					t.Errorf("%d credit left on a payment refunded in full, want 0", left)
				}
			}
		})
	}
}

// TestTopupStripeRefund_PendingThenFailedTakesNothing: a refund made in Stripe
// is recorded only once it succeeded. One first seen pending waits, and when
// it fails no money moved: no note, and no credit taken back.
func TestTopupStripeRefund_PendingThenFailedTakesNothing(t *testing.T) {
	r := newOutsideRig(t, "Outside Pending Failed")
	inv, pi := r.topup(t, 9000, 10000, 0)

	row := r.refund(t, inv.ID, pi, 5000, domain.StripeRefundPending)
	if row.Outcome != domain.StripeRefundAwaiting {
		t.Fatalf("pending refund: outcome %s, want awaiting", row.Outcome)
	}
	recordStripeRefund(t, r.ctx, r.svc, r.h.tenantID, row.StripeRefundID, pi, 5000, domain.StripeRefundFailed, r.next())
	if row = r.refundRow(t, inv.ID, row.StripeRefundID); row.Outcome != domain.StripeRefundNotRefunded {
		t.Errorf("failed refund: outcome %s, want not_refunded", row.Outcome)
	}
	if n := len(reliefNotes(t, r.ctx, r.svc, r.h.tenantID, inv.ID)); n != 0 {
		t.Errorf("%d notes for a refund that moved no money, want none", n)
	}
	if retired, _ := r.retired(t, inv.ID); retired != 0 {
		t.Errorf("%d credit taken back, want none", retired)
	}
}

// TestTopupStripeRefund_FlaggedAsOnAnyInvoice: a refund made in Stripe on a
// top-up that fails a check is flagged for the operator, as on any invoice,
// and takes no credit back: one that may replace a relief's failed card
// refund (rule 7), and one larger than the card has left (rule 8).
func TestTopupStripeRefund_FlaggedAsOnAnyInvoice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		relief creditnote.CommitReliefInput
		refuse bool
		cents  int64
		want   domain.StripeRefundReviewReason
	}{
		{"a relief's card refund failed", creditnote.CommitReliefInput{RetireCents: 2000}, true, 1800, domain.StripeRefundReviewCreditNoteRefundFailed},
		{"more than the card has left", creditnote.CommitReliefInput{RetireCents: 6000}, false, 4000, domain.StripeRefundReviewExceedsCardRefundable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newOutsideRig(t, "Outside Flagged "+tc.name)
			inv, pi := r.topup(t, 9000, 10000, 0)
			if tc.refuse {
				r.cards.refuseNext("charge_disputed")
			}
			tc.relief.InvoiceID = inv.ID
			if _, err := r.relief(tc.relief); err != nil {
				t.Fatalf("relief: %v", err)
			}
			retiredBefore, _ := r.retired(t, inv.ID)

			row := r.refund(t, inv.ID, pi, tc.cents, domain.StripeRefundSucceeded)
			if row.Outcome != domain.StripeRefundNeedsReview || row.ReviewReason != tc.want {
				t.Errorf("refund: %s (%s), want needs_review (%s)", row.Outcome, row.ReviewReason, tc.want)
			}
			if n := len(reliefNotes(t, r.ctx, r.svc, r.h.tenantID, inv.ID)); n != 1 {
				t.Errorf("%d notes, want only the relief's", n)
			}
			if retired, _ := r.retired(t, inv.ID); retired != retiredBefore {
				t.Errorf("credit taken back went from %d to %d, want no change", retiredBefore, retired)
			}
		})
	}
}

// TestTopupLostDispute_RecordsOneNote: a lost dispute on a top-up's own
// payment is recorded once, in the transaction that records the loss, as a
// credit note for what the bank returned, no more than the payment's
// documents still allow. The money is out of band (the bank returned it, not
// a card refund), and the note takes back the unused credit it paid for. A
// dispute in another currency writes no note.
func TestTopupLostDispute_RecordsOneNote(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		used                 int64
		relief               int64 // credit retired by a relief first
		events               []domain.DisputeStatus
		cents                int64
		currency             string
		wantTotal, wantTaken int64
		wantNoNote           bool
	}{
		{name: "the whole payment", used: 3000, events: []domain.DisputeStatus{domain.DisputeLost}, cents: 9000, currency: "usd", wantTotal: 9000, wantTaken: 7000},
		{name: "part of it", events: []domain.DisputeStatus{domain.DisputeLost}, cents: 3600, currency: "usd", wantTotal: 3600, wantTaken: 4000},
		{name: "after a relief", relief: 2000, events: []domain.DisputeStatus{domain.DisputeLost}, cents: 9000, currency: "usd", wantTotal: 7200, wantTaken: 8000},
		{name: "in another currency", events: []domain.DisputeStatus{domain.DisputeLost}, cents: 9000, currency: "eur", wantNoNote: true},
		{name: "lost twice, after an appeal", events: []domain.DisputeStatus{domain.DisputeLost, domain.DisputeAppealNeedsResponse, domain.DisputeLost},
			cents: 3600, currency: "usd", wantTotal: 3600, wantTaken: 4000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newOutsideRig(t, "Outside Lost "+tc.name)
			inv, pi := r.topup(t, 9000, 10000, tc.used)
			if tc.relief > 0 {
				if _, err := r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireCents: tc.relief}); err != nil {
					t.Fatalf("relief: %v", err)
				}
			}
			notesBefore := len(reliefNotes(t, r.ctx, r.svc, r.h.tenantID, inv.ID))
			retiredBefore, _ := r.retired(t, inv.ID)

			var d domain.InvoiceDispute
			for _, st := range tc.events {
				d = r.dispute(t, "du_out_"+inv.ID, pi, st, tc.cents, tc.currency)
			}
			if d.Status != domain.DisputeLost {
				t.Fatalf("dispute status %s, want lost", d.Status)
			}
			notes := reliefNotes(t, r.ctx, r.svc, r.h.tenantID, inv.ID)
			retired, _ := r.retired(t, inv.ID)
			if tc.wantNoNote {
				if d.CreditNoteID != "" || len(notes) != notesBefore || retired != retiredBefore {
					t.Errorf("dispute in %s: note %q, %d notes (was %d), %d taken back (was %d); want none of it",
						tc.currency, d.CreditNoteID, len(notes), notesBefore, retired, retiredBefore)
				}
				return
			}
			if len(notes) != notesBefore+1 {
				t.Fatalf("%d notes after the loss, want %d: one for the loss", len(notes), notesBefore+1)
			}
			cn := r.note(t, inv.ID, d.CreditNoteID)
			if cn.ID == "" {
				t.Fatalf("dispute's credit_note_id %q names no note on the top-up", d.CreditNoteID)
			}
			if cn.Status != domain.CreditNoteIssued || cn.TotalCents != tc.wantTotal || cn.OutOfBandAmountCents != tc.wantTotal ||
				cn.RefundAmountCents != 0 || cn.CreditAmountCents != 0 || cn.Reason != "Payment reversed in a dispute" {
				t.Errorf("loss note = %+v, want an issued note of %d out of band, reason %q", cn, tc.wantTotal, "Payment reversed in a dispute")
			}
			if cn.CommitRetiredCents != tc.wantTaken || retired-retiredBefore != tc.wantTaken {
				t.Errorf("the loss took back %d credit (grant: %d), want %d", cn.CommitRetiredCents, retired-retiredBefore, tc.wantTaken)
			}
			// Its reads and its ledger entry say the bank returned the money.
			if !cn.ReturnedByBank {
				t.Errorf("loss note %s does not read as returned by the bank", cn.CreditNoteNumber)
			}
			entries, err := r.h.creditSvc.ListEntries(r.ctx, credit.ListFilter{TenantID: r.h.tenantID, CustomerID: r.h.custID})
			if err != nil {
				t.Fatalf("ledger: %v", err)
			}
			words := "Taken back by credit note " + cn.CreditNoteNumber + " after a lost dispute"
			found := false
			for _, e := range entries {
				found = found || (e.AmountCents == -tc.wantTaken && strings.HasPrefix(e.Description, words))
			}
			if !found {
				t.Errorf("no ledger entry of -%d reading %q", tc.wantTaken, words)
			}
		})
	}
}

// TestCommitRelief_BoundedByTheDocumentCap: once money went back outside
// Hardcount, relief refunds the smaller of the unused credit and what the
// payment's documents still allow. "Already returned in full" is only for a
// payment with no room left. After a lost dispute nothing can go back to the
// card, so relief says how to record the rest another way.
func TestCommitRelief_BoundedByTheDocumentCap(t *testing.T) {
	const inFull = "This payment has already been returned in full."
	t.Run("after the whole payment was lost", func(t *testing.T) {
		r := newOutsideRig(t, "Cap Whole Loss")
		inv, pi := r.topup(t, 9000, 10000, 0)
		r.dispute(t, "du_cap_whole", pi, domain.DisputeLost, 9000, "usd")
		_, err := r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireAll: true})
		var de *errs.DomainError
		if !errors.Is(err, errs.ErrInvalidState) || !errors.As(err, &de) || de.Message != inFull {
			t.Errorf("relief after the whole payment was lost: %v, want 409 %q", err, inFull)
		}
	})
	t.Run("after part of it was lost", func(t *testing.T) {
		r := newOutsideRig(t, "Cap Part Loss")
		inv, pi := r.topup(t, 9000, 10000, 0)
		r.dispute(t, "du_cap_part", pi, domain.DisputeLost, 3600, "usd")
		_, err := r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireAll: true})
		if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "refund_amount_cents" || !strings.Contains(err.Error(), "dispute") {
			t.Fatalf("relief to the card after a loss: %v, want 422 on refund_amount_cents naming the dispute", err)
		}
		oob := int64(5400)
		cn, err := r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireAll: true, OutOfBandAmountCents: &oob})
		if err != nil || cn.TotalCents != 5400 || cn.OutOfBandAmountCents != 5400 || cn.CommitRetiredCents != 6000 {
			t.Errorf("relief out of band: %+v, %v; want 54.00 out of band for 60.00 of credit", cn, err)
		}
	})
	// The reviewer's case: 80.00 of credit used, 50.00 refunded in Stripe
	// (which took back the 20.00 then unused), then the usage invoice voided,
	// which gave the 80.00 back. The documents allow 40.00 more, worth 44.45
	// of credit at the price paid (f rounds half to even: f(64.45) = 58.00).
	// 44.46 is refused; 44.45 and 44.44 return the last 40.00, so each would
	// take all 80.00 and the operator is told to send retire_all; 44.43
	// returns 39.99. retire_all refunds the 40.00, and is not refused for the
	// 72.00 the 80.00 unused would be worth. A note that uses up what the
	// documents allow takes all the unused credit: none may outlive a payment
	// returned in full.
	reviewerCase := func(t *testing.T, name string) (*outsideRig, domain.Invoice) {
		r := newOutsideRig(t, name)
		inv, pi := r.topup(t, 9000, 10000, 0)
		usage := r.h.seedPayable(t, r.ctx, r.h.custID, 8000, 900)
		if _, err := credit.NewPostgresStore(r.h.db).ApplyToInvoiceAtomic(r.ctx, r.h.tenantID, r.h.custID, usage, "usage", 8000, time.Now().UTC()); err != nil {
			t.Fatalf("draw: %v", err)
		}
		row := r.refund(t, inv.ID, pi, 5000, domain.StripeRefundSucceeded)
		if cn := r.note(t, inv.ID, row.CreditNoteID); cn.CommitRetiredCents != 2000 {
			t.Fatalf("refund %s (%s): its note took %d credit, want the 2000 unused", row.Outcome, row.ReviewReason, cn.CommitRetiredCents)
		}
		if _, err := r.h.invSvc.Void(r.ctx, r.h.tenantID, usage); err != nil {
			t.Fatalf("void the usage invoice: %v", err)
		}
		return r, inv
	}
	t.Run("after a refund made in Stripe, then a void that returned credit", func(t *testing.T) {
		r, inv := reviewerCase(t, "Cap Reviewer Case")
		if _, err := r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireCents: 4446}); !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "retire_cents" {
			t.Errorf("retire 44.46 of credit: %v, want 422 on retire_cents: the documents allow 44.45", err)
		}
		for _, cents := range []int64{4445, 4444} {
			if _, err := r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireCents: cents}); !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "retire_cents" || !strings.Contains(err.Error(), "retire_all") {
				t.Errorf("retire %d of credit, the last of the payment: %v, want 422 on retire_cents naming retire_all", cents, err)
			}
		}
		cn, err := r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireAll: true})
		if err != nil || cn.TotalCents != 4000 || cn.CommitRetiredCents != 8000 {
			t.Fatalf("retire_all: total %d taking %d credit, %v; want 40.00 taking all 80.00 unused", cn.TotalCents, cn.CommitRetiredCents, err)
		}
		_, err = r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireAll: true})
		var de *errs.DomainError
		if !errors.As(err, &de) || de.Message != inFull {
			t.Errorf("relief with no room left: %v, want %q", err, inFull)
		}
	})
	t.Run("the same, a cent short of the last of the payment", func(t *testing.T) {
		r, inv := reviewerCase(t, "Cap Reviewer Case Short")
		cn, err := r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireCents: 4443})
		if err != nil || cn.TotalCents != 3999 || cn.CommitRetiredCents != 4443 {
			t.Fatalf("retire 44.43: total %d taking %d credit, %v; want 39.99 taking 44.43", cn.TotalCents, cn.CommitRetiredCents, err)
		}
		// The last cent uses up what the documents allow, so it takes all the
		// credit left.
		cn, err = r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireAll: true})
		if err != nil || cn.TotalCents != 1 || cn.CommitRetiredCents != 8000-4443 {
			t.Errorf("retire_all after: total %d taking %d credit, %v; want 0.01 taking the %d left", cn.TotalCents, cn.CommitRetiredCents, err, 8000-4443)
		}
	})
}

// TestTopupLostDispute_UnbilledReadFails: the recording never waits on
// credit. When the usage not invoiced yet cannot be read, the loss is
// recorded without its note and a Critical notice says why; the operator's
// record-loss then writes it.
func TestTopupLostDispute_UnbilledReadFails(t *testing.T) {
	r := newOutsideRig(t, "Outside Loss Unbilled Fails")
	r.h.invSvc.SetTopupCreditReader(r.svc)
	inv, pi := r.topup(t, 9000, 10000, 0)

	r.unbilled.fail.Store(true)
	d := r.dispute(t, "du_out_unbilled", pi, domain.DisputeLost, 9000, "usd")
	if d.Status != domain.DisputeLost || d.CreditNoteID != "" {
		t.Fatalf("dispute: status %s, note %q; want lost with no note", d.Status, d.CreditNoteID)
	}
	if n := len(reliefNotes(t, r.ctx, r.svc, r.h.tenantID, inv.ID)); n != 0 {
		t.Errorf("%d notes while the credit could not be read, want none", n)
	}
	got, err := r.h.invSvc.Get(r.ctx, r.h.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if att := got.Attention; att == nil || att.Reason != domain.AttentionReasonDisputeLost || att.Severity != domain.AttentionSeverityCritical {
		t.Errorf("notice = %+v, want a Critical dispute_lost", att)
	}

	r.unbilled.fail.Store(false)
	d, err = r.h.invSvc.RecordDisputeLoss(r.ctx, r.h.tenantID, inv.ID, d.ID)
	if err != nil {
		t.Fatalf("record-loss: %v", err)
	}
	cn := r.note(t, inv.ID, d.CreditNoteID)
	if cn.TotalCents != 9000 || cn.CommitRetiredCents != 10000 {
		t.Errorf("record-loss note = %+v, want 90.00 taking all 100.00 of credit", cn)
	}
	got, _ = r.h.invSvc.Get(r.ctx, r.h.tenantID, inv.ID)
	if att := got.Attention; att == nil || att.Severity != domain.AttentionSeverityWarning {
		t.Errorf("notice after record-loss = %+v, want a Warning", att)
	}
	if _, err := r.h.invSvc.RecordDisputeLoss(r.ctx, r.h.tenantID, inv.ID, d.ID); !errors.Is(err, errs.ErrInvalidState) {
		t.Errorf("a second record-loss: %v, want 409", err)
	}
}

// TestTopupStripeRefund_LinkedCreateRecordsAFlaggedRefund: every refund made
// in Stripe flagged on a top-up has the way out it has on any invoice, the
// linked create, which writes the note for its amount and takes back the
// credit it paid for.
func TestTopupStripeRefund_LinkedCreateRecordsAFlaggedRefund(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag func(r *outsideRig, t *testing.T, invoiceID, pi string) domain.InvoiceStripeRefund
		want domain.StripeRefundReviewReason
	}{
		{"seen more than a day late", func(r *outsideRig, t *testing.T, invoiceID, pi string) domain.InvoiceStripeRefund {
			at := r.next()
			if _, err := r.svc.RecordStripeRefund(r.ctx, r.h.tenantID, domain.StripeRefundSnapshot{
				StripeEventID: "evt_out_late", EventAt: at, StripeRefundID: "re_out_late", StripePaymentIntentID: pi,
				AmountCents: 4500, Currency: "usd", Status: domain.StripeRefundSucceeded, CreatedAt: at.Add(-48 * time.Hour),
			}); err != nil {
				t.Fatalf("record refund: %v", err)
			}
			return r.refundRow(t, invoiceID, "re_out_late")
		}, domain.StripeRefundReviewSeenLate},
		{"the credit could not be read", func(r *outsideRig, t *testing.T, invoiceID, pi string) domain.InvoiceStripeRefund {
			r.unbilled.fail.Store(true)
			defer r.unbilled.fail.Store(false)
			return r.refund(t, invoiceID, pi, 4500, domain.StripeRefundSucceeded)
		}, domain.StripeRefundReviewCreditNoteNotWritten},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newOutsideRig(t, "Outside Linked "+tc.name)
			r.h.invSvc.SetTopupCreditReader(r.svc)
			rows := &auditRows{}
			r.svc.SetAuditLogger(rows)
			inv, pi := r.topup(t, 9000, 10000, 0)
			row := tc.flag(r, t, inv.ID, pi)
			if row.Outcome != domain.StripeRefundNeedsReview || row.ReviewReason != tc.want {
				t.Fatalf("refund: %s (%s), want needs_review (%s)", row.Outcome, row.ReviewReason, tc.want)
			}
			if n := len(reliefNotes(t, r.ctx, r.svc, r.h.tenantID, inv.ID)); n != 0 {
				t.Fatalf("%d notes before the operator records it, want none", n)
			}
			if tc.want == domain.StripeRefundReviewCreditNoteNotWritten {
				got, _ := r.h.invSvc.Get(r.ctx, r.h.tenantID, inv.ID)
				if att := got.Attention; att == nil || att.Severity != domain.AttentionSeverityCritical {
					t.Errorf("notice = %+v, want Critical", att)
				}
			}

			r.svc.SetRefundReader(fixedRefund{domain.StripeRefundFacts{ID: row.StripeRefundID, PaymentIntentID: pi, AmountCents: 4500, Currency: "usd", Status: domain.StripeRefundSucceeded}})
			cn, err := r.svc.Create(r.ctx, r.h.tenantID, creditnote.CreateInput{
				InvoiceID: inv.ID, Reason: "Refunded in Stripe", RefundAmountCents: 4500, AutoIssue: true, StripeRefundID: row.StripeRefundID,
				Lines: []creditnote.CreditLineInput{{Description: "Refund made in Stripe (" + row.StripeRefundID + ")", Quantity: 1, UnitAmountCents: 4500}},
			})
			if err != nil {
				t.Fatalf("linked create: %v", err)
			}
			if cn.Status != domain.CreditNoteIssued || cn.TotalCents != 4500 || cn.StripeRefundID != row.StripeRefundID || cn.CommitRetiredCents != 5000 {
				t.Errorf("linked note = %+v, want 45.00 holding %s, taking back 50.00 of credit", cn, row.StripeRefundID)
			}
			if retired, _ := r.retired(t, inv.ID); retired != 5000 {
				t.Errorf("grant has %d taken back, want 5000", retired)
			}
			issued := false
			for _, e := range rows.rows {
				issued = issued || (e.ResourceID == cn.ID && e.Action == "credit_note.issued" && e.Metadata["credit_taken_back_cents"] == int64(5000))
			}
			if !issued {
				t.Errorf("no credit_note.issued audit row of %s with credit_taken_back_cents 5000 in %+v", cn.ID, rows.rows)
			}
			if row = r.refundRow(t, inv.ID, row.StripeRefundID); row.Outcome != domain.StripeRefundRecordedAsCreditNote || row.CreditNoteID != cn.ID {
				t.Errorf("refund row: %s, note %q; want credit_note %s", row.Outcome, row.CreditNoteID, cn.ID)
			}
		})
	}
}

// TestTopupStripeRefund_RetryAfterFailedSendsTheAmount: a recorded refund
// whose card refund later fails keeps its note and the credit it took back;
// Retry refund sends the refund's amount again, not a figure worked out from
// the credit.
func TestTopupStripeRefund_RetryAfterFailedSendsTheAmount(t *testing.T) {
	r := newOutsideRig(t, "Outside Retry")
	inv, pi := r.topup(t, 9000, 10000, 0)
	row := r.refund(t, inv.ID, pi, 4500, domain.StripeRefundSucceeded)
	recordStripeRefund(t, r.ctx, r.svc, r.h.tenantID, row.StripeRefundID, pi, 4500, domain.StripeRefundFailed, r.next())
	cn := r.note(t, inv.ID, row.CreditNoteID)
	if cn.RefundStatus != domain.RefundFailed {
		t.Fatalf("note refund status %s, want failed", cn.RefundStatus)
	}
	retried, err := r.svc.RetryRefund(r.ctx, r.h.tenantID, cn.ID)
	if err != nil {
		t.Fatalf("retry refund: %v", err)
	}
	if got := r.cards.lastAmount.Load(); got != 4500 || retried.RefundStatus != domain.RefundSucceeded {
		t.Errorf("retry sent %d (status %s), want 4500, succeeded", got, retried.RefundStatus)
	}
	if retired, _ := r.retired(t, inv.ID); retired != 5000 {
		t.Errorf("grant has %d taken back after the retry, want 5000", retired)
	}
}

// outboxPayloads returns the payloads of one event type queued for the
// harness's tenant, oldest first.
func outboxPayloads(t *testing.T, h *commitHarness, ctx context.Context, eventType string) []map[string]any {
	t.Helper()
	tx, err := h.db.BeginTx(ctx, postgres.TxTenant, h.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM webhook_outbox WHERE event_type = $1 ORDER BY next_attempt_at, id`, eventType)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var p jsonMap
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// jsonMap scans a jsonb column.
type jsonMap map[string]any

func (m *jsonMap) Scan(src any) error {
	b, ok := src.([]byte)
	if !ok {
		return fmt.Errorf("jsonMap: %T", src)
	}
	return json.Unmarshal(b, (*map[string]any)(m))
}

var _ sql.Scanner = (*jsonMap)(nil)

// flakyInvoices is the invoice reader whose failOn-th Get fails, as a
// database read can.
type flakyInvoices struct {
	creditnote.InvoiceReader
	failOn int32
	n      atomic.Int32
}

func (f *flakyInvoices) Get(ctx context.Context, tenantID, id string) (domain.Invoice, error) {
	if f.n.Add(1) == f.failOn {
		return domain.Invoice{}, errors.New("read invoice: connection reset")
	}
	return f.InvoiceReader.Get(ctx, tenantID, id)
}

// lateRefund records a refund made in Stripe that Hardcount first hears of
// two days after it was made, so it is flagged seen_late.
func (r *outsideRig) lateRefund(t *testing.T, invoiceID, pi string, cents int64) domain.InvoiceStripeRefund {
	t.Helper()
	r.n++
	at, re := r.next(), fmt.Sprintf("re_late_out_%d", r.n)
	if _, err := r.svc.RecordStripeRefund(r.ctx, r.h.tenantID, domain.StripeRefundSnapshot{
		StripeEventID: "evt_" + re, EventAt: at, StripeRefundID: re, StripePaymentIntentID: pi,
		AmountCents: cents, Currency: "usd", Status: domain.StripeRefundSucceeded, CreatedAt: at.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("record refund: %v", err)
	}
	row := r.refundRow(t, invoiceID, re)
	if row.ReviewReason != domain.StripeRefundReviewSeenLate {
		t.Fatalf("late refund: %s (%s), want needs_review seen_late", row.Outcome, row.ReviewReason)
	}
	return row
}

// link records a flagged refund made in Stripe with the operator's linked
// create.
func (r *outsideRig) link(invoiceID, pi string, row domain.InvoiceStripeRefund) (domain.CreditNote, error) {
	r.svc.SetRefundReader(fixedRefund{domain.StripeRefundFacts{ID: row.StripeRefundID, PaymentIntentID: pi, AmountCents: row.AmountCents, Currency: "usd", Status: domain.StripeRefundSucceeded}})
	return r.svc.Create(r.ctx, r.h.tenantID, creditnote.CreateInput{
		InvoiceID: invoiceID, Reason: "Refunded in Stripe", RefundAmountCents: row.AmountCents, AutoIssue: true, StripeRefundID: row.StripeRefundID,
		Lines: []creditnote.CreditLineInput{{Description: "Refund made in Stripe (" + row.StripeRefundID + ")", Quantity: 1, UnitAmountCents: row.AmountCents}},
	})
}

// TestTopupLostDispute_ARefundMadeBeforeStillRecords: a lost dispute stops
// Hardcount sending anything more to the card, but a refund made in Stripe
// before it moved its money already, so the operator's linked create still
// records it, on a top-up and on an ordinary invoice. The sequence: 27.00
// refunded in Stripe is flagged, the other 63.00 is disputed and lost, then
// the 27.00 is recorded; the whole payment is back, and no credit is left.
func TestTopupLostDispute_ARefundMadeBeforeStillRecords(t *testing.T) {
	t.Run("a top-up", func(t *testing.T) {
		r := newOutsideRig(t, "Lost Then Linked")
		inv, pi := r.topup(t, 9000, 10000, 0)
		row := r.lateRefund(t, inv.ID, pi, 2700)
		d := r.dispute(t, "du_lost_then_linked", pi, domain.DisputeLost, 6300, "usd")
		if cn := r.note(t, inv.ID, d.CreditNoteID); cn.TotalCents != 6300 {
			t.Fatalf("loss note %+v, want 63.00", cn)
		}
		cn, err := r.link(inv.ID, pi, row)
		if err != nil || cn.TotalCents != 2700 || cn.StripeRefundID != row.StripeRefundID {
			t.Fatalf("linked create after the loss: %+v, %v; want a note of 27.00 holding %s", cn, err, row.StripeRefundID)
		}
		if _, left := r.retired(t, inv.ID); left != 0 {
			t.Errorf("%d credit left after the whole payment came back, want 0", left)
		}
	})
	t.Run("an ordinary invoice", func(t *testing.T) {
		r := newOutsideRig(t, "Lost Then Linked Ordinary")
		r.n++
		id := r.h.seedPayable(t, r.ctx, r.h.custID, 9000, 800)
		const pi = "pi_lost_then_linked_ord"
		payByCard(t, r.h, r.ctx, id, pi)
		row := r.lateRefund(t, id, pi, 2000)
		r.dispute(t, "du_lost_then_linked_ord", pi, domain.DisputeLost, 1000, "usd")
		if cn, err := r.link(id, pi, row); err != nil || cn.TotalCents != 2000 {
			t.Errorf("linked create after the loss: %+v, %v; want a note of 20.00", cn, err)
		}
	})
}

// TestTopupOutsideNote_DocumentsRoomCountsRefundsNoNoteHolds: the room a
// note has is what the payment's documents leave, less refunds made in
// Stripe that no note holds, for relief and for both outside notes. After
// 27.00 refunded in Stripe and dismissed, a 63.00 refund or a 63.00 loss
// returns the rest of the payment, so its note takes all the unused credit.
func TestTopupOutsideNote_DocumentsRoomCountsRefundsNoNoteHolds(t *testing.T) {
	for _, rest := range []string{"a refund made in Stripe", "a lost dispute"} {
		t.Run(rest, func(t *testing.T) {
			r := newOutsideRig(t, "Room "+rest)
			inv, pi := r.topup(t, 9000, 10000, 0)
			row := r.lateRefund(t, inv.ID, pi, 2700)
			if _, err := r.svc.DismissStripeRefund(r.ctx, r.h.tenantID, inv.ID, row.StripeRefundID, "handled by hand"); err != nil {
				t.Fatalf("dismiss: %v", err)
			}
			var cn domain.CreditNote
			if rest == "a refund made in Stripe" {
				cn = r.note(t, inv.ID, r.refund(t, inv.ID, pi, 6300, domain.StripeRefundSucceeded).CreditNoteID)
			} else {
				cn = r.note(t, inv.ID, r.dispute(t, "du_room", pi, domain.DisputeLost, 9000, "usd").CreditNoteID)
			}
			if cn.TotalCents != 6300 || cn.CommitRetiredCents != 10000 {
				t.Errorf("note of %d taking %d credit, want 63.00 taking all 100.00", cn.TotalCents, cn.CommitRetiredCents)
			}
		})
	}
}

// TestCommitRelief_WaitsForAnUnrecordedLoss: while a lost dispute on the
// payment has no credit note and record-loss can still write it, relief is
// refused and names record-loss: relief paid out of band would record a
// payment nobody made, and record-loss could then write a second note.
func TestCommitRelief_WaitsForAnUnrecordedLoss(t *testing.T) {
	r := newOutsideRig(t, "Relief Waits For Loss")
	inv, pi := r.topup(t, 9000, 10000, 0)
	r.unbilled.fail.Store(true)
	d := r.dispute(t, "du_unrecorded", pi, domain.DisputeLost, 3000, "usd")
	r.unbilled.fail.Store(false)
	if d.CreditNoteID != "" {
		t.Fatalf("loss recorded with note %s, want none", d.CreditNoteID)
	}
	oob := int64(9000)
	cn, err := r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireAll: true, OutOfBandAmountCents: &oob})
	if !errors.Is(err, errs.ErrInvalidState) || !strings.Contains(err.Error(), "record-loss") {
		t.Errorf("relief while the loss has no note: %+v, %v; want 409 naming record-loss", cn, err)
	}
}

// TestTopupStripeRefund_AFailedReadIsNotZero: when the invoice cannot be read
// before the recorder's transaction, the usage not invoiced yet is unknown,
// not zero: the refund on a top-up is flagged credit_note_not_written, as
// when the usage read itself fails. The linked create returns the error and
// writes nothing.
func TestTopupStripeRefund_AFailedReadIsNotZero(t *testing.T) {
	t.Run("the recorder", func(t *testing.T) {
		r := newOutsideRig(t, "Failed Read Recorder")
		inv, pi := r.topup(t, 9000, 10000, 0)
		r.svc = flakySvc(r, 1)
		row := r.refund(t, inv.ID, pi, 4500, domain.StripeRefundSucceeded)
		if row.Outcome != domain.StripeRefundNeedsReview || row.ReviewReason != domain.StripeRefundReviewCreditNoteNotWritten {
			t.Errorf("refund: %s (%s), want needs_review credit_note_not_written", row.Outcome, row.ReviewReason)
		}
	})
	t.Run("the linked create", func(t *testing.T) {
		r := newOutsideRig(t, "Failed Read Linked")
		inv, pi := r.topup(t, 9000, 10000, 0)
		row := r.lateRefund(t, inv.ID, pi, 4500)
		r.svc = flakySvc(r, 3) // Create's checks read the invoice twice first
		if cn, err := r.link(inv.ID, pi, row); err == nil {
			t.Errorf("linked create with the invoice unread: %+v, want an error", cn)
		}
		if n := len(reliefNotes(t, r.ctx, r.svc, r.h.tenantID, inv.ID)); n != 0 {
			t.Errorf("%d notes, want none", n)
		}
	})
	t.Run("the linked create, its usage unread", func(t *testing.T) {
		r := newOutsideRig(t, "Failed Usage Linked")
		inv, pi := r.topup(t, 9000, 10000, 0)
		row := r.lateRefund(t, inv.ID, pi, 4500)
		r.unbilled.fail.Store(true)
		if cn, err := r.link(inv.ID, pi, row); !errors.Is(err, errs.ErrUnavailable) || !strings.Contains(err.Error(), "wrote nothing") {
			t.Errorf("linked create with the usage unread: %+v, %v; want 503 saying nothing was written", cn, err)
		}
		if n := len(reliefNotes(t, r.ctx, r.svc, r.h.tenantID, inv.ID)); n != 0 {
			t.Errorf("%d notes, want none", n)
		}
	})
}

// flakySvc is the rig's credit-note service whose invoice reader fails its
// failOn-th Get.
func flakySvc(r *outsideRig, failOn int32) *creditnote.Service {
	cnStore := creditnote.NewPostgresStore(r.h.db)
	svc := creditnote.NewService(cnStore, &flakyInvoices{InvoiceReader: r.h.invStore, failOn: failOn}, r.cards, reliefGranterAdapter{r.h.creditSvc})
	svc.SetNumberGenerator(tenant.NewSettingsStore(r.h.db))
	svc.SetUnbilledUsageReader(r.unbilled)
	svc.SetStripeRefundRecording(cnStore, r.h.invStore, webhook.NewOutboxStore(r.h.db))
	return svc
}

// TestTopupOutsideNote_RedeliveryAfterTheFallback: when the usage not
// invoiced yet cannot be read, both recorders record what happened without
// its note (the refund flagged credit_note_not_written, the loss with no
// note). Stripe's redelivery of that event, and a later event, once the
// usage reads again, record nothing twice and write no note: the note stays
// for the operator, whose action writes it once.
func TestTopupOutsideNote_RedeliveryAfterTheFallback(t *testing.T) {
	t.Run("the Stripe refund recorder", func(t *testing.T) {
		r := newOutsideRig(t, "Redelivery Refund")
		inv, pi := r.topup(t, 9000, 10000, 0)
		r.unbilled.fail.Store(true)
		row := r.refund(t, inv.ID, pi, 4500, domain.StripeRefundSucceeded)
		r.unbilled.fail.Store(false)
		snap := domain.StripeRefundSnapshot{
			StripeEventID: row.LastStripeEventID, EventAt: row.LastEventAt, StripeRefundID: row.StripeRefundID, StripePaymentIntentID: pi,
			AmountCents: 4500, Currency: "usd", Status: domain.StripeRefundSucceeded, CreatedAt: row.RefundCreatedAt,
		}
		later := snap
		later.StripeEventID, later.EventAt = snap.StripeEventID+"_later", r.next()
		for _, ev := range []domain.StripeRefundSnapshot{snap, later} {
			if _, err := r.svc.RecordStripeRefund(r.ctx, r.h.tenantID, ev); err != nil {
				t.Fatalf("redeliver %s: %v", ev.StripeEventID, err)
			}
		}
		rows, err := r.svc.ListStripeRefunds(r.ctx, r.h.tenantID, inv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].ReviewReason != domain.StripeRefundReviewCreditNoteNotWritten || len(reliefNotes(t, r.ctx, r.svc, r.h.tenantID, inv.ID)) != 0 {
			t.Fatalf("after the redeliveries: %+v; want the one refund still flagged credit_note_not_written, and no note", rows)
		}
		if cn, err := r.link(inv.ID, pi, rows[0]); err != nil || cn.CommitRetiredCents != 5000 {
			t.Fatalf("the operator's linked create: %+v, %v; want a note taking 50.00", cn, err)
		}
		if _, err := r.link(inv.ID, pi, rows[0]); err == nil {
			t.Errorf("a second linked create of the same refund succeeded")
		}
	})
	t.Run("the dispute recorder", func(t *testing.T) {
		r := newOutsideRig(t, "Redelivery Loss")
		inv, pi := r.topup(t, 9000, 10000, 0)
		r.unbilled.fail.Store(true)
		d := r.dispute(t, "du_redeliver", pi, domain.DisputeLost, 9000, "usd")
		r.unbilled.fail.Store(false)
		snap := domain.DisputeSnapshot{
			StripeEventID: d.LastStripeEventID, EventAt: d.LastEventAt, StripeDisputeID: d.StripeDisputeID,
			StripePaymentIntentID: pi, Status: domain.DisputeLost, AmountCents: 9000, Currency: "usd", OpenedAt: d.OpenedAt,
		}
		later := snap
		later.StripeEventID, later.EventAt = snap.StripeEventID+"_later", r.next()
		for _, ev := range []domain.DisputeSnapshot{snap, later} {
			res, err := r.h.invStore.RecordStripeDispute(r.ctx, r.h.tenantID, ev)
			if err != nil || res.Dispute.CreditNoteID != "" {
				t.Fatalf("redeliver %s: note %q, %v; want the loss still without its note", ev.StripeEventID, res.Dispute.CreditNoteID, err)
			}
		}
		if n := len(reliefNotes(t, r.ctx, r.svc, r.h.tenantID, inv.ID)); n != 0 {
			t.Fatalf("%d notes after the redeliveries, want none", n)
		}
		if got, err := r.h.invSvc.RecordDisputeLoss(r.ctx, r.h.tenantID, inv.ID, d.ID); err != nil || got.CreditNoteID == "" {
			t.Fatalf("record-loss: %+v, %v; want the loss's note", got, err)
		}
		if _, err := r.h.invSvc.RecordDisputeLoss(r.ctx, r.h.tenantID, inv.ID, d.ID); !errors.Is(err, errs.ErrInvalidState) {
			t.Errorf("a second record-loss: %v, want 409", err)
		}
	})
}

// TestTopupLostDispute_ThenWon: Stripe can record a lost dispute as won
// later. The loss's note stands, so relief finds the payment returned in
// full, and no dispute notice remains: the lost notice said to sell the
// credit again as a top-up paid outside Hardcount.
func TestTopupLostDispute_ThenWon(t *testing.T) {
	r := newOutsideRig(t, "Lost Then Won")
	r.h.invSvc.SetTopupCreditReader(r.svc)
	inv, pi := r.topup(t, 9000, 10000, 0)
	lost := r.dispute(t, "du_lost_won", pi, domain.DisputeLost, 9000, "usd")
	won := r.dispute(t, "du_lost_won", pi, domain.DisputeWon, 9000, "usd")
	if cn := r.note(t, inv.ID, lost.CreditNoteID); won.CreditNoteID != lost.CreditNoteID || cn.Status != domain.CreditNoteIssued {
		t.Fatalf("after the win: dispute names %q, note %+v; want the loss's issued note standing", won.CreditNoteID, cn)
	}
	var de *errs.DomainError
	if _, err := r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireAll: true}); !errors.As(err, &de) || de.Message != "This payment has already been returned in full." {
		t.Errorf("relief after the win: %v, want 409: the payment has already been returned in full", err)
	}
	got, err := r.h.invSvc.Get(r.ctx, r.h.tenantID, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attention != nil && (got.Attention.Reason == domain.AttentionReasonDisputeLost || got.Attention.Reason == domain.AttentionReasonDisputeOpen) {
		t.Errorf("notice after the win: %+v, want no dispute notice", got.Attention)
	}
}

// TestRecordDisputeLoss_Refusals: record-loss answers 404 for a dispute not on
// the invoice before anything else, 503 having written nothing when the usage
// not invoiced yet cannot be read, and 409 with a sentence for each loss it
// cannot record.
func TestRecordDisputeLoss_Refusals(t *testing.T) {
	r := newOutsideRig(t, "Record Loss Refusals")
	lost := func(t *testing.T, pi, du string, cents int64, currency string) string {
		return r.dispute(t, du, pi, domain.DisputeLost, cents, currency).ID
	}
	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T) (invoiceID, disputeID string)
		failUsage bool
		kind      error
		says      string
	}{
		{"a dispute not on the invoice, which is no top-up", func(t *testing.T) (string, string) {
			return r.h.seedPayable(t, r.ctx, r.h.custID, 5000, 950), "hc_dsp_none"
		}, false, errs.ErrNotFound, ""},
		{"not lost", func(t *testing.T) (string, string) {
			inv, pi := r.topup(t, 9000, 10000, 0)
			return inv.ID, r.dispute(t, "du_ref_open", pi, domain.DisputeNeedsResponse, 9000, "usd").ID
		}, false, errs.ErrInvalidState, "not lost"},
		{"already recorded", func(t *testing.T) (string, string) {
			inv, pi := r.topup(t, 9000, 10000, 0)
			return inv.ID, lost(t, pi, "du_ref_done", 3000, "usd")
		}, false, errs.ErrInvalidState, "already recorded"},
		{"in another currency", func(t *testing.T) (string, string) {
			inv, pi := r.topup(t, 9000, 10000, 0)
			return inv.ID, lost(t, pi, "du_ref_eur", 9000, "eur")
		}, false, errs.ErrInvalidState, "commit relief with its money as out_of_band_amount_cents"},
		{"nothing left to record", func(t *testing.T) (string, string) {
			inv, pi := r.topup(t, 9000, 10000, 0)
			r.refund(t, inv.ID, pi, 9000, domain.StripeRefundSucceeded)
			return inv.ID, lost(t, pi, "du_ref_full", 9000, "usd")
		}, false, errs.ErrInvalidState, "returned in full"},
		{"another payment's dispute", func(t *testing.T) (string, string) {
			inv, _ := r.topup(t, 9000, 10000, 0)
			execBypassStamp(t, r.h.db, `UPDATE invoices SET payment_anomaly_payment_intent_id = 'pi_ref_other' WHERE id = $1`, inv.ID)
			return inv.ID, lost(t, "pi_ref_other", "du_ref_other", 9000, "usd")
		}, false, errs.ErrInvalidState, "not on the payment"},
		{"the usage unread", func(t *testing.T) (string, string) {
			inv, pi := r.topup(t, 9000, 10000, 0)
			r.unbilled.fail.Store(true)
			return inv.ID, lost(t, pi, "du_ref_unread", 9000, "usd")
		}, true, errs.ErrUnavailable, "wrote nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invoiceID, disputeID := tc.setup(t)
			notes := len(reliefNotes(t, r.ctx, r.svc, r.h.tenantID, invoiceID))
			_, err := r.h.invSvc.RecordDisputeLoss(r.ctx, r.h.tenantID, invoiceID, disputeID)
			r.unbilled.fail.Store(false)
			if !errors.Is(err, tc.kind) || !strings.Contains(fmt.Sprint(err), tc.says) {
				t.Errorf("record-loss: %v, want %v saying %q", err, tc.kind, tc.says)
			}
			if n := len(reliefNotes(t, r.ctx, r.svc, r.h.tenantID, invoiceID)); n != notes {
				t.Errorf("%d notes after the refusal, want %d", n, notes)
			}
		})
	}
}

// TestTopupLostDispute_UnwiredRecorderRefuses: a store with no loss recorder
// wired refuses a loss on a top-up, which Stripe then sends again, instead
// of recording it with no note and no notice of why; a loss on an ordinary
// invoice needs no recorder. Boot refuses such a server
// (MustHaveDisputeLossRecorder).
func TestTopupLostDispute_UnwiredRecorderRefuses(t *testing.T) {
	r := newOutsideRig(t, "Unwired Loss Recorder")
	store := invoice.NewPostgresStore(r.h.db)
	inv, pi := r.topup(t, 9000, 10000, 0)
	snap := domain.DisputeSnapshot{StripeEventID: "evt_unwired", EventAt: r.next(), StripeDisputeID: "du_unwired",
		StripePaymentIntentID: pi, Status: domain.DisputeLost, AmountCents: 9000, Currency: "usd", OpenedAt: r.at}
	if _, err := store.RecordStripeDispute(r.ctx, r.h.tenantID, snap); err == nil || !strings.Contains(err.Error(), "SetDisputeLossRecorder") {
		t.Errorf("loss on a top-up with no recorder: %v, want an error naming SetDisputeLossRecorder", err)
	}
	if ds, err := r.h.invStore.ListDisputesByInvoice(r.ctx, r.h.tenantID, inv.ID); err != nil || len(ds) != 0 {
		t.Errorf("disputes %+v, %v; want none recorded", ds, err)
	}
	r.n++
	ordinary := r.h.seedPayable(t, r.ctx, r.h.custID, 9000, 960)
	payByCard(t, r.h, r.ctx, ordinary, "pi_unwired_ordinary")
	snap.StripeEventID, snap.StripeDisputeID, snap.StripePaymentIntentID = "evt_unwired_ord", "du_unwired_ord", "pi_unwired_ordinary"
	if _, err := store.RecordStripeDispute(r.ctx, r.h.tenantID, snap); err != nil {
		t.Errorf("loss on an ordinary invoice with no recorder: %v, want it recorded", err)
	}
}
