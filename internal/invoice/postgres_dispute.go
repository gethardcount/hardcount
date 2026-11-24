package invoice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

const disputeCols = `id, invoice_id, stripe_dispute_id, stripe_charge_id, stripe_payment_intent_id,
	status, reason, amount_cents, currency, evidence_due_by, opened_at, closed_at,
	COALESCE(credit_note_id, ''), last_event_at, last_stripe_event_id, livemode, created_at, updated_at`

func scanDisputeDest(d *domain.InvoiceDispute) []any {
	return []any{
		&d.ID, &d.InvoiceID, &d.StripeDisputeID, &d.StripeChargeID, &d.StripePaymentIntentID,
		(*string)(&d.Status), &d.Reason, &d.AmountCents, &d.Currency, &d.EvidenceDueBy, &d.OpenedAt, &d.ClosedAt,
		&d.CreditNoteID, &d.LastEventAt, &d.LastStripeEventID, &d.Livemode, &d.CreatedAt, &d.UpdatedAt,
	}
}

// RecordStripeDispute records one charge.dispute.* snapshot against the
// invoice whose payment was disputed (#979), in one transaction:
//
//   - the invoice is found from the dispute's own row when Hardcount already
//     recorded it, otherwise from the disputed PaymentIntent (the invoice's
//     charge attempts, then the invoice's own payment and anomaly columns).
//     No invoice holds it: errs.ErrNotFound, the charge was not Hardcount's;
//   - the invoice row is locked first, then the dispute row (lock order,
//     playbook item 8), so two events for one dispute serialize. The
//     invoice lock is FOR NO KEY UPDATE, which never waits for the FOR KEY
//     SHARE a commit relief holds on it (creditnote.CreateAndIssueCommitRelief);
//     this transaction writes no invoice column;
//   - the snapshot is written only when it is newer than the row
//     (InvoiceDispute.SupersededBy), so a redelivery or an older event that
//     arrives late changes nothing and queues nothing;
//   - on the move into lost of a dispute on a top-up's own payment that has no
//     credit note yet, the loss's note is written in this transaction
//     (recordLossTx): money returned by the bank, taking back the unused
//     credit it paid for. The usage not invoiced yet is read before the
//     transaction (lossInputs); when it cannot be read, the loss is recorded
//     without its note and the operator's record-loss writes it;
//   - payment.dispute_opened is queued when this call creates the row, and
//     payment.dispute_closed when it moves the dispute to a closed status it
//     did not have, both in this transaction (ADR-040 outbox): the events
//     commit with the record or not at all.
//
// The invoice row itself is never written: its status, payment status and
// amounts stay as they are.
func (s *PostgresStore) RecordStripeDispute(ctx context.Context, tenantID string, snap domain.DisputeSnapshot) (domain.DisputeRecordResult, error) {
	if snap.StripeDisputeID == "" || snap.StripePaymentIntentID == "" {
		return domain.DisputeRecordResult{}, fmt.Errorf("dispute snapshot needs a dispute id and a payment intent id")
	}
	if !snap.Status.Valid() {
		return domain.DisputeRecordResult{}, fmt.Errorf("dispute %s: unknown status %q", snap.StripeDisputeID, snap.Status)
	}
	currency := strings.ToUpper(strings.TrimSpace(snap.Currency))
	loss := s.lossInputs(ctx, tenantID, snap)

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.DisputeRecordResult{}, err
	}
	defer postgres.Rollback(tx)

	invoiceID, err := disputedInvoiceTx(ctx, tx, snap.StripeDisputeID, snap.StripePaymentIntentID)
	if err != nil {
		return domain.DisputeRecordResult{}, err
	}
	// The whole row: the events below carry the invoice (ADR-131).
	var inv domain.Invoice
	if err := tx.QueryRowContext(ctx,
		`SELECT `+invCols+` FROM invoices WHERE id = $1 FOR NO KEY UPDATE`, invoiceID,
	).Scan(s.scanInvDest(&inv)...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.DisputeRecordResult{}, errs.ErrNotFound
		}
		return domain.DisputeRecordResult{}, fmt.Errorf("lock disputed invoice: %w", err)
	}

	var cur domain.InvoiceDispute
	err = tx.QueryRowContext(ctx,
		`SELECT `+disputeCols+` FROM invoice_disputes WHERE stripe_dispute_id = $1 FOR UPDATE`,
		snap.StripeDisputeID,
	).Scan(scanDisputeDest(&cur)...)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.DisputeRecordResult{}, fmt.Errorf("read dispute %s: %w", snap.StripeDisputeID, err)
	}

	var res domain.DisputeRecordResult
	switch {
	case !found:
		var closedAt *time.Time
		if snap.Status.Closed() {
			closedAt = &snap.EventAt
		}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO invoice_disputes
				(tenant_id, invoice_id, stripe_dispute_id, stripe_charge_id, stripe_payment_intent_id,
				 status, reason, amount_cents, currency, evidence_due_by, opened_at, closed_at,
				 last_event_at, last_stripe_event_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			RETURNING `+disputeCols,
			tenantID, invoiceID, snap.StripeDisputeID, snap.StripeChargeID, snap.StripePaymentIntentID,
			string(snap.Status), snap.Reason, snap.AmountCents, currency, snap.EvidenceDueBy,
			snap.OpenedAt, closedAt, snap.EventAt, snap.StripeEventID,
		).Scan(scanDisputeDest(&res.Dispute)...); err != nil {
			return domain.DisputeRecordResult{}, fmt.Errorf("insert dispute %s: %w", snap.StripeDisputeID, err)
		}
		res.Applied, res.Opened, res.Closed = true, true, snap.Status.Closed()

	case !cur.SupersededBy(snap):
		// A redelivery, or an event older than the one the row holds.
		res.Dispute = cur
		return res, tx.Commit()

	default:
		// The close time is the first event that carried this closed status.
		// A later snapshot with the same status keeps it; a new closed status
		// (a late win after a loss) restarts it; an open status clears it.
		var closedAt *time.Time
		if snap.Status.Closed() {
			closedAt = &snap.EventAt
			if cur.Status == snap.Status && cur.ClosedAt != nil {
				closedAt = cur.ClosedAt
			}
		}
		chargeID := snap.StripeChargeID
		if chargeID == "" {
			chargeID = cur.StripeChargeID
		}
		if err := tx.QueryRowContext(ctx, `
			UPDATE invoice_disputes SET
				stripe_charge_id = $2, status = $3, reason = $4, amount_cents = $5, currency = $6,
				evidence_due_by = $7, closed_at = $8, last_event_at = $9, last_stripe_event_id = $10,
				updated_at = now()
			WHERE id = $1
			RETURNING `+disputeCols,
			cur.ID, chargeID, string(snap.Status), snap.Reason, snap.AmountCents, currency,
			snap.EvidenceDueBy, closedAt, snap.EventAt, snap.StripeEventID,
		).Scan(scanDisputeDest(&res.Dispute)...); err != nil {
			return domain.DisputeRecordResult{}, fmt.Errorf("update dispute %s: %w", snap.StripeDisputeID, err)
		}
		res.Applied = true
		res.Closed = snap.Status.Closed() && snap.Status != cur.Status
	}

	if res.Dispute.Status == domain.DisputeLost && (!found || cur.Status != domain.DisputeLost) {
		if res.Dispute, err = s.recordLossTx(ctx, tx, tenantID, inv, res.Dispute, loss); err != nil {
			return domain.DisputeRecordResult{}, err
		}
	}

	if s.outbox != nil {
		payload := domain.InvoiceWebhookData(inv, map[string]any{"dispute": res.Dispute})
		if res.Opened {
			if _, err := s.outbox.Enqueue(ctx, tx, tenantID, domain.EventPaymentDisputeOpened, payload); err != nil {
				return domain.DisputeRecordResult{}, fmt.Errorf("enqueue %s: %w", domain.EventPaymentDisputeOpened, err)
			}
		}
		if res.Closed {
			if _, err := s.outbox.Enqueue(ctx, tx, tenantID, domain.EventPaymentDisputeClosed, payload); err != nil {
				return domain.DisputeRecordResult{}, fmt.Errorf("enqueue %s: %w", domain.EventPaymentDisputeClosed, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.DisputeRecordResult{}, err
	}
	return res, nil
}

// lossInput is what the loss's note needs from before the transaction: the
// customer's usage not invoiced yet (DisputeLossRecorder.UnbilledUsageCents),
// or why it could not be read.
type lossInput struct {
	unbilledCents int64
	err           error
}

// lossInputs reads, before RecordStripeDispute's transaction, the usage not
// invoiced yet for a lost snapshot on a top-up's own payment; zero for any
// other snapshot. The transaction decides again under its locks.
func (s *PostgresStore) lossInputs(ctx context.Context, tenantID string, snap domain.DisputeSnapshot) lossInput {
	if snap.Status != domain.DisputeLost || s.disputeLoss == nil {
		return lossInput{}
	}
	inv, err := s.disputedInvoice(ctx, tenantID, snap)
	if err != nil {
		return lossInput{err: err}
	}
	if !(domain.InvoiceDispute{StripePaymentIntentID: snap.StripePaymentIntentID, Currency: snap.Currency}).LossTakesCredit(inv) {
		return lossInput{}
	}
	cents, err := s.disputeLoss.UnbilledUsageCents(ctx, tenantID, inv)
	return lossInput{unbilledCents: cents, err: err}
}

// disputedInvoice reads the invoice a dispute snapshot is on, on a
// transaction of its own.
func (s *PostgresStore) disputedInvoice(ctx context.Context, tenantID string, snap domain.DisputeSnapshot) (domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)
	invoiceID, err := disputedInvoiceTx(ctx, tx, snap.StripeDisputeID, snap.StripePaymentIntentID)
	if err != nil {
		return domain.Invoice{}, err
	}
	var inv domain.Invoice
	if err := tx.QueryRowContext(ctx, `SELECT `+invCols+` FROM invoices WHERE id = $1`, invoiceID).Scan(s.scanInvDest(&inv)...); err != nil {
		return domain.Invoice{}, fmt.Errorf("read disputed invoice %s: %w", invoiceID, err)
	}
	return inv, nil
}

// recordLossTx writes, on tx, the credit note of lost dispute d while it has
// none, and returns d naming it. Only a loss on a top-up's own payment in its
// currency writes one (domain.InvoiceDispute.LossTakesCredit), and none when
// the payment's documents already return all of it. When the usage not
// invoiced yet could not be read, the loss is recorded without its note
// (credit_note_id stays NULL, the lost notice is Critical) and an operator's
// record-loss writes it: the record never waits on credit. The caller holds
// the invoice row and the dispute row.
func (s *PostgresStore) recordLossTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, d domain.InvoiceDispute, in lossInput) (domain.InvoiceDispute, error) {
	if s.disputeLoss == nil || d.CreditNoteID != "" || !d.LossTakesCredit(inv) {
		return d, nil
	}
	if in.err != nil {
		slog.ErrorContext(ctx, "lost dispute on a top-up recorded without its credit note: the usage not invoiced yet could not be read, so the credit the payment paid for could not be worked out; record the loss on the invoice's dispute (record-loss)",
			"invoice_id", inv.ID, "stripe_dispute_id", d.StripeDisputeID, "error", in.err)
		return d, nil
	}
	noteID, err := s.disputeLoss.TakeBackForLostDisputeTx(ctx, tx, tenantID, inv, d, in.unbilledCents)
	if err != nil || noteID == "" {
		return d, err
	}
	if err := tx.QueryRowContext(ctx, `
		UPDATE invoice_disputes SET credit_note_id = $2, updated_at = now()
		WHERE id = $1 AND credit_note_id IS NULL
		RETURNING `+disputeCols, d.ID, noteID).Scan(scanDisputeDest(&d)...); err != nil {
		return domain.InvoiceDispute{}, fmt.Errorf("name the credit note of lost dispute %s: %w", d.StripeDisputeID, err)
	}
	return d, nil
}

// RecordDisputeLoss writes the credit note of a lost dispute on a top-up's
// own payment that has none, the step RecordStripeDispute takes on a loss,
// run again on the stored dispute (POST
// /v1/invoices/{id}/disputes/{dispute_id}/record-loss). The usage not
// invoiced yet is read first, then the invoice row FOR NO KEY UPDATE and the
// dispute row FOR UPDATE, as the recorder takes them. It is refused, with a
// sentence, when the dispute is not lost, already has its note, is not on the
// top-up's own payment, is in another currency, or the payment's documents
// already return all of it. errs.ErrNotFound when no such dispute is on the
// invoice.
func (s *PostgresStore) RecordDisputeLoss(ctx context.Context, tenantID, invoiceID, disputeID string) (domain.InvoiceDispute, error) {
	if s.disputeLoss == nil {
		return domain.InvoiceDispute{}, fmt.Errorf("recording a lost dispute is not wired (call SetDisputeLossRecorder)")
	}
	pre, err := s.Get(ctx, tenantID, invoiceID)
	if err != nil {
		return domain.InvoiceDispute{}, err
	}
	if pre.BillingReason != domain.BillingReasonTopup {
		return domain.InvoiceDispute{}, errs.InvalidState("This invoice is not a top-up, so its lost dispute takes back no credit; record the loss with an ordinary credit note.")
	}
	unbilled, err := s.disputeLoss.UnbilledUsageCents(ctx, tenantID, pre)
	if err != nil {
		return domain.InvoiceDispute{}, err
	}

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.InvoiceDispute{}, err
	}
	defer postgres.Rollback(tx)
	var inv domain.Invoice
	if err := tx.QueryRowContext(ctx,
		`SELECT `+invCols+` FROM invoices WHERE id = $1 FOR NO KEY UPDATE`, invoiceID,
	).Scan(s.scanInvDest(&inv)...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.InvoiceDispute{}, errs.ErrNotFound
		}
		return domain.InvoiceDispute{}, fmt.Errorf("lock invoice %s: %w", invoiceID, err)
	}
	var d domain.InvoiceDispute
	if err := tx.QueryRowContext(ctx,
		`SELECT `+disputeCols+` FROM invoice_disputes WHERE id = $1 AND invoice_id = $2 FOR UPDATE`, disputeID, invoiceID,
	).Scan(scanDisputeDest(&d)...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.InvoiceDispute{}, errs.ErrNotFound
		}
		return domain.InvoiceDispute{}, fmt.Errorf("lock dispute %s: %w", disputeID, err)
	}
	switch {
	case d.Status != domain.DisputeLost:
		return domain.InvoiceDispute{}, errs.InvalidState("This dispute is not lost, so there is no loss to record.")
	case d.CreditNoteID != "":
		return domain.InvoiceDispute{}, errs.InvalidState("This loss is already recorded as a credit note.")
	case !d.OnTopupPayment(inv):
		return domain.InvoiceDispute{}, errs.InvalidState("This dispute is not on the payment that paid this top-up, so it takes back none of its credit.")
	case !d.LossTakesCredit(inv):
		return domain.InvoiceDispute{}, errs.InvalidState(fmt.Sprintf(
			"This dispute is in %s and the top-up in %s, so Hardcount takes back no credit for it; record the loss in your own books.",
			strings.ToUpper(d.Currency), strings.ToUpper(inv.Currency)))
	}
	if d, err = s.recordLossTx(ctx, tx, tenantID, inv, d, lossInput{unbilledCents: unbilled}); err != nil {
		return domain.InvoiceDispute{}, err
	}
	if d.CreditNoteID == "" {
		return domain.InvoiceDispute{}, errs.InvalidState("This payment has already been returned in full, so the loss has nothing left to record.")
	}
	return d, tx.Commit()
}

// disputedInvoiceTx finds the invoice a dispute belongs to. A recorded
// dispute keeps the invoice it was first recorded against. A new one is
// matched by its PaymentIntent: every payment a settle path saw has a charge
// attempt row (the success upserts it in the settle transaction), and the
// invoice columns cover rows from before attempts were recorded and payments
// flagged as anomalies.
func disputedInvoiceTx(ctx context.Context, tx *sql.Tx, stripeDisputeID, paymentIntentID string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT invoice_id FROM invoice_disputes WHERE stripe_dispute_id = $1`, stripeDisputeID,
	).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read dispute %s: %w", stripeDisputeID, err)
	}
	return invoiceForPaymentIntentTx(ctx, tx, paymentIntentID)
}

