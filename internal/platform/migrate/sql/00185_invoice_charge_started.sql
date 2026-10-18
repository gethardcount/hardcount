-- +goose Up
-- The charge record (ADR-127, #976). Just before Velox calls Stripe, the
-- charge writes these four columns on the invoice in a commit of its own, and
-- clears them once the result is saved or nothing was sent. A crash, a deploy
-- or a deadline that ends during the Stripe call leaves them set. While they
-- are set no path charges the invoice again, and the reconciler asks Stripe
-- about the charge from 10 minutes on.
--   charge_started_at              database now() when the charge was recorded
--   charge_started_seq             the charge_attempt_seq the key was built from
--   charge_started_key             the exact idempotency key Stripe received
--   charge_started_stripe_customer the Stripe customer the charge was sent for
-- All four are nullable with no default, so the ADD COLUMNs are instant. The
-- CHECK keeps them all set or all NULL. It is added NOT VALID, so it holds for
-- new rows at once; 00186 checks the rows already there.
ALTER TABLE invoices ADD COLUMN charge_started_at TIMESTAMPTZ;
ALTER TABLE invoices ADD COLUMN charge_started_seq BIGINT;
ALTER TABLE invoices ADD COLUMN charge_started_key TEXT;
ALTER TABLE invoices ADD COLUMN charge_started_stripe_customer TEXT;
ALTER TABLE invoices ADD CONSTRAINT invoices_charge_started_all_or_none CHECK (
    (charge_started_at IS NULL AND charge_started_seq IS NULL
        AND charge_started_key IS NULL AND charge_started_stripe_customer IS NULL)
    OR (charge_started_at IS NOT NULL AND charge_started_seq IS NOT NULL
        AND charge_started_key IS NOT NULL AND charge_started_stripe_customer IS NOT NULL)
) NOT VALID;

-- +goose Down
-- Rollback runs only with APP_ENV=local. A charge still recorded loses its
-- record, which is what a binary from before this migration ignores anyway.
ALTER TABLE invoices DROP CONSTRAINT invoices_charge_started_all_or_none;
ALTER TABLE invoices DROP COLUMN charge_started_stripe_customer;
ALTER TABLE invoices DROP COLUMN charge_started_key;
ALTER TABLE invoices DROP COLUMN charge_started_seq;
ALTER TABLE invoices DROP COLUMN charge_started_at;
