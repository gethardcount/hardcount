package creditnote

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// StripeRefundStore is the store behind refunds made in Stripe outside
// Hardcount (ADR-134): the invoice_stripe_refunds rows, and the credit-note
// reads and writes RecordStripeRefund makes on the caller's transaction.
// *PostgresStore implements it. It is its own interface, beside Store,
// because only RecordStripeRefund, the operator's link at create and the
// readers need it.
type StripeRefundStore interface {
	// InvoiceForStripeRefundTx returns the invoice a refund already belongs
	// to: the one its row names, else the one whose credit note holds it.
	// errs.ErrNotFound when neither exists.
	InvoiceForStripeRefundTx(ctx context.Context, tx *sql.Tx, stripeRefundID string) (string, error)
	// LockStripeRefundTx reads the refund's row FOR UPDATE; found is false
	// when there is none.
	LockStripeRefundTx(ctx context.Context, tx *sql.Tx, stripeRefundID string) (row domain.InvoiceStripeRefund, found bool, err error)
	// CreditNoteByStripeRefundTx returns the credit note holding the refund
	// id, if any.
	CreditNoteByStripeRefundTx(ctx context.Context, tx *sql.Tx, stripeRefundID string) (cn domain.CreditNote, found bool, err error)
	// ListByInvoiceTx lists every credit note on the invoice on tx, under the
	// caller's per-invoice credit-note lock.
	ListByInvoiceTx(ctx context.Context, tx *sql.Tx, invoiceID string) ([]domain.CreditNote, error)
	InsertStripeRefundTx(ctx context.Context, tx *sql.Tx, tenantID string, row domain.InvoiceStripeRefund) (domain.InvoiceStripeRefund, error)
	// UpdateStripeRefundSnapshotTx writes a newer snapshot's status, reason,
	// failure reason and event onto the row. The outcome never changes here.
	UpdateStripeRefundSnapshotTx(ctx context.Context, tx *sql.Tx, id string, snap domain.StripeRefundSnapshot) (domain.InvoiceStripeRefund, error)
	// LinkStripeRefundTx marks a needs_review or dismissed row as recorded by
	// the credit note.
	LinkStripeRefundTx(ctx context.Context, tx *sql.Tx, id, creditNoteID string) (domain.InvoiceStripeRefund, error)
	// DismissStripeRefundTx marks a needs_review row as handled outside
	// Hardcount.
	DismissStripeRefundTx(ctx context.Context, tx *sql.Tx, id, note string, at time.Time) (domain.InvoiceStripeRefund, error)
	// ApplyRefundWebhookStatusTx is ApplyRefundWebhookStatus on the caller's
	// tx, with the same monotonic guard. found is false when no credit note
	// holds the refund id; flipped says whether the status moved.
	ApplyRefundWebhookStatusTx(ctx context.Context, tx *sql.Tx, stripeRefundID string, status domain.RefundStatus) (cn domain.CreditNote, flipped, found bool, err error)
	ListStripeRefundsByInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceStripeRefund, error)
	// StripeRefundsNeedingReviewByInvoices returns, for a page of invoices,
	// the newest needs_review refund on each: invoice id → row.
	StripeRefundsNeedingReviewByInvoices(ctx context.Context, tenantID string, invoiceIDs []string) (map[string]domain.InvoiceStripeRefund, error)
}

const stripeRefundCols = `id, invoice_id, stripe_refund_id, stripe_payment_intent_id, stripe_charge_id,
	amount_cents, currency, status, reason, failure_reason, outcome, review_reason,
	COALESCE(credit_note_id, ''), dismissed_note, dismissed_at, refund_created_at,
	last_event_at, last_stripe_event_id, livemode, created_at, updated_at`

func scanStripeRefundDest(r *domain.InvoiceStripeRefund) []any {
	return []any{
		&r.ID, &r.InvoiceID, &r.StripeRefundID, &r.StripePaymentIntentID, &r.StripeChargeID,
		&r.AmountCents, &r.Currency, (*string)(&r.Status), &r.Reason, &r.FailureReason,
		(*string)(&r.Outcome), (*string)(&r.ReviewReason),
		&r.CreditNoteID, &r.DismissedNote, &r.DismissedAt, &r.RefundCreatedAt,
		&r.LastEventAt, &r.LastStripeEventID, &r.Livemode, &r.CreatedAt, &r.UpdatedAt,
	}
}

