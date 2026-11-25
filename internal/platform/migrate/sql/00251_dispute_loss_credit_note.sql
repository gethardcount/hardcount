-- +goose Up
-- The credit note that records a lost dispute on a top-up's own payment
-- (domain.InvoiceDispute.CreditNoteID). A loss is recorded only while it is
-- NULL, so a dispute lost twice, after an appeal, writes one note.
-- hardcount:blocking-ddl-ok: invoice_disputes and invoice_stripe_refunds hold a few rows per account
ALTER TABLE invoice_disputes ADD COLUMN credit_note_id TEXT REFERENCES credit_notes(id) ON DELETE RESTRICT;
-- Every credit-note read asks whether a dispute names the note.
CREATE INDEX idx_invoice_disputes_credit_note ON invoice_disputes (credit_note_id) WHERE credit_note_id IS NOT NULL;

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

-- A paid invoice from before top-ups that sold prepaid credit and nothing
-- else, untaxed, with its commit grant, is a top-up: credit notes on it now
-- take the credit back (commit relief), since the credit-note gate reads
-- billing_reason. Its line keeps credit_valid_days NULL. A taxed one, one
-- with another line, one not paid and one with no grant stay as they are
-- (CHANGELOG, Before upgrading). A commit line is one with
-- commit_granted_cents set, as InvoiceLineItem.IsCommitLine reads it.
UPDATE invoices i SET billing_reason = 'topup'
WHERE i.status = 'paid'
  AND (i.billing_reason IS NULL OR i.billing_reason = 'manual')
  AND i.tax_amount_cents = 0
  AND (SELECT count(*) FROM invoice_line_items l WHERE l.invoice_id = i.id) = 1
  AND EXISTS (SELECT 1 FROM invoice_line_items l
              WHERE l.invoice_id = i.id AND l.commit_granted_cents IS NOT NULL AND l.commit_granted_cents > 0)
  AND EXISTS (SELECT 1 FROM customer_credit_ledger g
              WHERE g.tenant_id = i.tenant_id AND g.source_invoice_id = i.id AND g.grant_kind = 'commit');

-- +goose Down
-- The invoices the Up made top-ups stay top-ups: the schema before it
-- allows a paid top-up.
-- Fails, and changes nothing, while a row holds credit_note_not_written.
ALTER TABLE invoice_stripe_refunds DROP CONSTRAINT invoice_stripe_refunds_review_reason_check;
ALTER TABLE invoice_stripe_refunds ADD CONSTRAINT invoice_stripe_refunds_review_reason_check CHECK (review_reason IN (
    '', 'invoice_not_paid', 'currency_mismatch', 'prepaid_commit', 'seen_late',
    'credit_note_refund_failed', 'exceeds_card_refundable', 'exceeds_creditable'));
DROP INDEX idx_invoice_disputes_credit_note;
ALTER TABLE invoice_disputes DROP COLUMN credit_note_id;
