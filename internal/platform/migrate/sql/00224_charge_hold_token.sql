-- +goose Up
-- The charge hold's token (ADR-137). A charge claim writes charge_hold_token
-- equal to the hold it writes (auto_charge_claimed_until). A charge then
-- creates its PaymentIntent unconfirmed, puts it on the invoice as an unknown
-- outcome (a compare-and-set on the hold), and only then confirms it.
--
-- So the stale-hold sweep can tell, for a hold it finds:
--   - token equal to the hold: taken by a charger that stamps the invoice
--     before it confirms. On an invoice still open for a charge, that charger
--     never confirmed, and the hold is cleared with no Stripe call;
--   - token not equal (or NULL): taken by an earlier version, which charged
--     in one call and put nothing on the invoice first. The sweep keeps such
--     a hold and reports it instead of guessing.
--
-- A stale token left by an earlier claim never equals a later hold, so
-- clearing it is not needed.
--
-- The upgrade check. The previous version charged in one call and saved
-- nothing before its charge reached Stripe, so a charge hold or a parked
-- charge it left cannot be resolved by this version, which reads the
-- PaymentIntent the invoice names. The upgrade refuses to run while any exists: keep the
-- previous version running until its billing tick has finished them (a charge
-- hold clears within about an hour of its charge, a parked charge within 24
-- hours), then stop it and run the upgrade again.
--
-- Only invoices still open count: on a paid, voided or written-off invoice
-- nothing charges again, and this version clears such a hold (a written-off
-- park keeps its banner for a person). A hold the previous version keeps for
-- good on an open invoice (Stripe cannot be asked: the key revoked, the
-- customer deleted there) never clears on its own: write the invoice off on
-- the previous version, or fix the Stripe connection, first.
--
-- Hardcount has no production deployments at this change, so this check is
-- the whole upgrade path (ADR-137); the sweep keeps any such hold that gets
-- past it for a person, never guessing.
--
-- invoices forces row-level security, and the user that runs migrations may
-- be subject to it (an owner that is not a superuser sees no rows), so the
-- file turns the bypass on for its own transaction. Without it the check
-- counted nothing and let the upgrade through (review 4).
SET LOCAL app.bypass_rls = 'on';
-- +goose StatementBegin
DO $$
DECLARE
	n integer;
BEGIN
	SELECT count(*) INTO n FROM invoices
	WHERE status = 'finalized'
	  AND (auto_charge_claimed_until IS NOT NULL
	       OR (payment_status = 'unknown' AND COALESCE(stripe_payment_intent_id, '') = ''));
	IF n > 0 THEN
		RAISE EXCEPTION 'Hardcount upgrade stopped: % invoice(s) still open have a charge in progress or a parked charge from the previous version. Keep the previous version running until none is left (charge holds clear within about an hour, parked charges within 24 hours), then stop it and run the upgrade again. See docs/upgrades.md.', n;
	END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE invoices ADD COLUMN charge_hold_token TIMESTAMPTZ;

-- +goose Down
ALTER TABLE invoices DROP COLUMN charge_hold_token;
