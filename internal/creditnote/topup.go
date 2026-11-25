package creditnote

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// Every credit note on a paid top-up takes back the unused credit the money
// it records paid for (ADR-080): the operator's relief, which works out the
// cash from the credit, and money that went back outside Hardcount (a refund
// made in Stripe, a lost dispute), which works out the credit from the cash.
// Both run one core, issueNoteTx, whose steps each need the one before:
//
//  1. under the per-invoice credit-note lock, build takes the customer's
//     credit lock and the grant (holdTopupGrantTx) and works out the note's
//     total and the credit it takes back (topupHold);
//  2. the store numbers the note after build returns, then inserts it
//     (CreateUnderInvoiceLockDynamicTx);
//  3. the note is issued and the credit retired on the same transaction.
//
// The number row comes after the customer's lock, the order a top-up's
// payment keeps (invoice FOR UPDATE, customer lock, number row): numbering
// first would deadlock with the payment of the customer's other top-up.

// topupHold is a top-up's grant as a note on it reads it, under the
// customer's credit lock and the grant's row lock.
type topupHold struct {
	TopupGrant
	grossPaid int64 // what the top-up was paid
	unused    int64 // unusedCredit at the note's instant
}

// holdTopupGrantTx takes the customer's credit lock and the top-up's grant
// on tx, with the unused credit at the note's instant. It does not refuse an
// issued invoice still to draw on the grant (TopupGrant.PendingDraw); only
// relief does, since for money returned outside Hardcount the money has
// already gone back, and the invoice that was waiting to draw then goes to
// the card, which is right.
func (s *Service) holdTopupGrantTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, unbilledCents int64) (topupHold, error) {
	g, spendable, found, err := s.credits.LockTopupGrantAndBalanceTx(ctx, tx, tenantID, inv.ID, clock.Now(ctx))
	if err != nil {
		return topupHold{}, err
	}
	if !found {
		return topupHold{}, fmt.Errorf("invoice %s is a paid top-up with no funded grant: its payment funds the grant in the same transaction, so this is corrupted state; refusing to write the note", inv.ID)
	}
	return topupHold{TopupGrant: g, grossPaid: inv.TotalAmountCents, unused: unusedCredit(g, spendable, unbilledCents)}, nil
}

// cash is what r credit refunds at the price paid (topupNoteCash).
func (h topupHold) cash(r int64) int64 {
	return topupNoteCash(h.grossPaid, h.GrantedCents, h.CNRetiredCents, r)
}

