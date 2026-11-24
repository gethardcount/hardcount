package creditnote

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// Refunds made in Stripe outside Hardcount (ADR-134).
//
// An operator who refunds a Hardcount payment in the Stripe dashboard leaves
// Hardcount's books, the customer's documents and Stripe Tax saying the
// invoice was paid in full. RecordStripeRefund is the one writer that
// records such a refund: as an issued credit note whose card refund is that
// refund, when the refund is plainly a refund of the invoice's own payment,
// and otherwise as a flagged row the operator acts on.

// PaymentIntentInvoiceFinder finds the invoice a Stripe PaymentIntent paid or
// was charged for, on the caller's tx. invoice.PostgresStore implements it
// (the dispute lookup). errs.ErrNotFound when no invoice holds it.
type PaymentIntentInvoiceFinder interface {
	InvoiceForPaymentIntentTx(ctx context.Context, tx *sql.Tx, paymentIntentID string) (string, error)
}

// OutboxEnqueuer queues an outbound webhook event on the caller's tx
// (ADR-040). webhook.OutboxStore implements it.
type OutboxEnqueuer interface {
	Enqueue(ctx context.Context, tx *sql.Tx, tenantID, eventType string, payload map[string]any) (string, error)
}

// RefundReader reads one refund from Stripe, for the operator's link of a
// refund to a credit note at create: the link is made only for a refund
// Stripe says exists, on this invoice's payment, and not made by Hardcount.
type RefundReader interface {
	ReadRefund(ctx context.Context, refundID string) (domain.StripeRefundFacts, error)
}

// SetStripeRefundRecording wires RecordStripeRefund, the operator's link at
// create and the readers: the rows' store (the credit-note PostgresStore),
// the invoice lookup by PaymentIntent, and the outbox that carries
// payment.refunded_in_stripe. All three are required; the producer wires
// them together.
func (s *Service) SetStripeRefundRecording(store StripeRefundStore, invoices PaymentIntentInvoiceFinder, outbox OutboxEnqueuer) {
	s.refunds, s.piInvoices, s.outbox = store, invoices, outbox
}

// SetRefundReader wires the Stripe read the link at create makes. Without it
// a create that names stripe_refund_id is refused.
func (s *Service) SetRefundReader(r RefundReader) { s.refundReader = r }

// errRefundInvoiceNotPaidYet asks Stripe to deliver a refund event again: the
// refund is of a payment on an invoice Hardcount has not marked paid yet, and
// is young enough that the payment's own record may still be on its way (a
// hosted-page payment refunded at once).
var errRefundInvoiceNotPaidYet = errors.New("refund of a payment on an invoice not paid yet; the payment may not be recorded yet")