func (s *PostgresStore) InvoiceForStripeRefundTx(ctx context.Context, tx *sql.Tx, stripeRefundID string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT invoice_id FROM invoice_stripe_refunds WHERE stripe_refund_id = $1`, stripeRefundID,
	).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read refund %s: %w", stripeRefundID, err)
	}
	err = tx.QueryRowContext(ctx,
		`SELECT invoice_id FROM credit_notes WHERE stripe_refund_id = $1 ORDER BY created_at LIMIT 1`, stripeRefundID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errs.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("find the credit note holding refund %s: %w", stripeRefundID, err)
	}
	return id, nil
}

func (s *PostgresStore) LockStripeRefundTx(ctx context.Context, tx *sql.Tx, stripeRefundID string) (domain.InvoiceStripeRefund, bool, error) {
	var r domain.InvoiceStripeRefund
	err := tx.QueryRowContext(ctx,
		`SELECT `+stripeRefundCols+` FROM invoice_stripe_refunds WHERE stripe_refund_id = $1 FOR UPDATE`, stripeRefundID,
	).Scan(scanStripeRefundDest(&r)...)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.InvoiceStripeRefund{}, false, nil
	}
	if err != nil {
		return domain.InvoiceStripeRefund{}, false, fmt.Errorf("lock refund %s: %w", stripeRefundID, err)
	}
	return r, true, nil
}

func (s *PostgresStore) CreditNoteByStripeRefundTx(ctx context.Context, tx *sql.Tx, stripeRefundID string) (domain.CreditNote, bool, error) {
	var cn domain.CreditNote
	var metaJSON []byte
	err := tx.QueryRowContext(ctx, `SELECT `+cnReadCols+`
		FROM credit_notes WHERE stripe_refund_id = $1 ORDER BY created_at LIMIT 1`, stripeRefundID,
	).Scan(s.cnScanDest(&cn, &metaJSON)...)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CreditNote{}, false, nil
	}
	if err != nil {
		return domain.CreditNote{}, false, fmt.Errorf("find the credit note holding refund %s: %w", stripeRefundID, err)
	}
	_ = json.Unmarshal(metaJSON, &cn.Metadata)
	return cn, true, nil
}

func (s *PostgresStore) ListByInvoiceTx(ctx context.Context, tx *sql.Tx, invoiceID string) ([]domain.CreditNote, error) {
	return s.listByInvoiceTx(ctx, tx, invoiceID)
}

func (s *PostgresStore) InsertStripeRefundTx(ctx context.Context, tx *sql.Tx, tenantID string, r domain.InvoiceStripeRefund) (domain.InvoiceStripeRefund, error) {
	var out domain.InvoiceStripeRefund
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO invoice_stripe_refunds
			(tenant_id, invoice_id, stripe_refund_id, stripe_payment_intent_id, stripe_charge_id,
			 amount_cents, currency, status, reason, failure_reason, outcome, review_reason,
			 credit_note_id, refund_created_at, last_event_at, last_stripe_event_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NULLIF($13, ''), $14, $15, $16)
		RETURNING `+stripeRefundCols,
		tenantID, r.InvoiceID, r.StripeRefundID, r.StripePaymentIntentID, r.StripeChargeID,
		r.AmountCents, strings.ToUpper(strings.TrimSpace(r.Currency)), string(r.Status), r.Reason, r.FailureReason,
		string(r.Outcome), string(r.ReviewReason), r.CreditNoteID, r.RefundCreatedAt, r.LastEventAt, r.LastStripeEventID,
	).Scan(scanStripeRefundDest(&out)...); err != nil {
		return domain.InvoiceStripeRefund{}, fmt.Errorf("insert refund %s: %w", r.StripeRefundID, err)
	}
	return out, nil
}

func (s *PostgresStore) UpdateStripeRefundSnapshotTx(ctx context.Context, tx *sql.Tx, id string, snap domain.StripeRefundSnapshot) (domain.InvoiceStripeRefund, error) {
	var out domain.InvoiceStripeRefund
	if err := tx.QueryRowContext(ctx, `
		UPDATE invoice_stripe_refunds SET
			status = $2, reason = $3, failure_reason = $4,
			stripe_charge_id = COALESCE(NULLIF($5, ''), stripe_charge_id),
			last_event_at = $6, last_stripe_event_id = $7, updated_at = now()
		WHERE id = $1
		RETURNING `+stripeRefundCols,
		id, string(snap.Status), snap.Reason, snap.FailureReason, snap.StripeChargeID, snap.EventAt, snap.StripeEventID,
	).Scan(scanStripeRefundDest(&out)...); err != nil {
		return domain.InvoiceStripeRefund{}, fmt.Errorf("update refund %s: %w", snap.StripeRefundID, err)
	}
	return out, nil
}