// InvoiceForPaymentIntentTx finds, on the caller's tx, the invoice a Stripe
// PaymentIntent paid or was charged for: the invoice's charge attempts first,
// then its own payment and anomaly columns, as for a dispute. errs.ErrNotFound
// when no invoice holds it. The credit-note domain records a refund made in
// Stripe against this invoice (ADR-134).
func (s *PostgresStore) InvoiceForPaymentIntentTx(ctx context.Context, tx *sql.Tx, paymentIntentID string) (string, error) {
	return invoiceForPaymentIntentTx(ctx, tx, paymentIntentID)
}

// invoiceForPaymentIntentTx matches an invoice by PaymentIntent: every
// payment a settle path saw has a charge attempt row (the success upserts it
// in the settle transaction), and the invoice columns cover rows from before
// attempts were recorded and payments flagged as anomalies.
func invoiceForPaymentIntentTx(ctx context.Context, tx *sql.Tx, paymentIntentID string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT invoice_id FROM invoice_charge_attempts WHERE stripe_payment_intent_id = $1`, paymentIntentID,
	).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("find attempt for payment intent %s: %w", paymentIntentID, err)
	}
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM invoices
		WHERE stripe_payment_intent_id = $1 OR payment_anomaly_payment_intent_id = $1
		ORDER BY (stripe_payment_intent_id = $1) DESC, created_at
		LIMIT 1`, paymentIntentID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errs.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("find invoice for payment intent %s: %w", paymentIntentID, err)
	}
	return id, nil
}