// worth is the most credit cash buys back at the price paid: the largest r
// with cash(r) <= cash, found by halving, since cash(r) never falls as r
// grows.
func (h topupHold) worth(cash int64) int64 {
	lo, hi := int64(0), h.GrantedCents-h.CNRetiredCents
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if h.cash(mid) <= cash {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// takes is the credit a note of total takes back when it would take r and
// the payment's documents allow room: a note that uses up what they allow
// takes all the unused credit, whatever the price says, so no credit
// outlives a payment returned in full.
func (h topupHold) takes(r, total, room int64) int64 {
	if total >= room {
		return h.unused
	}
	return min(r, h.unused)
}

// takesBackFor is the credit a note for money returned outside Hardcount
// takes back: what amount buys at the price paid, or all the unused credit
// when amount uses up room, what the payment's documents still allow.
func (h topupHold) takesBackFor(amount, room int64) int64 {
	return h.takes(h.worth(amount), amount, room)
}

// issueNoteTx is the core: it inserts the note build returns on tx (build
// runs under the credit-note lock, and on a top-up takes the customer's
// lock), the store numbering it after build, issues it on tx, and retires the
// credit it takes back from the top-up's grant (CommitRetiredCents). The
// caller owns tx and writes the audit rows.
func (s *Service) issueNoteTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, build func(existing []domain.CreditNote) (domain.CreditNote, []domain.CreditNoteLineItem, error)) (domain.CreditNote, error) {
	// A lost dispute's note is told apart by the dispute naming it, which
	// happens after the insert, so the header's mark is carried here.
	var byBank bool
	cn, err := s.store.CreateUnderInvoiceLockDynamicTx(ctx, tx, tenantID, inv.ID, s.numbers, func(existing []domain.CreditNote) (domain.CreditNote, []domain.CreditNoteLineItem, error) {
		header, lines, err := build(existing)
		byBank = header.ReturnedByBank
		return header, lines, err
	})
	if err != nil {
		return domain.CreditNote{}, err
	}
	cn.ReturnedByBank = byBank
	return s.issueCreatedTx(ctx, tx, tenantID, inv, cn)
}

// issueCreatedTx issues, on tx, a note the same transaction created, and
// retires the credit it takes back (CommitRetiredCents) from the top-up's
// grant: the issue of every note that is issued as it is made (issueNoteTx,
// the linked create).
func (s *Service) issueCreatedTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, cn domain.CreditNote) (domain.CreditNote, error) {
	// Trivially won on a row this transaction created, but the one writer of
	// issued, and it stamps issued_at.
	won, err := s.store.TransitionStatusTx(ctx, tx, tenantID, cn.ID, domain.CreditNoteDraft, domain.CreditNoteIssued)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("issue credit note %s: %w", cn.CreditNoteNumber, err)
	}
	if !won {
		return domain.CreditNote{}, fmt.Errorf("credit note %s lost its issue CAS on the transaction that created it (impossible; investigate)", cn.ID)
	}
	cn.Status = domain.CreditNoteIssued
	if cn.CommitRetiredCents > 0 {
		if _, err := s.credits.RetireForTopupNoteTx(ctx, tx, tenantID, inv.ID, cn.ID, cn.CreditNoteNumber, cn.CommitRetiredCents, cn.TotalCents, inv.TotalAmountCents, cn.ReturnedByBank); err != nil {
			return domain.CreditNote{}, fmt.Errorf("take back the credit of credit note %s: %w", cn.CreditNoteNumber, err)
		}
	}
	return cn, nil
}

// noteSums adds up the totals and the tax of the notes on the invoice that
// are not voided: what its documents already return.
func noteSums(invoiceID string, notes []domain.CreditNote) (total, tax int64, err error) {
	for _, cn := range notes {
		if cn.Status == domain.CreditNoteVoided {
			continue
		}
		if total, err = domain.SumCents(total, cn.TotalCents); err == nil {
			tax, err = domain.SumCents(tax, cn.TaxAmountCents)
		}
		if err != nil {
			return 0, 0, fmt.Errorf("credit notes on invoice %s: %w", invoiceID, err)
		}
	}
	return total, tax, nil
}

// documentsRoom is what a payment's documents still leave to return: what
// was paid, less the totals of its notes not voided, less the refunds made in
// Stripe of the payment that no note holds, which returned money too. It is
// the one room relief, both notes for money returned outside Hardcount and
// the lost notice read, from notes and card read under the credit-note lock.
func documentsRoom(inv domain.Invoice, notes []domain.CreditNote, card cardRoom) (int64, error) {
	returned, _, err := noteSums(inv.ID, notes)
	if err != nil {
		return 0, err
	}
	return inv.TotalAmountCents - returned - card.StripeCents, nil
}

// DocumentsRoom is documentsRoom read on a transaction of its own, for
// display: the lost notice says whether a loss still has room to record.
func (s *Service) DocumentsRoom(ctx context.Context, tenantID string, inv domain.Invoice) (int64, error) {
	tx, err := s.store.BeginTx(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)
	notes, err := s.refunds.ListByInvoiceTx(ctx, tx, inv.ID)
	if err != nil {
		return 0, err
	}
	card, err := s.cardRoomTx(ctx, tx, inv, "")
	if err != nil {
		return 0, err
	}
	return documentsRoom(inv, notes, card)
}