func (s *PostgresStore) LinkStripeRefundTx(ctx context.Context, tx *sql.Tx, id, creditNoteID string) (domain.InvoiceStripeRefund, error) {
	var out domain.InvoiceStripeRefund
	err := tx.QueryRowContext(ctx, `
		UPDATE invoice_stripe_refunds SET
			outcome = 'credit_note', credit_note_id = $2, dismissed_at = NULL, updated_at = now()
		WHERE id = $1 AND outcome IN ('needs_review', 'dismissed')
		RETURNING `+stripeRefundCols, id, creditNoteID,
	).Scan(scanStripeRefundDest(&out)...)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.InvoiceStripeRefund{}, errs.InvalidState("this refund can no longer be recorded as a credit note")
	}
	if err != nil {
		return domain.InvoiceStripeRefund{}, fmt.Errorf("link refund row %s to credit note %s: %w", id, creditNoteID, err)
	}
	return out, nil
}

func (s *PostgresStore) DismissStripeRefundTx(ctx context.Context, tx *sql.Tx, id, note string, at time.Time) (domain.InvoiceStripeRefund, error) {
	var out domain.InvoiceStripeRefund
	err := tx.QueryRowContext(ctx, `
		UPDATE invoice_stripe_refunds SET
			outcome = 'dismissed', dismissed_note = $2, dismissed_at = $3, updated_at = now()
		WHERE id = $1 AND outcome = 'needs_review'
		RETURNING `+stripeRefundCols, id, note, at,
	).Scan(scanStripeRefundDest(&out)...)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.InvoiceStripeRefund{}, errs.InvalidState("only a refund that needs review can be dismissed")
	}
	if err != nil {
		return domain.InvoiceStripeRefund{}, fmt.Errorf("dismiss refund row %s: %w", id, err)
	}
	return out, nil
}

// ApplyRefundWebhookStatusTx carries the monotonic precedence of
// ApplyRefundWebhookStatusAudited (failed > succeeded > pending; an unknown
// row is never moved by a webhook), on the caller's tx.
func (s *PostgresStore) ApplyRefundWebhookStatusTx(ctx context.Context, tx *sql.Tx, stripeRefundID string, status domain.RefundStatus) (domain.CreditNote, bool, bool, error) {
	var cn domain.CreditNote
	err := tx.QueryRowContext(ctx, `
		UPDATE credit_notes SET refund_status=$1, updated_at=$2
		WHERE stripe_refund_id=$3
		  AND refund_status IS DISTINCT FROM $1
		  AND refund_status NOT IN ('failed', 'unknown')
		  AND ($1 = 'failed' OR refund_status <> 'succeeded')
		RETURNING id, credit_note_number, customer_id, invoice_id`,
		status, clock.Now(ctx), stripeRefundID).Scan(&cn.ID, &cn.CreditNoteNumber, &cn.CustomerID, &cn.InvoiceID)
	if err == nil {
		return cn, true, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.CreditNote{}, false, false, err
	}
	// Zero rows: no note holds the id, or the guard skipped a stale or
	// same-value write.
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM credit_notes WHERE stripe_refund_id=$1)`, stripeRefundID).Scan(&exists); err != nil {
		return domain.CreditNote{}, false, false, err
	}
	return domain.CreditNote{}, false, exists, nil
}

func (s *PostgresStore) ListStripeRefundsByInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceStripeRefund, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx,
		`SELECT `+stripeRefundCols+` FROM invoice_stripe_refunds WHERE invoice_id = $1 ORDER BY refund_created_at, id`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("list refunds made in Stripe: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []domain.InvoiceStripeRefund{}
	for rows.Next() {
		var r domain.InvoiceStripeRefund
		if err := rows.Scan(scanStripeRefundDest(&r)...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func (s *PostgresStore) StripeRefundsNeedingReviewByInvoices(ctx context.Context, tenantID string, invoiceIDs []string) (map[string]domain.InvoiceStripeRefund, error) {
	out := make(map[string]domain.InvoiceStripeRefund)
	if len(invoiceIDs) == 0 {
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT ON (invoice_id) `+stripeRefundCols+`
		FROM invoice_stripe_refunds
		WHERE invoice_id = ANY($1::text[]) AND outcome = $2
		ORDER BY invoice_id, refund_created_at DESC, id DESC`,
		invoiceIDs, string(domain.StripeRefundNeedsReview))
	if err != nil {
		return nil, fmt.Errorf("read refunds needing review: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r domain.InvoiceStripeRefund
		if err := rows.Scan(scanStripeRefundDest(&r)...); err != nil {
			return nil, err
		}
		out[r.InvoiceID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}