// RecordStripeRefund records one refund webhook's snapshot of a refund that
// does not carry hardcount_cn_id, in one transaction:
//
//   - the invoice is found from the refund's row, else from the credit note
//     that holds the refund id, else by the refund's PaymentIntent (the
//     invoice's charge attempts, its own payment, its anomaly marker). No
//     invoice holds it: errs.ErrNotFound, the charge was not Hardcount's;
//   - the invoice row is locked, then the per-invoice credit-note lock, then
//     the refund's row (lock order, playbook item 8), and every check
//     below reads under them;
//   - a credit note already holds the refund id (a refund Hardcount made
//     before refunds carried hardcount_cn_id, a note this recorded, or one the
//     operator linked or attached): its refund status moves monotonically,
//     as from any refund webhook, and the row, if any, keeps the newer
//     snapshot. Nothing else;
//   - the row is awaiting (first seen in flight) and this newer snapshot
//     says the refund succeeded, failed or was canceled: it is decided now,
//     as a first sighting would be, with the 24-hour rule measured from
//     Hardcount's first sighting;
//   - any other row keeps the newer snapshot. Nothing else: its outcome was
//     decided;
//   - first sighting: a refund still in flight gets an awaiting row and
//     nothing else; any other is decided (decideStripeRefund). A credit_note
//     outcome inserts and issues the note in this transaction, no draft ever
//     visible, with its refund already done. On a top-up the note also takes
//     back the unused credit the refund paid for (recordRefundCreditNoteTx);
//     the usage not invoiced yet is read before the transaction, and when it
//     cannot be read the refund is flagged credit_note_not_written instead,
//     for the linked create to record.
//
// A decision queues payment.refunded_in_stripe and writes the audit rows in
// its transaction. After the commit, a recorded note runs the existing
// external legs: the refund leg skips (the note holds the refund id, so
// Hardcount calls nothing at Stripe), and the tax reversal runs, recovered by
// its sweep if it fails. The invoice itself is never written. Nothing here
// starts dunning, writes the credit ledger or emails the customer.
func (s *Service) RecordStripeRefund(ctx context.Context, tenantID string, snap domain.StripeRefundSnapshot) (domain.StripeRefundRecordResult, error) {
	if s.refunds == nil || s.piInvoices == nil || s.outbox == nil {
		return domain.StripeRefundRecordResult{}, fmt.Errorf("recording refunds made in Stripe is not wired (call SetStripeRefundRecording)")
	}
	if snap.StripeRefundID == "" || snap.StripePaymentIntentID == "" {
		return domain.StripeRefundRecordResult{}, fmt.Errorf("refund snapshot needs a refund id and a payment intent id")
	}
	if !snap.Status.Valid() {
		return domain.StripeRefundRecordResult{}, fmt.Errorf("refund %s: unknown status %q", snap.StripeRefundID, snap.Status)
	}
	if snap.AmountCents <= 0 || strings.TrimSpace(snap.Currency) == "" {
		return domain.StripeRefundRecordResult{}, fmt.Errorf("refund %s has no amount or no currency", snap.StripeRefundID)
	}
	// The note on a top-up needs the usage not invoiced yet, read before the
	// transaction; only a succeeded refund can become a note.
	var unbilled int64
	var unbilledErr error
	if snap.Status == domain.StripeRefundSucceeded {
		unbilled, unbilledErr = s.unbilledForRefund(ctx, tenantID, snap)
	}

	tx, err := s.store.BeginTx(ctx, tenantID)
	if err != nil {
		return domain.StripeRefundRecordResult{}, err
	}
	defer postgres.Rollback(tx)

	invoiceID, err := s.refunds.InvoiceForStripeRefundTx(ctx, tx, snap.StripeRefundID)
	if errors.Is(err, errs.ErrNotFound) {
		invoiceID, err = s.piInvoices.InvoiceForPaymentIntentTx(ctx, tx, snap.StripePaymentIntentID)
	}
	if err != nil {
		return domain.StripeRefundRecordResult{}, err
	}
	// The note, its line, its issue and the tax stamp after the commit read
	// one instant.
	ctx = clock.EnsureEffectiveNow(ctx, s.clock)

	inv, err := s.invoices.LockForCreditNoteTx(ctx, tx, tenantID, invoiceID)
	if err != nil {
		return domain.StripeRefundRecordResult{}, fmt.Errorf("lock invoice %s: %w", invoiceID, err)
	}
	if err := s.store.LockInvoicesTx(ctx, tx, tenantID, []string{invoiceID}); err != nil {
		return domain.StripeRefundRecordResult{}, err
	}

	// Re-read under the locks: a concurrent delivery, or the operator's link,
	// may have recorded this refund since the lookup above.
	held, noteHolds, err := s.refunds.CreditNoteByStripeRefundTx(ctx, tx, snap.StripeRefundID)
	if err != nil {
		return domain.StripeRefundRecordResult{}, err
	}
	row, rowFound, err := s.refunds.LockStripeRefundTx(ctx, tx, snap.StripeRefundID)
	if err != nil {
		return domain.StripeRefundRecordResult{}, err
	}
	decideNow := !noteHolds && rowFound && row.Outcome == domain.StripeRefundAwaiting &&
		!snap.Status.InFlight() && row.SupersededBy(snap)
	if (noteHolds || rowFound) && !decideNow {
		res := domain.StripeRefundRecordResult{}
		if rowFound {
			if row.SupersededBy(snap) {
				if row, err = s.refunds.UpdateStripeRefundSnapshotTx(ctx, tx, row.ID, snap); err != nil {
					return domain.StripeRefundRecordResult{}, err
				}
				res.Applied = true
			}
			res.Refund = &row
		}
		if noteHolds {
			res.CreditNoteID = held.ID
			flipped, err := s.applyNoteRefundStatusTx(ctx, tx, snap)
			if err != nil {
				return domain.StripeRefundRecordResult{}, err
			}
			res.Applied = res.Applied || flipped
		}
		if err := tx.Commit(); err != nil {
			return domain.StripeRefundRecordResult{}, err
		}
		return res, nil
	}

	var prior *domain.InvoiceStripeRefund
	firstSeen := snap.EventAt
	if decideNow {
		prior, firstSeen = &row, row.FirstEventAt
	}
	rec := domain.InvoiceStripeRefund{
		InvoiceID:             invoiceID,
		StripeRefundID:        snap.StripeRefundID,
		StripePaymentIntentID: snap.StripePaymentIntentID,
		StripeChargeID:        snap.StripeChargeID,
		AmountCents:           snap.AmountCents,
		Currency:              snap.Currency,
		Status:                snap.Status,
		Reason:                snap.Reason,
		FailureReason:         snap.FailureReason,
		RefundCreatedAt:       snap.CreatedAt,
		FirstEventAt:          firstSeen,
		LastEventAt:           snap.EventAt,
		LastStripeEventID:     snap.StripeEventID,
	}
	if rec.RefundCreatedAt.IsZero() {
		rec.RefundCreatedAt = snap.EventAt
	}

	// First seen in flight: wait for the refund's outcome. Nothing is
	// recorded or sent until a later event decides it.
	if prior == nil && snap.Status.InFlight() {
		rec.Outcome = domain.StripeRefundAwaiting
		if rec, err = s.refunds.InsertStripeRefundTx(ctx, tx, tenantID, rec); err != nil {
			return domain.StripeRefundRecordResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return domain.StripeRefundRecordResult{}, err
		}
		return domain.StripeRefundRecordResult{Refund: &rec, Applied: true}, nil
	}

	existing, err := s.refunds.ListByInvoiceTx(ctx, tx, invoiceID)
	if err != nil {
		return domain.StripeRefundRecordResult{}, err
	}
	card, err := s.cardRoomTx(ctx, tx, inv, snap.StripeRefundID)
	if err != nil {
		return domain.StripeRefundRecordResult{}, err
	}
	d, err := decideStripeRefund(inv, snap, firstSeen, existing, card)
	if err != nil {
		return domain.StripeRefundRecordResult{}, err
	}
	if d.retry {
		return domain.StripeRefundRecordResult{}, fmt.Errorf("refund %s on invoice %s (%s, payment %s): %w",
			snap.StripeRefundID, invoiceID, inv.Status, inv.PaymentStatus, errRefundInvoiceNotPaidYet)
	}
	if d.outcome == domain.StripeRefundRecordedAsCreditNote && inv.BillingReason == domain.BillingReasonTopup && unbilledErr != nil {
		// The refund is recorded whatever the credit says; its note waits for
		// the operator's linked create, and the notice is Critical.
		slog.ErrorContext(ctx, "refund made in Stripe on a top-up recorded without its credit note: the usage not invoiced yet could not be read, so the credit the refund paid for could not be worked out; record it with a credit note linked to the refund",
			"invoice_id", invoiceID, "stripe_refund_id", snap.StripeRefundID, "error", unbilledErr)
		d = stripeRefundDecision{outcome: domain.StripeRefundNeedsReview, reviewReason: domain.StripeRefundReviewCreditNoteNotWritten}
	}
	rec.Outcome, rec.ReviewReason = d.outcome, d.reviewReason

	var cn domain.CreditNote
	if d.outcome == domain.StripeRefundRecordedAsCreditNote {
		if cn, err = s.recordRefundCreditNoteTx(ctx, tx, tenantID, inv, snap, unbilled); err != nil {
			return domain.StripeRefundRecordResult{}, err
		}
		rec.CreditNoteID = cn.ID
	}
	if prior == nil {
		rec, err = s.refunds.InsertStripeRefundTx(ctx, tx, tenantID, rec)
	} else {
		rec, err = s.refunds.DecideStripeRefundTx(ctx, tx, prior.ID, snap, rec.Outcome, rec.ReviewReason, rec.CreditNoteID)
	}
	if err != nil {
		return domain.StripeRefundRecordResult{}, err
	}
	// The event carries the invoice as locked: the refund leaves it as it was
	// (ADR-131). It is sent once per refund, when its outcome is decided.
	if _, err := s.outbox.Enqueue(ctx, tx, tenantID, domain.EventPaymentRefundedInStripe,
		domain.InvoiceWebhookData(inv, map[string]any{"refund": rec})); err != nil {
		return domain.StripeRefundRecordResult{}, fmt.Errorf("enqueue %s: %w", domain.EventPaymentRefundedInStripe, err)
	}
	if s.audit != nil {
		if err := s.audit.LogInTx(ctx, tx, audit.Entry{
			Action:        domain.AuditActionUpdate,
			ResourceType:  "invoice",
			ResourceID:    inv.ID,
			ResourceLabel: inv.InvoiceNumber,
			Metadata: withoutEmpty(map[string]any{
				"action":                   "refunded_in_stripe",
				"stripe_refund_id":         rec.StripeRefundID,
				"stripe_payment_intent_id": rec.StripePaymentIntentID,
				"amount_cents":             rec.AmountCents,
				"currency":                 rec.Currency,
				"stripe_status":            string(rec.Status),
				"outcome":                  string(rec.Outcome),
				"review_reason":            string(rec.ReviewReason),
				"credit_note_id":           rec.CreditNoteID,
				"customer_id":              inv.CustomerID,
			}),
		}); err != nil {
			return domain.StripeRefundRecordResult{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.StripeRefundRecordResult{}, err
	}

	res := domain.StripeRefundRecordResult{Refund: &rec, CreditNoteID: cn.ID, Recorded: true, Applied: true}
	if cn.ID != "" {
		// The note holds the refund id, so the refund leg makes no Stripe
		// call; the tax reversal runs, and its sweep recovers a failure.
		if _, err := s.runPostIssueExternalLegs(ctx, tenantID, cn.ID, cn, inv); err != nil {
			slog.WarnContext(ctx, "refund made in Stripe recorded as a credit note; reading it back after its tax reversal failed",
				"credit_note_id", cn.ID, "stripe_refund_id", rec.StripeRefundID, "error", err)
		}
	}
	return res, nil
}

// stripeRefundDecision is what decideStripeRefund picked for a refund whose
// outcome is decided now. retry asks Stripe to deliver the event again.
type stripeRefundDecision struct {
	outcome      domain.StripeRefundOutcome
	reviewReason domain.StripeRefundReviewReason
	retry        bool
}

// decideStripeRefund picks what a refund does once Stripe says how it ended
// (ADR-134). inv is the invoice as locked, existing its credit notes under the
// credit-note lock, card what can still go back to its card besides this
// refund, firstSeen the creation time of the first event Hardcount saw for
// the refund. The refund becomes a credit note only when all of these hold:
//
//  1. it succeeded (a refund still in flight waits; failed or canceled moved
//     no money);
//  2. its PaymentIntent is the invoice's own payment, not a second charge or
//     money on a voided invoice;
//  3. the invoice is paid, with payment status succeeded;
//  4. its currency is the invoice's;
//  5. Hardcount first saw it soon enough after its creation
//     (domain.StripeRefundSeenLate);
//  6. no credit note on the invoice has a card refund that failed (the
//     refund may replace it, and only the operator can say);
//  7. it fits both caps: what can still go back to the card (cardRoom), and
//     what the invoice has left to credit.
//
// A top-up is no exception: its note also takes back the credit the refund
// paid for (recordRefundCreditNoteTx). A refund of the invoice's own payment
// that fails 3 while it is young asks for redelivery: the payment may be
// recorded in a moment.
func decideStripeRefund(inv domain.Invoice, snap domain.StripeRefundSnapshot, firstSeen time.Time, existing []domain.CreditNote, card cardRoom) (stripeRefundDecision, error) {
	review := func(r domain.StripeRefundReviewReason) (stripeRefundDecision, error) {
		return stripeRefundDecision{outcome: domain.StripeRefundNeedsReview, reviewReason: r}, nil
	}
	own := snap.StripePaymentIntentID == inv.StripePaymentIntentID
	paid := inv.Status == domain.InvoicePaid && inv.PaymentStatus == domain.PaymentSucceeded
	failedNote := false
	for _, cn := range existing {
		if cn.Status != domain.CreditNoteVoided && cn.RefundAmountCents > 0 && cn.RefundStatus == domain.RefundFailed {
			failedNote = true
		}
	}
	switch {
	case snap.Status.MovedNoMoney():
		return stripeRefundDecision{outcome: domain.StripeRefundNotRefunded}, nil
	case snap.Status != domain.StripeRefundSucceeded:
		return stripeRefundDecision{outcome: domain.StripeRefundAwaiting}, nil
	case !own && (paid || inv.Status == domain.InvoiceVoided || snap.StripePaymentIntentID == inv.PaymentAnomalyPaymentIntentID):
		return stripeRefundDecision{outcome: domain.StripeRefundOtherPayment}, nil
	case !paid:
		if snap.Young {
			return stripeRefundDecision{retry: true}, nil
		}
		return review(domain.StripeRefundReviewInvoiceNotPaid)
	case !strings.EqualFold(snap.Currency, inv.Currency):
		return review(domain.StripeRefundReviewCurrencyMismatch)
	case domain.StripeRefundSeenLate(snap.CreatedAt, firstSeen):
		return review(domain.StripeRefundReviewSeenLate)
	case failedNote:
		return review(domain.StripeRefundReviewCreditNoteRefundFailed)
	}
	credited, _, err := noteSums(inv.ID, existing)
	if err != nil {
		return stripeRefundDecision{}, err
	}
	if snap.AmountCents > card.refundable {
		return review(domain.StripeRefundReviewExceedsCardRefundable)
	}
	if snap.AmountCents > inv.TotalAmountCents-credited { // never credited+amount: that sum can wrap
		return review(domain.StripeRefundReviewExceedsCreditable)
	}
	return stripeRefundDecision{outcome: domain.StripeRefundRecordedAsCreditNote}, nil
}

// stripeRefundCreditNoteReason is the reason a recorded note carries:
// "Refunded in Stripe", with Stripe's reason when the operator gave one.
func stripeRefundCreditNoteReason(stripeReason string) string {
	r := strings.TrimSpace(strings.ReplaceAll(stripeReason, "_", " "))
	if r == "" {
		return "Refunded in Stripe"
	}
	return "Refunded in Stripe: " + r
}

// recordRefundCreditNoteTx inserts and issues, on tx, the credit note that
// records a refund made in Stripe: one gross line for the refund, the
// proportional tax and last-note true-up of buildCreditNote, all of it
// allocated to the card refund, which is this refund (its id and Stripe's
// status set, recorded_from_stripe). The note is issued in the transaction
// that inserts it, like commit relief, so no draft is ever visible. On a
// top-up it also takes back the unused credit the refund's amount paid for,
// in the core's lock order (issueNoteTx); unbilledCents is the usage not
// invoiced yet, read before the transaction. The caller holds the invoice row
// and the credit-note lock.
func (s *Service) recordRefundCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, snap domain.StripeRefundSnapshot, unbilledCents int64) (domain.CreditNote, error) {
	amount := snap.AmountCents
	input := CreateInput{
		InvoiceID:         inv.ID,
		Reason:            stripeRefundCreditNoteReason(snap.Reason),
		RefundAmountCents: amount,
	}
	line := domain.CreditNoteLineItem{
		Description:     "Refund made in Stripe (" + snap.StripeRefundID + ")",
		Quantity:        1,
		UnitAmountCents: amount,
		AmountCents:     amount,
	}
	cn, err := s.issueNoteTx(ctx, tx, tenantID, inv, func(existing []domain.CreditNote) (domain.CreditNote, []domain.CreditNoteLineItem, error) {
		existingTotal, existingTax, err := noteSums(inv.ID, existing)
		if err != nil {
			return domain.CreditNote{}, nil, err
		}
		card, err := s.cardRoomTx(ctx, tx, inv, snap.StripeRefundID)
		if err != nil {
			return domain.CreditNote{}, nil, err
		}
		cn, err := buildCreditNote(input, inv, amount, existingTotal, existingTax, card)
		if err != nil {
			return domain.CreditNote{}, nil, err
		}
		cn.StripeRefundID = snap.StripeRefundID
		cn.RefundStatus = snap.Status.CreditNoteRefundStatus()
		cn.RecordedFromStripe = true
		if inv.BillingReason == domain.BillingReasonTopup {
			hold, err := s.holdTopupGrantTx(ctx, tx, tenantID, inv, unbilledCents)
			if err != nil {
				return domain.CreditNote{}, nil, err
			}
			cn.CommitRetiredCents = hold.takes(hold.worth(amount), amount, inv.TotalAmountCents-existingTotal)
		}
		return cn, []domain.CreditNoteLineItem{line}, nil
	})
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("record refund %s as a credit note: %w", snap.StripeRefundID, err)
	}
	if s.audit != nil {
		for _, e := range []audit.Entry{{
			Action: domain.AuditActionCreate,
			Metadata: map[string]any{
				"invoice_id":       cn.InvoiceID,
				"customer_id":      cn.CustomerID,
				"total_cents":      cn.TotalCents,
				"reason":           cn.Reason,
				"currency":         cn.Currency,
				"origin":           "stripe_refund",
				"stripe_refund_id": snap.StripeRefundID,
			},
		}, {
			Action: "credit_note.issued",
			Metadata: map[string]any{
				"credit_note_number":       cn.CreditNoteNumber,
				"invoice_id":               cn.InvoiceID,
				"customer_id":              cn.CustomerID,
				"total_cents":              cn.TotalCents,
				"refund_amount_cents":      cn.RefundAmountCents,
				"credit_amount_cents":      cn.CreditAmountCents,
				"out_of_band_amount_cents": cn.OutOfBandAmountCents,
				"credit_taken_back_cents":  cn.CommitRetiredCents,
				"reason":                   cn.Reason,
				"currency":                 cn.Currency,
				"origin":                   "stripe_refund",
				"stripe_refund_id":         snap.StripeRefundID,
				"refund_status":            string(cn.RefundStatus),
			},
		}} {
			e.ResourceType, e.ResourceID, e.ResourceLabel = "credit_note", cn.ID, cn.CreditNoteNumber
			if err := s.audit.LogInTx(ctx, tx, e); err != nil {
				return domain.CreditNote{}, fmt.Errorf("audit emission: %w", err)
			}
		}
	}
	return cn, nil
}