// cardRoom is what the card that paid an invoice has left, read under its
// credit-note lock: the one figure every credit note's card refund fits
// (domain.CardRefundsSQL). It checks that a refund fits, one already made in
// Stripe included. lost says the bank won a dispute on the payment: a path
// that sends a refund (relief's card leg, an ordinary note's, the refund
// route, Retry refund) sends none then; a path that records one already made
// (the Stripe-refund recorder, the linked create) does.
type cardRoom struct {
	CardRefunds
	lost       bool
	refundable int64
}

// errNothingMoreToTheCard refuses a refund a path would send to the card
// after the bank won a dispute on the payment: Stripe refuses it
// (charge_disputed), and the customer already has the money. instead says
// what the operator does with the amount.
func errNothingMoreToTheCard(instead string) error {
	return errs.Invalid("refund_amount_cents",
		"The customer's bank reversed this payment in a dispute, so nothing more can go back to the card; "+instead)
}

// cardRoomTx reads the invoice's cardRoom on tx, leaving out the refund made
// in Stripe exceptRefundID, the one a caller is recording.
func (s *Service) cardRoomTx(ctx context.Context, tx *sql.Tx, inv domain.Invoice, exceptRefundID string) (cardRoom, error) {
	refunds, err := s.store.CardRefundsTx(ctx, tx, inv.ID, exceptRefundID)
	if err != nil {
		return cardRoom{}, err
	}
	_, lost, err := s.disputeStateTx(ctx, tx, inv.ID, inv.StripePaymentIntentID)
	if err != nil {
		return cardRoom{}, err
	}
	committed, err := domain.SumCents(refunds.NotesCents, refunds.StripeCents)
	if err != nil {
		return cardRoom{}, fmt.Errorf("card refunds of invoice %s: %w", inv.ID, err)
	}
	return cardRoom{CardRefunds: refunds, lost: lost, refundable: inv.CardRefundableCents(committed)}, nil
}

// cardRoom reads the invoice's cardRoom on a transaction of its own, for a
// figure checked again under the lock.
func (s *Service) cardRoom(ctx context.Context, tenantID string, inv domain.Invoice) (cardRoom, error) {
	tx, err := s.store.BeginTx(ctx, tenantID)
	if err != nil {
		return cardRoom{}, err
	}
	defer postgres.Rollback(tx)
	return s.cardRoomTx(ctx, tx, inv, "")
}

// disputeLossReason is the reason of the note that records a lost dispute.
const disputeLossReason = "Payment reversed in a dispute"

// errNothingToRecord stops a loss's note when the payment's documents
// already return all of it.
var errNothingToRecord = errors.New("the payment's credit notes already return all of it")

