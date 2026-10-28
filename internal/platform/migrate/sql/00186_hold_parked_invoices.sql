-- +goose Up
-- A parked invoice (a charge whose unclear answer named no PaymentIntent)
-- keeps its claim's charge hold, and the stale-hold sweep finishes it from
-- that hold: it records a payment Stripe shows, and with none 24 hours after
-- the hold it puts the invoice back where a charge picks it up (REC-5).
--
-- Only an install that ran main between the stale-hold sweep (#1299,
-- 2026-10-19) and REC-5 has such parks; no release has one without the
-- other. That code cleared a park's hold at its first check when Stripe
-- showed nothing, and left it parked for the Stripe Search sweep, which
-- REC-5 deletes. Without a hold no sweep would ever look at such a park
-- again. Where no park lacks a hold this changes nothing. This gives each one a hold at
-- its parked attempt's time: written by the database once Stripe had
-- answered, so the sweep's list (from an hour before the hold) covers the
-- request, and its 24 hours count from the attempt. A park whose attempt row
-- was lost (a best-effort write) gets 2026-01-01, before any charge it can
-- have had.
--
-- invoices and invoice_charge_attempts force row-level security, and the
-- user that runs migrations may be subject to it, so the file turns the
-- bypass on for its own transaction.
SET LOCAL app.bypass_rls = 'on';
UPDATE invoices i SET auto_charge_claimed_until = COALESCE(
    (SELECT max(a.created_at) FROM invoice_charge_attempts a
     WHERE a.invoice_id = i.id AND a.outcome = 'unknown' AND a.stripe_payment_intent_id = ''),
    '2026-01-01T00:00:00Z')
WHERE i.payment_status = 'unknown' AND COALESCE(i.stripe_payment_intent_id, '') = ''
  AND i.status IN ('finalized', 'uncollectible')
  AND i.auto_charge_claimed_until IS NULL;

-- +goose Down
-- Nothing to undo: a restored hold only makes the sweep check the park
-- again, and Hardcount without REC-5 clears it at that check.
SELECT 1;
