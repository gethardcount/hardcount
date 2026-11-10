-- +goose Up
-- A credit note that owes Stripe Tax a reversal now carries
-- tax_reversal_pending from the statement that issues it, and the pending
-- list is that marker alone. Before this release the marker was written only
-- after a reversal call failed, and the list also found, for 24 hours, an
-- issued note with no reversal stored on an invoice that had a Stripe tax
-- transaction (a note whose call never ran, or whose marker write failed).
--
-- This marks the notes that second arm would still have listed when the
-- upgrade runs, so the upgrade drops nothing the earlier version would have
-- recovered: issued, with an amount, no reversal stored, not marked, last
-- written within 24 hours, not on a test clock, on an invoice Stripe Tax
-- taxed, with tax and a recorded tax transaction.
--
-- It does not touch updated_at: that column is the note's age for the
-- refunds-needing-attention rule.
--
-- Not marked here: a note issued while its invoice's tax was not yet recorded
-- at Stripe, and anything older than 24 hours. The earlier version would not
-- have reversed those either. CHANGELOG.md gives the wider UPDATE for an
-- operator who wants them.
--
-- credit_notes and invoices force row-level security, and the user that runs
-- migrations may be subject to it, so the file turns the bypass on for its
-- own transaction.
SET LOCAL app.bypass_rls = 'on';
UPDATE credit_notes cn
SET tax_reversal_pending = true
WHERE cn.status = 'issued'
  AND NOT cn.tax_reversal_pending
  AND COALESCE(cn.tax_transaction_id, '') = ''
  AND cn.total_cents > 0
  AND cn.is_simulated = false
  AND cn.updated_at > now() - interval '24 hours'
  AND EXISTS (
        SELECT 1 FROM invoices i
        WHERE i.id = cn.invoice_id
          AND i.tenant_id = cn.tenant_id
          AND i.tax_provider = 'stripe_tax'
          AND i.tax_amount_cents > 0
          AND COALESCE(i.tax_transaction_id, '') <> ''
      );

-- +goose Down
-- Nothing to undo: a marked note is one that owes its reversal, and the
-- earlier version's list reads the marker too.
SELECT 1;