// DisputeStatusesTx returns the status of every dispute recorded on the
// invoice's payment paymentIntentID, on the caller's tx, with no lock. Commit
// relief and "returned another way" read it under the credit-note lock to
// refuse a payment the bank has taken or holds.
func (s *PostgresStore) DisputeStatusesTx(ctx context.Context, tx *sql.Tx, invoiceID, paymentIntentID string) ([]domain.DisputeStatus, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT status FROM invoice_disputes WHERE invoice_id = $1 AND stripe_payment_intent_id = $2`, invoiceID, paymentIntentID)
	if err != nil {
		return nil, fmt.Errorf("read disputes of invoice %s: %w", invoiceID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []domain.DisputeStatus
	for rows.Next() {
		var st domain.DisputeStatus
		if err := rows.Scan((*string)(&st)); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ListDisputesByInvoice returns the invoice's disputes, oldest first.
func (s *PostgresStore) ListDisputesByInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceDispute, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx,
		`SELECT `+disputeCols+` FROM invoice_disputes WHERE invoice_id = $1 ORDER BY opened_at, id`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("list disputes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []domain.InvoiceDispute{}
	for rows.Next() {
		var d domain.InvoiceDispute
		if err := rows.Scan(scanDisputeDest(&d)...); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// NoticeDisputesByInvoices returns, for a page of invoices, the dispute each
// one's notice reads: its newest dispute closed as lost, else its newest
// chargeback still open (an appeal included, an inquiry not). Invoices with
// neither are absent. One read for the page.
func (s *PostgresStore) NoticeDisputesByInvoices(ctx context.Context, tenantID string, invoiceIDs []string) (map[string]domain.InvoiceDispute, error) {
	out := make(map[string]domain.InvoiceDispute)
	if len(invoiceIDs) == 0 {
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT ON (invoice_id) `+disputeCols+`
		FROM invoice_disputes
		WHERE invoice_id = ANY($1::text[])
		  AND status IN ('lost', 'needs_response', 'under_review', 'appeal_needs_response', 'appeal_under_review')
		ORDER BY invoice_id, (status = 'lost') DESC, COALESCE(closed_at, opened_at) DESC, id DESC`,
		invoiceIDs)
	if err != nil {
		return nil, fmt.Errorf("read the disputes the notices show: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var d domain.InvoiceDispute
		if err := rows.Scan(scanDisputeDest(&d)...); err != nil {
			return nil, err
		}
		out[d.InvoiceID] = d
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}