// unbilledForRefund reads, before RecordStripeRefund's transaction, the
// usage not invoiced yet of the customer whose top-up the refund is on: what
// a note on a top-up counts as credit already used. 0 for a refund on any
// other invoice, or on none: the transaction looks it up again.
func (s *Service) unbilledForRefund(ctx context.Context, tenantID string, snap domain.StripeRefundSnapshot) (int64, error) {
	tx, err := s.store.BeginTx(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	invoiceID, err := s.refunds.InvoiceForStripeRefundTx(ctx, tx, snap.StripeRefundID)
	if errors.Is(err, errs.ErrNotFound) {
		invoiceID, err = s.piInvoices.InvoiceForPaymentIntentTx(ctx, tx, snap.StripePaymentIntentID)
	}
	postgres.Rollback(tx)
	if err != nil {
		return 0, nil
	}
	inv, err := s.invoices.Get(ctx, tenantID, invoiceID)
	if err != nil || inv.BillingReason != domain.BillingReasonTopup {
		return 0, nil
	}
	return s.UnbilledUsageCents(ctx, tenantID, inv)
}

// applyNoteRefundStatusTx moves the refund status of the credit note holding
// the refund, with ApplyRefundWebhook's monotonic guard and audit row, on tx.
func (s *Service) applyNoteRefundStatusTx(ctx context.Context, tx *sql.Tx, snap domain.StripeRefundSnapshot) (bool, error) {
	status := snap.Status.CreditNoteRefundStatus()
	cn, flipped, _, err := s.refunds.ApplyRefundWebhookStatusTx(ctx, tx, snap.StripeRefundID, status)
	if err != nil {
		return false, fmt.Errorf("apply refund status: %w", err)
	}
	if flipped && s.audit != nil {
		if err := s.audit.LogInTx(ctx, tx, audit.Entry{
			Action:        domain.AuditActionUpdate,
			ResourceType:  "credit_note",
			ResourceID:    cn.ID,
			ResourceLabel: cn.CreditNoteNumber,
			Metadata: withoutEmpty(map[string]any{
				"action":                  "refund_status_changed",
				"refund_status":           string(status),
				"stripe_refund_id":        snap.StripeRefundID,
				"invoice_id":              cn.InvoiceID,
				"customer_id":             cn.CustomerID,
				"provider_failure_reason": snap.FailureReason,
			}),
		}); err != nil {
			return false, fmt.Errorf("audit emission: %w", err)
		}
	}
	return flipped, nil
}

// maxDismissNoteLen bounds the operator's note on a dismissed refund.
const maxDismissNoteLen = 1000

// DismissStripeRefund records that the operator handled a refund flagged for
// review outside Hardcount, with their note. Only a needs_review refund can
// be dismissed; the attention notice clears. The row and its audit row
// commit together. The refund is named by its Stripe id, on the invoice the
// caller names.
func (s *Service) DismissStripeRefund(ctx context.Context, tenantID, invoiceID, stripeRefundID, note string) (domain.InvoiceStripeRefund, error) {
	if s.refunds == nil {
		return domain.InvoiceStripeRefund{}, fmt.Errorf("recording refunds made in Stripe is not wired (call SetStripeRefundRecording)")
	}
	note = strings.TrimSpace(note)
	if note == "" {
		return domain.InvoiceStripeRefund{}, errs.Required("note")
	}
	if utf8.RuneCountInString(note) > maxDismissNoteLen {
		return domain.InvoiceStripeRefund{}, errs.Invalid("note", fmt.Sprintf("must be at most %d characters", maxDismissNoteLen))
	}
	tx, err := s.store.BeginTx(ctx, tenantID)
	if err != nil {
		return domain.InvoiceStripeRefund{}, err
	}
	defer postgres.Rollback(tx)

	row, found, err := s.refunds.LockStripeRefundTx(ctx, tx, stripeRefundID)
	if err != nil {
		return domain.InvoiceStripeRefund{}, err
	}
	if !found || row.InvoiceID != invoiceID {
		return domain.InvoiceStripeRefund{}, errs.ErrNotFound
	}
	if row, err = s.refunds.DismissStripeRefundTx(ctx, tx, row.ID, note, clock.Now(ctx)); err != nil {
		return domain.InvoiceStripeRefund{}, err
	}
	if s.audit != nil {
		if err := s.audit.LogInTx(ctx, tx, audit.Entry{
			Action:       domain.AuditActionUpdate,
			ResourceType: "invoice",
			ResourceID:   invoiceID,
			Metadata: map[string]any{
				"action":           "stripe_refund_dismissed",
				"stripe_refund_id": row.StripeRefundID,
				"amount_cents":     row.AmountCents,
				"currency":         row.Currency,
				"review_reason":    string(row.ReviewReason),
				"note":             note,
			},
		}); err != nil {
			return domain.InvoiceStripeRefund{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.InvoiceStripeRefund{}, err
	}
	return row, nil
}

// ListStripeRefunds returns the refunds made in Stripe outside Hardcount on
// the invoice's payments, oldest first. Empty when recording is not wired.
func (s *Service) ListStripeRefunds(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceStripeRefund, error) {
	if s.refunds == nil {
		return []domain.InvoiceStripeRefund{}, nil
	}
	return s.refunds.ListStripeRefundsByInvoice(ctx, tenantID, invoiceID)
}

// StripeRefundsNeedingReview returns, for a page of invoices, the newest
// refund made in Stripe on each that still needs the operator: invoice id →
// row. The refunded_in_stripe attention notice reads it.
func (s *Service) StripeRefundsNeedingReview(ctx context.Context, tenantID string, invoiceIDs []string) (map[string]domain.InvoiceStripeRefund, error) {
	if s.refunds == nil {
		return map[string]domain.InvoiceStripeRefund{}, nil
	}
	return s.refunds.StripeRefundsNeedingReviewByInvoices(ctx, tenantID, invoiceIDs)
}

// linkedStripeRefund is a refund the operator links to the credit note they
// create (POST /v1/credit-notes with stripe_refund_id), checked against
// Stripe before the create.
type linkedStripeRefund struct {
	facts domain.StripeRefundFacts
}

// checkLinkedStripeRefund reads the refund an operator's create names from
// Stripe and checks it can be this note's card refund: it exists, it is a
// refund of this invoice's own payment, Hardcount did not make it, it moved
// money, and the note sends exactly its amount to the card and none to the
// credit balance. The checks that read Hardcount's own records (no note holds
// it yet, its row allows it) run under the credit-note lock in create.
func (s *Service) checkLinkedStripeRefund(ctx context.Context, tenantID string, input CreateInput) (*linkedStripeRefund, error) {
	if s.refundReader == nil || s.refunds == nil {
		return nil, errs.InvalidState("Stripe is not connected, so a refund made in Stripe cannot be linked")
	}
	// The refund is already made, so its note is issued at once: a draft
	// holding a refund id could never be voided (Void keeps an executed
	// refund in the cap).
	if !input.AutoIssue {
		return nil, errs.Invalid("auto_issue", "must be true with stripe_refund_id: the refund is already made, so its credit note is issued at once")
	}
	inv, err := s.invoices.Get(ctx, tenantID, input.InvoiceID)
	if err != nil {
		return nil, fmt.Errorf("invoice not found: %w", err)
	}
	if inv.Status != domain.InvoicePaid || inv.PaymentStatus != domain.PaymentSucceeded {
		return nil, errs.Invalid("stripe_refund_id", "a refund made in Stripe can be linked only on a paid invoice")
	}
	facts, err := s.refundReader.ReadRefund(ctx, input.StripeRefundID)
	if err != nil {
		return nil, err
	}
	if err := checkStripeRefundFacts(facts, inv); err != nil {
		return nil, err
	}
	switch {
	case input.RefundType != "":
		return nil, errs.Invalid("refund_type", "use refund_amount_cents with stripe_refund_id")
	case input.RefundAmountCents != facts.AmountCents:
		return nil, errs.Invalid("refund_amount_cents", fmt.Sprintf(
			"must be the refund's amount, %s: the card part of this note is that refund", domain.FormatMoneyMinor(facts.AmountCents, inv.Currency)))
	case input.CreditAmountCents != 0:
		return nil, errs.Invalid("credit_amount_cents", "a note that records a refund made in Stripe sends nothing to the credit balance; issue a separate credit note for credit")
	}
	return &linkedStripeRefund{facts: facts}, nil
}

// checkStripeRefundFacts is what a refund read from Stripe must be for the
// operator to link or attach it: a refund of the invoice's own payment, not
// made by Hardcount, succeeded (a pending one is recorded by Hardcount when
// it succeeds), in the invoice's currency.
func checkStripeRefundFacts(facts domain.StripeRefundFacts, inv domain.Invoice) error {
	switch {
	case facts.HardcountCreditNoteID != "":
		return errs.Invalid("stripe_refund_id", "Hardcount made this refund for credit note "+facts.HardcountCreditNoteID+"; it is not a refund made in Stripe")
	case facts.PaymentIntentID == "" || facts.PaymentIntentID != inv.StripePaymentIntentID:
		return errs.Invalid("stripe_refund_id", "this refund is not of the payment that paid this invoice")
	case facts.Status.MovedNoMoney():
		return errs.Invalid("stripe_refund_id", "this refund "+string(facts.Status)+" at Stripe, so no money went back to the customer")
	case facts.Status != domain.StripeRefundSucceeded:
		return errs.Invalid("stripe_refund_id", "this refund is still "+string(facts.Status)+" at Stripe; Hardcount records it once it succeeds")
	case !strings.EqualFold(facts.Currency, inv.Currency):
		return errs.Invalid("stripe_refund_id", fmt.Sprintf("this refund is in %s and the invoice in %s", strings.ToUpper(facts.Currency), inv.Currency))
	}
	return nil
}

// linkUnderLockTx runs in create's build, under the credit-note lock: no note
// on the invoice holds the refund yet, and its row, if any, is one the
// operator may still record. It returns the row to link once the note is
// inserted, nil when there is none.
func (s *Service) linkUnderLockTx(ctx context.Context, tx *sql.Tx, invoiceID string, link *linkedStripeRefund, existingCNs []domain.CreditNote) (*domain.InvoiceStripeRefund, error) {
	id := link.facts.ID
	for _, cn := range existingCNs {
		if cn.StripeRefundID == id {
			return nil, errs.InvalidState("credit note " + cn.CreditNoteNumber + " already records this refund")
		}
	}
	row, found, err := s.refunds.LockStripeRefundTx(ctx, tx, id)
	if err != nil || !found {
		return nil, err
	}
	if row.InvoiceID != invoiceID {
		return nil, errs.Invalid("stripe_refund_id", "this refund was recorded against another invoice")
	}
	switch row.Outcome {
	case domain.StripeRefundNeedsReview, domain.StripeRefundDismissed:
		return &row, nil
	case domain.StripeRefundAwaiting:
		return nil, errs.InvalidState("this refund is still in flight at Stripe; Hardcount records it once it succeeds")
	case domain.StripeRefundOtherPayment:
		return nil, errs.Invalid("stripe_refund_id", "this refund is of a payment that did not pay this invoice")
	case domain.StripeRefundNotRefunded:
		return nil, errs.Invalid("stripe_refund_id", "this refund moved no money")
	default:
		return nil, errs.InvalidState("this refund is already recorded as a credit note")
	}
}

// createAndIssueLinked creates and issues, in ONE transaction, the credit
// note an operator links to a refund made in Stripe (POST /v1/credit-notes
// with stripe_refund_id). Its card refund is that refund, already done, so
// no draft may ever hold it: a draft holding a refund id cannot be voided,
// and a failed issue would leave the refund hidden in it while its flagged
// row read recorded. Here the note, its issue, the row's link and the audit
// rows commit together or not at all. Locks: the invoice row, then the
// credit-note lock (in create), then the refund's row, as RecordStripeRefund
// takes them. After the commit the shared external legs run: no refund call
// (the note holds the refund id), and the tax reversal.
func (s *Service) createAndIssueLinked(ctx context.Context, tenantID string, input CreateInput) (domain.CreditNote, error) {
	// On a top-up the note takes back the credit the refund paid for, which
	// needs the usage not invoiced yet, read before the transaction.
	var unbilled int64
	if pre, err := s.invoices.Get(ctx, tenantID, input.InvoiceID); err == nil && pre.BillingReason == domain.BillingReasonTopup {
		if unbilled, err = s.UnbilledUsageCents(ctx, tenantID, pre); err != nil {
			return domain.CreditNote{}, err
		}
	}
	tx, err := s.store.BeginTx(ctx, tenantID)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("begin linked credit note tx: %w", err)
	}
	defer postgres.Rollback(tx)
	inv, err := s.invoices.LockForCreditNoteTx(ctx, tx, tenantID, input.InvoiceID)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("lock invoice: %w", err)
	}
	if inv.Status != domain.InvoicePaid || inv.PaymentStatus != domain.PaymentSucceeded {
		return domain.CreditNote{}, errs.Invalid("stripe_refund_id", "a refund made in Stripe can be linked only on a paid invoice")
	}
	var cn domain.CreditNote
	if inv.BillingReason == domain.BillingReasonTopup {
		cn, err = s.linkedTopupNoteTx(ctx, tx, tenantID, inv, input, unbilled)
	} else {
		cn, err = s.create(ctx, tenantID, input, tx)
		if err == nil {
			cn, err = s.issueDraftTx(ctx, tx, tenantID, cn)
		}
	}
	if err != nil {
		return domain.CreditNote{}, err
	}
	// The note sends nothing to the credit balance (checkLinkedStripeRefund),
	// so issuing it grants nothing and changes no amount due; on a top-up it
	// took its credit back on this transaction (issueNoteTx).
	if err := s.emitIssuedTx(ctx, tx, cn, 0, 0, false); err != nil {
		return domain.CreditNote{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.CreditNote{}, fmt.Errorf("commit linked credit note: %w", err)
	}
	return s.runPostIssueExternalLegs(ctx, tenantID, cn.ID, cn, inv)
}

// issueDraftTx issues, on tx, a draft the same transaction created.
func (s *Service) issueDraftTx(ctx context.Context, tx *sql.Tx, tenantID string, cn domain.CreditNote) (domain.CreditNote, error) {
	won, err := s.store.TransitionStatusTx(ctx, tx, tenantID, cn.ID, domain.CreditNoteDraft, domain.CreditNoteIssued)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("issue the linked credit note: %w", err)
	}
	if !won {
		return domain.CreditNote{}, fmt.Errorf("the linked credit note %s lost its issue CAS on the transaction that created it (impossible; investigate)", cn.ID)
	}
	cn.Status = domain.CreditNoteIssued
	return cn, nil
}

// linkedTopupNoteTx writes and issues, on tx, the note an operator links to a
// refund made in Stripe on a top-up: the note create would write, built and
// checked the same way, which also takes back the unused credit its total
// paid for. It runs the core (issueNoteTx), so its number comes after the
// customer's lock; create numbers before it, which is right only where no
// customer lock follows. create's refusal of a top-up stays for an unlinked
// note.
func (s *Service) linkedTopupNoteTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, input CreateInput, unbilledCents int64) (domain.CreditNote, error) {
	if strings.TrimSpace(input.Reason) == "" {
		return domain.CreditNote{}, errs.Required("reason")
	}
	if err := checkLines(input.Lines); err != nil {
		return domain.CreditNote{}, err
	}
	lines, subtotal, err := noteLines(input, func() ([]domain.InvoiceLineItem, error) {
		return s.invoices.ListLineItemsTx(ctx, tx, tenantID, inv.ID)
	})
	if err != nil {
		return domain.CreditNote{}, err
	}
	var linkRow *domain.InvoiceStripeRefund
	cn, err := s.issueNoteTx(ctx, tx, tenantID, inv, func(existing []domain.CreditNote) (domain.CreditNote, []domain.CreditNoteLineItem, error) {
		var err error
		if linkRow, err = s.linkUnderLockTx(ctx, tx, inv.ID, input.linked, existing); err != nil {
			return domain.CreditNote{}, nil, err
		}
		existingTotal, existingTax, err := noteSums(inv.ID, existing)
		if err != nil {
			return domain.CreditNote{}, nil, err
		}
		if err := fitsCreditable(inv, subtotal, existingTotal); err != nil {
			return domain.CreditNote{}, nil, err
		}
		card, err := s.cardRoomTx(ctx, tx, inv, input.linked.facts.ID)
		if err != nil {
			return domain.CreditNote{}, nil, err
		}
		cn, err := buildCreditNote(input, inv, subtotal, existingTotal, existingTax, card)
		if err != nil {
			return domain.CreditNote{}, nil, err
		}
		cn.StripeRefundID = input.linked.facts.ID
		cn.RefundStatus = input.linked.facts.Status.CreditNoteRefundStatus()
		cn.RecordedFromStripe = true
		hold, err := s.holdTopupGrantTx(ctx, tx, tenantID, inv, unbilledCents)
		if err != nil {
			return domain.CreditNote{}, nil, err
		}
		cn.CommitRetiredCents = hold.takes(hold.worth(subtotal), subtotal, inv.TotalAmountCents-existingTotal)
		return cn, lines, nil
	})
	if err != nil {
		return domain.CreditNote{}, err
	}
	if linkRow != nil {
		if _, err := s.refunds.LinkStripeRefundTx(ctx, tx, linkRow.ID, cn.ID); err != nil {
			return domain.CreditNote{}, err
		}
	}
	if s.audit != nil {
		if err := s.audit.LogInTx(ctx, tx, audit.Entry{
			Action:        domain.AuditActionCreate,
			ResourceType:  "credit_note",
			ResourceID:    cn.ID,
			ResourceLabel: cn.CreditNoteNumber,
			Metadata: map[string]any{
				"invoice_id":              cn.InvoiceID,
				"customer_id":             cn.CustomerID,
				"total_cents":             cn.TotalCents,
				"reason":                  cn.Reason,
				"currency":                cn.Currency,
				"stripe_refund_id":        cn.StripeRefundID,
				"credit_taken_back_cents": cn.CommitRetiredCents,
			},
		}); err != nil {
			return domain.CreditNote{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	return cn, nil
}

// blockedAction words refundBlockedByStripeRefund's refusal for the action it
// guards: what to do once the refund made in Stripe is settled or decided.
type blockedAction struct{ unrecorded, inFlight string }

var (
	blockRetry = blockedAction{
		unrecorded: "If it replaces this refund, attach it to this credit note; otherwise record it or dismiss it. Then retry. Retrying now could refund the customer twice.",
		inFlight:   "Wait for Stripe to settle it, then retry. Retrying now could refund the customer twice.",
	}
	blockMark = blockedAction{
		unrecorded: "If it replaces this refund, attach it to this credit note; otherwise record it or dismiss it. Then mark this refund as returned another way if it still is: that refund may be its money.",
		inFlight:   "Wait for Stripe to settle it before you mark this refund as returned another way: it may be its money.",
	}
	blockRelief = blockedAction{
		unrecorded: "Record it or dismiss it first, then refund the unused credit: that refund may already be this money.",
		inFlight:   "Wait for Stripe to settle it, then refund the unused credit: that refund may already be this money.",
	}
)

// refundBlockedByStripeRefund guards Retry refund, "returned another way" and
// relief against refunding a customer twice (ADR-134): while the invoice has
// a refund made in Stripe that is not settled yet (flagged for review, or
// still in flight), the operator may have refunded by hand what the action
// would send. The action is refused until that refund is recorded, attached
// to a note, or dismissed.
func (s *Service) refundBlockedByStripeRefund(ctx context.Context, tenantID, invoiceID string, action blockedAction) error {
	if s.refunds == nil {
		return nil
	}
	rows, err := s.refunds.ListStripeRefundsByInvoice(ctx, tenantID, invoiceID)
	if err != nil {
		return fmt.Errorf("read refunds made in Stripe on the invoice: %w", err)
	}
	for _, r := range rows {
		switch r.Outcome {
		case domain.StripeRefundNeedsReview:
			return errs.InvalidState(fmt.Sprintf(
				"A refund of %s was made in Stripe on this invoice (%s) and is not recorded here. %s",
				domain.FormatMoneyMinor(r.AmountCents, r.Currency), r.StripeRefundID, action.unrecorded)).WithCode("stripe_refund_unresolved")
		case domain.StripeRefundAwaiting:
			return errs.InvalidState(fmt.Sprintf(
				"A refund of %s made in Stripe on this invoice (%s) is still in flight. %s",
				domain.FormatMoneyMinor(r.AmountCents, r.Currency), r.StripeRefundID, action.inFlight)).WithCode("stripe_refund_unresolved")
		}
	}
	return nil
}

// AttachStripeRefund makes a refund made in Stripe the card refund of an
// issued credit note whose own Hardcount refund failed (POST
// /v1/credit-notes/{id}/attach-stripe-refund): the operator refunded by hand
// what the note's refund was for. The refund is read from Stripe first and
// must be of the invoice's payment, not made by Hardcount, succeeded, and of
// exactly the note's refund amount. Under the invoice row lock and the
// credit-note lock, no note may hold it yet and its row, if any, must be
// flagged or dismissed; the note then holds the refund (CAS on the failed
// refund it held), its row is marked recorded, and the audit row is written,
// in one transaction. No new note, no second tax reversal (the note reversed
// its tax when it was issued), no Stripe call. Hardcount never matches a
// refund to a note by amount by itself.
func (s *Service) AttachStripeRefund(ctx context.Context, tenantID, creditNoteID, stripeRefundID string) (domain.CreditNote, error) {
	stripeRefundID = strings.TrimSpace(stripeRefundID)
	if stripeRefundID == "" {
		return domain.CreditNote{}, errs.Required("stripe_refund_id")
	}
	if s.refundReader == nil || s.refunds == nil {
		return domain.CreditNote{}, errs.InvalidState("Stripe is not connected, so a refund made in Stripe cannot be attached")
	}
	cn, err := s.store.Get(ctx, tenantID, creditNoteID)
	if err != nil {
		return domain.CreditNote{}, err
	}
	if cn.Status != domain.CreditNoteIssued || cn.RefundStatus != domain.RefundFailed || cn.RefundAmountCents <= 0 {
		return domain.CreditNote{}, errs.InvalidState("a refund made in Stripe can be attached only to an issued credit note whose card refund failed")
	}
	inv, err := s.invoices.Get(ctx, tenantID, cn.InvoiceID)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("get invoice: %w", err)
	}
	facts, err := s.refundReader.ReadRefund(ctx, stripeRefundID)
	if err != nil {
		return domain.CreditNote{}, err
	}
	if err := checkStripeRefundFacts(facts, inv); err != nil {
		return domain.CreditNote{}, err
	}
	if facts.AmountCents != cn.RefundAmountCents {
		return domain.CreditNote{}, errs.Invalid("stripe_refund_id", fmt.Sprintf(
			"this refund is %s and the credit note's card refund is %s; attach only a refund of the same amount",
			domain.FormatMoneyMinor(facts.AmountCents, inv.Currency), domain.FormatMoneyMinor(cn.RefundAmountCents, inv.Currency)))
	}

	tx, err := s.store.BeginTx(ctx, tenantID)
	if err != nil {
		return domain.CreditNote{}, err
	}
	defer postgres.Rollback(tx)
	if _, err := s.invoices.LockForCreditNoteTx(ctx, tx, tenantID, cn.InvoiceID); err != nil {
		return domain.CreditNote{}, fmt.Errorf("lock invoice: %w", err)
	}
	if err := s.store.LockInvoicesTx(ctx, tx, tenantID, []string{cn.InvoiceID}); err != nil {
		return domain.CreditNote{}, err
	}
	if holder, found, err := s.refunds.CreditNoteByStripeRefundTx(ctx, tx, stripeRefundID); err != nil {
		return domain.CreditNote{}, err
	} else if found {
		return domain.CreditNote{}, errs.InvalidState("credit note " + holder.CreditNoteNumber + " already records this refund")
	}
	row, rowFound, err := s.refunds.LockStripeRefundTx(ctx, tx, stripeRefundID)
	if err != nil {
		return domain.CreditNote{}, err
	}
	if rowFound {
		if _, err := s.linkUnderLockTx(ctx, tx, cn.InvoiceID, &linkedStripeRefund{facts: facts}, nil); err != nil {
			return domain.CreditNote{}, err
		}
	}
	status := facts.Status.CreditNoteRefundStatus()
	attached, err := s.refunds.AttachStripeRefundTx(ctx, tx, cn.ID, cn.StripeRefundID, stripeRefundID, status)
	if err != nil {
		return domain.CreditNote{}, err
	}
	if !attached {
		return domain.CreditNote{}, errs.InvalidState("the credit note's refund changed since it was read; reload it and try again")
	}
	if rowFound {
		if _, err := s.refunds.LinkStripeRefundTx(ctx, tx, row.ID, cn.ID); err != nil {
			return domain.CreditNote{}, err
		}
	}
	if s.audit != nil {
		if err := s.audit.LogInTx(ctx, tx, audit.Entry{
			Action:        domain.AuditActionUpdate,
			ResourceType:  "credit_note",
			ResourceID:    cn.ID,
			ResourceLabel: cn.CreditNoteNumber,
			Metadata: withoutEmpty(map[string]any{
				"action":                 "stripe_refund_attached",
				"stripe_refund_id":       stripeRefundID,
				"prior_stripe_refund_id": cn.StripeRefundID,
				"refund_status":          string(status),
				"prior_refund_status":    string(cn.RefundStatus),
				"invoice_id":             cn.InvoiceID,
				"customer_id":            cn.CustomerID,
				"refund_amount_cents":    cn.RefundAmountCents,
			}),
		}); err != nil {
			return domain.CreditNote{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.CreditNote{}, err
	}
	return s.store.Get(ctx, tenantID, cn.ID)
}
