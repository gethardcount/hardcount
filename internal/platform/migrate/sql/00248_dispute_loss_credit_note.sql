-- +goose Up
-- The credit note that records a lost dispute on a top-up's own payment
-- (domain.InvoiceDispute.CreditNoteID). A loss is recorded only while it is
-- NULL, so a dispute lost twice, after an appeal, writes one note.
-- hardcount:blocking-ddl-ok: invoice_disputes and invoice_stripe_refunds hold a few rows per account
ALTER TABLE invoice_disputes ADD COLUMN credit_note_id TEXT REFERENCES credit_notes(id) ON DELETE RESTRICT;

-- A refund made in Stripe on a top-up is now recorded as a credit note, so
-- prepaid_commit is never written again. A row that still holds it is a
-- refund whose credit note is not written yet, which the linked create
-- records, so it moves to credit_note_not_written: the review reason for a
-- refund whose note could not be written when it arrived.
ALTER TABLE invoice_stripe_refunds DROP CONSTRAINT invoice_stripe_refunds_review_reason_check;
UPDATE invoice_stripe_refunds SET review_reason = 'credit_note_not_written' WHERE review_reason = 'prepaid_commit';
ALTER TABLE invoice_stripe_refunds ADD CONSTRAINT invoice_stripe_refunds_review_reason_check CHECK (review_reason IN (
    '', 'invoice_not_paid', 'currency_mismatch', 'seen_late', 'credit_note_refund_failed',
    'exceeds_card_refundable', 'exceeds_creditable', 'credit_note_not_written'));

-- +goose Down
-- Fails, and changes nothing, while a row holds credit_note_not_written.
ALTER TABLE invoice_stripe_refunds DROP CONSTRAINT invoice_stripe_refunds_review_reason_check;
ALTER TABLE invoice_stripe_refunds ADD CONSTRAINT invoice_stripe_refunds_review_reason_check CHECK (review_reason IN (
    '', 'invoice_not_paid', 'currency_mismatch', 'prepaid_commit', 'seen_late',
    'credit_note_refund_failed', 'exceeds_card_refundable', 'exceeds_creditable'));
ALTER TABLE invoice_disputes DROP COLUMN credit_note_id;
