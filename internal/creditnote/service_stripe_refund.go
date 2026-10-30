package creditnote

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
//     the refund's row (lock order rule, playbook §5.6), and every check
//     below reads under them;
//   - a credit note already holds the refund id (a refund Hardcount made
//     before refunds carried hardcount_cn_id, a note this recorded, or one the
//     operator linked): its refund status moves monotonically, as from any
//     refund webhook, and the row, if any, keeps the newer snapshot. Nothing
//     else;
//   - the row exists: it keeps the newer snapshot. Nothing else: the outcome
//     was decided at first sighting;
//   - first sighting: decideStripeRefund picks the outcome. A credit_note
//     outcome inserts and issues the note in this transaction, no draft ever
//     visible, with its refund already done. payment.refunded_in_stripe is
//     queued and the audit rows written in this transaction.
//
// After the commit, a recorded note runs the existing external legs: the
// refund leg skips (the note holds the refund id, so Hardcount calls nothing
// at Stripe), and the tax reversal runs, recovered by its sweep if it fails.
// The invoice itself is never written. Nothing here starts dunning, writes
// the credit ledger or emails the customer.
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
	// ADR-030: a recorded note is dated on the invoice's clock, as every
	// credit note is.
	ctx = s.bindForInvoice(ctx, tenantID, invoiceID)

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
	if noteHolds || rowFound {
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

	// First sighting.
	existing, err := s.refunds.ListByInvoiceTx(ctx, tx, invoiceID)
	if err != nil {
		return domain.StripeRefundRecordResult{}, err
	}
	// The invoice's lines are fixed once it is finalized, so reading them on
	// their own transaction is safe.
	lines, err := s.invoices.ListLineItems(ctx, tenantID, invoiceID)
	if err != nil {
		return domain.StripeRefundRecordResult{}, fmt.Errorf("list invoice line items: %w", err)
	}
	hasCommit := false
	for _, li := range lines {
		hasCommit = hasCommit || li.IsCommitLine()
	}
	d, err := decideStripeRefund(inv, snap, hasCommit, existing)
	if err != nil {
		return domain.StripeRefundRecordResult{}, err
	}
	if d.retry {
		return domain.StripeRefundRecordResult{}, fmt.Errorf("refund %s on invoice %s (%s, payment %s): %w",
			snap.StripeRefundID, invoiceID, inv.Status, inv.PaymentStatus, errRefundInvoiceNotPaidYet)
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
		Outcome:               d.outcome,
		ReviewReason:          d.reviewReason,
		RefundCreatedAt:       snap.CreatedAt,
		LastEventAt:           snap.EventAt,
		LastStripeEventID:     snap.StripeEventID,
	}
	if rec.RefundCreatedAt.IsZero() {
		rec.RefundCreatedAt = snap.EventAt
	}
	var cn domain.CreditNote
	if d.outcome == domain.StripeRefundRecordedAsCreditNote {
		if cn, err = s.recordRefundCreditNoteTx(ctx, tx, tenantID, inv, snap); err != nil {
			return domain.StripeRefundRecordResult{}, err
		}
		rec.CreditNoteID = cn.ID
	}
	if rec, err = s.refunds.InsertStripeRefundTx(ctx, tx, tenantID, rec); err != nil {
		return domain.StripeRefundRecordResult{}, err
	}
	// The event carries the invoice as locked: the refund leaves it as it was
	// (ADR-131).
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

// stripeRefundDecision is what decideStripeRefund picked for a first
// sighting. retry asks Stripe to deliver the event again instead.
type stripeRefundDecision struct {
	outcome      domain.StripeRefundOutcome
	reviewReason domain.StripeRefundReviewReason
	retry        bool
}

// decideStripeRefund picks what a refund first seen does (ADR-134). inv is
// the invoice as locked, existing its credit notes under the credit-note
// lock, hasCommit whether it bought prepaid credit. The refund becomes a
// credit note only when all of these hold:
//
//  1. it did not end without moving money (failed or canceled);
//  2. its PaymentIntent is the invoice's own payment, not a second charge or
//     money on a voided invoice;
//  3. the invoice is paid, with payment status succeeded;
//  4. its currency is the invoice's;
//  5. the invoice holds no prepaid-commit line (ADR-078 D4);
//  6. Hardcount sees it within StripeRefundLateAfter of its creation;
//  7. it fits both caps: what the card payment has left to refund after the
//     notes not voided, and what the invoice has left to credit.
//
// A refund of the invoice's own payment that fails 3 while it is young asks
// for redelivery: the payment may be recorded in a moment.
func decideStripeRefund(inv domain.Invoice, snap domain.StripeRefundSnapshot, hasCommit bool, existing []domain.CreditNote) (stripeRefundDecision, error) {
	review := func(r domain.StripeRefundReviewReason) (stripeRefundDecision, error) {
		return stripeRefundDecision{outcome: domain.StripeRefundNeedsReview, reviewReason: r}, nil
	}
	own := snap.StripePaymentIntentID == inv.StripePaymentIntentID
	paid := inv.Status == domain.InvoicePaid && inv.PaymentStatus == domain.PaymentSucceeded
	switch {
	case snap.Status.MovedNoMoney():
		return stripeRefundDecision{outcome: domain.StripeRefundNotRefunded}, nil
	case !own && (paid || inv.Status == domain.InvoiceVoided || snap.StripePaymentIntentID == inv.PaymentAnomalyPaymentIntentID):
		return stripeRefundDecision{outcome: domain.StripeRefundOtherPayment}, nil
	case !paid:
		if snap.Young {
			return stripeRefundDecision{retry: true}, nil
		}
		return review(domain.StripeRefundReviewInvoiceNotPaid)
	case !strings.EqualFold(snap.Currency, inv.Currency):
		return review(domain.StripeRefundReviewCurrencyMismatch)
	case hasCommit:
		return review(domain.StripeRefundReviewPrepaidCommit)
	case snap.SeenLate():
		return review(domain.StripeRefundReviewSeenLate)
	}
	var refunded, credited int64
	for _, cn := range existing {
		if cn.Status == domain.CreditNoteVoided {
			continue
		}
		var err error
		if refunded, err = domain.SumCents(refunded, cn.RefundAmountCents); err == nil {
			credited, err = domain.SumCents(credited, cn.TotalCents)
		}
		if err != nil {
			return stripeRefundDecision{}, fmt.Errorf("credit notes on invoice %s: %w", inv.ID, err)
		}
	}
	if snap.AmountCents > inv.CardRefundableCents(refunded) {
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
// that inserts it, like commit relief, so no draft is ever visible. The
// caller holds the invoice row and the credit-note lock.
func (s *Service) recordRefundCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, snap domain.StripeRefundSnapshot) (domain.CreditNote, error) {
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
	build := func(lockTx *sql.Tx, existingCNs []domain.CreditNote) (domain.CreditNote, error) {
		var existingTotal, existingTaxReversed int64
		for _, cn := range existingCNs {
			if cn.Status == domain.CreditNoteVoided {
				continue
			}
			var err error
			if existingTotal, err = domain.SumCents(existingTotal, cn.TotalCents); err == nil {
				existingTaxReversed, err = domain.SumCents(existingTaxReversed, cn.TaxAmountCents)
			}
			if err != nil {
				return domain.CreditNote{}, fmt.Errorf("credit notes on invoice %s: %w", inv.ID, err)
			}
		}
		cn, err := s.buildCreditNote(ctx, lockTx, tenantID, input, inv, amount, existingTotal, existingTaxReversed, existingCNs)
		if err != nil {
			return domain.CreditNote{}, err
		}
		cn.StripeRefundID = snap.StripeRefundID
		cn.RefundStatus = snap.Status.CreditNoteRefundStatus()
		cn.RecordedFromStripe = true
		return cn, nil
	}
	var emit func(tx *sql.Tx, created domain.CreditNote) error
	if s.audit != nil {
		emit = func(tx *sql.Tx, created domain.CreditNote) error {
			return s.audit.LogInTx(ctx, tx, audit.Entry{
				Action:        domain.AuditActionCreate,
				ResourceType:  "credit_note",
				ResourceID:    created.ID,
				ResourceLabel: created.CreditNoteNumber,
				Metadata: map[string]any{
					"invoice_id":       created.InvoiceID,
					"customer_id":      created.CustomerID,
					"total_cents":      created.TotalCents,
					"reason":           created.Reason,
					"currency":         created.Currency,
					"origin":           "stripe_refund",
					"stripe_refund_id": snap.StripeRefundID,
				},
			})
		}
	}
	cn, err := s.store.CreateUnderInvoiceLockTxAudited(ctx, tx, tenantID, inv.ID, []domain.CreditNoteLineItem{line}, build, emit)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("record refund %s as a credit note: %w", snap.StripeRefundID, err)
	}
	won, err := s.store.TransitionStatusTx(ctx, tx, tenantID, cn.ID, domain.CreditNoteDraft, domain.CreditNoteIssued)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("issue the credit note recording refund %s: %w", snap.StripeRefundID, err)
	}
	if !won {
		return domain.CreditNote{}, fmt.Errorf("the credit note recording refund %s lost its issue CAS on the transaction that created it (impossible; investigate)", snap.StripeRefundID)
	}
	cn.Status = domain.CreditNoteIssued
	if s.audit != nil {
		if err := s.audit.LogInTx(ctx, tx, audit.Entry{
			Action:        "credit_note.issued",
			ResourceType:  "credit_note",
			ResourceID:    cn.ID,
			ResourceLabel: cn.CreditNoteNumber,
			Metadata: map[string]any{
				"credit_note_number":       cn.CreditNoteNumber,
				"invoice_id":               cn.InvoiceID,
				"customer_id":              cn.CustomerID,
				"total_cents":              cn.TotalCents,
				"refund_amount_cents":      cn.RefundAmountCents,
				"credit_amount_cents":      cn.CreditAmountCents,
				"out_of_band_amount_cents": cn.OutOfBandAmountCents,
				"reason":                   cn.Reason,
				"currency":                 cn.Currency,
				"origin":                   "stripe_refund",
				"stripe_refund_id":         snap.StripeRefundID,
				"refund_status":            string(cn.RefundStatus),
			},
		}); err != nil {
			return domain.CreditNote{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	return cn, nil
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
	ctx = s.bindForInvoice(ctx, tenantID, invoiceID)
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
	switch {
	case facts.HardcountCreditNoteID != "":
		return nil, errs.Invalid("stripe_refund_id", "Hardcount made this refund for credit note "+facts.HardcountCreditNoteID+"; it is not a refund made in Stripe")
	case facts.PaymentIntentID == "" || facts.PaymentIntentID != inv.StripePaymentIntentID:
		return nil, errs.Invalid("stripe_refund_id", "this refund is not of the payment that paid this invoice")
	case facts.Status.MovedNoMoney():
		return nil, errs.Invalid("stripe_refund_id", "this refund "+string(facts.Status)+" at Stripe, so no money went back to the customer")
	case !strings.EqualFold(facts.Currency, inv.Currency):
		return nil, errs.Invalid("stripe_refund_id", fmt.Sprintf("this refund is in %s and the invoice in %s", strings.ToUpper(facts.Currency), inv.Currency))
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
	case domain.StripeRefundOtherPayment:
		return nil, errs.Invalid("stripe_refund_id", "this refund is of a payment that did not pay this invoice")
	case domain.StripeRefundNotRefunded:
		return nil, errs.Invalid("stripe_refund_id", "this refund moved no money")
	default:
		return nil, errs.InvalidState("this refund is already recorded as a credit note")
	}
}