// TakeBackForLostDisputeTx records, on the dispute recorder's transaction, a
// dispute lost on a top-up's own payment (domain.InvoiceDispute
// .LossTakesCredit): one issued credit note for what the bank returned, no
// more than the payment's documents still allow, as money returned out of
// band (the bank returned it: not a card refund, and Hardcount emails it to
// no one), taking back the unused credit that money paid for. It returns the
// note's id, or "" when the documents have no room left. The caller holds the
// invoice row and the dispute row, and read unbilledCents (UnbilledUsageCents)
// before its transaction.
func (s *Service) TakeBackForLostDisputeTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, d domain.InvoiceDispute, unbilledCents int64) (string, error) {
	if s.credits == nil || s.numbers == nil {
		return "", fmt.Errorf("recording a lost dispute on a top-up is not wired: it needs the credit granter and the number generator")
	}
	ctx = clock.EnsureEffectiveNow(ctx, s.clock)
	cn, err := s.issueNoteTx(ctx, tx, tenantID, inv, func(existing []domain.CreditNote) (domain.CreditNote, []domain.CreditNoteLineItem, error) {
		card, err := s.cardRoomTx(ctx, tx, inv, "")
		if err != nil {
			return domain.CreditNote{}, nil, err
		}
		room, err := documentsRoom(inv, existing, card)
		if err != nil {
			return domain.CreditNote{}, nil, err
		}
		if room <= 0 {
			return domain.CreditNote{}, nil, errNothingToRecord
		}
		total := min(d.AmountCents, room)
		hold, err := s.holdTopupGrantTx(ctx, tx, tenantID, inv, unbilledCents)
		if err != nil {
			return domain.CreditNote{}, nil, err
		}
		header := domain.CreditNote{
			InvoiceID:            inv.ID,
			CustomerID:           inv.CustomerID,
			Status:               domain.CreditNoteDraft,
			Reason:               disputeLossReason,
			SubtotalCents:        total, // a top-up carries no tax
			TotalCents:           total,
			OutOfBandAmountCents: total,
			CommitRetiredCents:   hold.takesBackFor(total, room),
			Currency:             inv.Currency,
			RefundStatus:         domain.RefundNone,
			ReturnedByBank:       true,
		}
		line := domain.CreditNoteLineItem{
			Description:     "Payment reversed by the customer's bank (dispute " + d.StripeDisputeID + ")",
			Quantity:        1,
			UnitAmountCents: total,
			AmountCents:     total,
		}
		return header, []domain.CreditNoteLineItem{line}, nil
	})
	if errors.Is(err, errNothingToRecord) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("record lost dispute %s as a credit note: %w", d.StripeDisputeID, err)
	}
	facts := map[string]any{"origin": "dispute_lost", "stripe_dispute_id": d.StripeDisputeID}
	if s.audit != nil {
		// The create row states what the issued row states.
		md := map[string]any{
			"credit_note_number":       cn.CreditNoteNumber,
			"invoice_id":               cn.InvoiceID,
			"customer_id":              cn.CustomerID,
			"total_cents":              cn.TotalCents,
			"out_of_band_amount_cents": cn.OutOfBandAmountCents,
			"credit_taken_back_cents":  cn.CommitRetiredCents,
			"reason":                   cn.Reason,
			"currency":                 cn.Currency,
		}
		maps.Copy(md, facts)
		if err := s.audit.LogInTx(ctx, tx, audit.Entry{
			Action: domain.AuditActionCreate, ResourceType: "credit_note", ResourceID: cn.ID, ResourceLabel: cn.CreditNoteNumber, Metadata: md,
		}); err != nil {
			return "", fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := s.auditIssuedTx(ctx, tx, cn, facts); err != nil {
		return "", err
	}
	return cn.ID, nil
}

// UnbilledUsageCents is the customer's usage no invoice has billed yet in the
// invoice's currency (billing's figure), which a note on a top-up counts as
// credit already used. It is read before the note's transaction.
func (s *Service) UnbilledUsageCents(ctx context.Context, tenantID string, inv domain.Invoice) (int64, error) {
	if s.unbilled == nil {
		return 0, fmt.Errorf("the unbilled usage reader is not wired (call SetUnbilledUsageReader)")
	}
	byCurrency, err := s.unbilled.UnbilledUsage(ctx, tenantID, inv.CustomerID)
	if err != nil {
		return 0, fmt.Errorf("read the usage not invoiced yet: %w", err)
	}
	return byCurrency[strings.ToUpper(inv.Currency)], nil
}

// UnusedCredit is the credit the paid top-up invoiceID still has unused at
// `at` (unusedCredit), read with no lock, for display: the open-chargeback
// notice says how much the customer can still spend. 0 when the invoice
// funded no grant.
func (s *Service) UnusedCredit(ctx context.Context, tenantID, invoiceID string, at time.Time) (int64, error) {
	if s.credits == nil {
		return 0, fmt.Errorf("the credit granter is not wired")
	}
	inv, err := s.invoices.Get(ctx, tenantID, invoiceID)
	if err != nil {
		return 0, err
	}
	unbilled, err := s.UnbilledUsageCents(ctx, tenantID, inv)
	if err != nil {
		return 0, err
	}
	g, spendable, found, err := s.credits.TopupGrantAndBalance(ctx, tenantID, invoiceID, at)
	if err != nil || !found {
		return 0, err
	}
	return unusedCredit(g, spendable, unbilled), nil
}
