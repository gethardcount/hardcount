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
	"github.com/gethardcount/hardcount/internal/platform/postgres"
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
	// of credit at the price paid (f rounds half to even: f(64.45) = 58.00):
	// retire_all refunds that, and is not refused
	// for the 72.00 the 80.00 unused would be worth. The note uses up what
	// the documents allow, so it takes all 80.00 of unused credit: none may
	// outlive a payment returned in full.
	t.Run("after a refund made in Stripe, then a void that returned credit", func(t *testing.T) {
		r := newOutsideRig(t, "Cap Reviewer Case")
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
		if _, err := r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireCents: 4446}); !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "retire_cents" {
			t.Errorf("retire 44.46 of credit: %v, want 422 on retire_cents: the documents allow 44.45", err)
		}
		// 44.45 of credit returns the last 40.00, so it would take all 80.00:
		// asked for less, the operator is told to send retire_all.
		if _, err := r.relief(creditnote.CommitReliefInput{InvoiceID: inv.ID, RetireCents: 4445}); !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "retire_cents" || !strings.Contains(err.Error(), "retire_all") {
			t.Errorf("retire 44.45 of credit, the last of the payment: %v, want 422 on retire_cents naming retire_all", err)
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
