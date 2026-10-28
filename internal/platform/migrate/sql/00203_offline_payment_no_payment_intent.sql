-- +goose Up
-- An offline payment (money the operator records as received outside
-- Hardcount: a bank transfer, a cheque) stops writing a fake PaymentIntent id.
-- Before this, it wrote "out_of_band:<paid_at>" into
-- invoices.stripe_payment_intent_id, and the settle's attempt upsert wrote
-- a succeeded invoice_charge_attempts row under the same fake id. Now the
-- invoice has no PaymentIntent and paid_out_of_band is true (Stripe's name
-- for the same fact).
--
-- Existing rows are converted: the marker becomes paid_out_of_band = true
-- and a NULL PaymentIntent, and the fake attempt rows are deleted (a charge
-- attempt is a charge sent to Stripe, ADR-103; an offline payment sends
-- none). Nothing an invoice says changes: amounts, status, paid_at and
-- updated_at stay as they are. Under the clean break from 0.8.x only local
-- databases hold such rows.
--
-- The PaymentIntent columns then refuse the prefix, and an invoice paid out
-- of band refuses a PaymentIntent, so a reader that checks "has a
-- PaymentIntent" is always right about whether a card paid. NOT VALID here,
-- so adding them reads no rows; 00204 checks the rows already there.
--
-- Locks: the ADD COLUMN is instant, but its lock on invoices is held until
-- this file commits. Reads and writes of invoices therefore wait while the
-- UPDATE reads every invoice and the DELETE reads every charge attempt (no
-- index serves the prefix).
--
-- These tables force row-level security, and the user that runs migrations
-- may be subject to it, so the file turns the bypass on for its own
-- transaction.
SET LOCAL app.bypass_rls = 'on';

ALTER TABLE invoices ADD COLUMN paid_out_of_band BOOLEAN NOT NULL DEFAULT false;

UPDATE invoices SET paid_out_of_band = true, stripe_payment_intent_id = NULL
WHERE starts_with(stripe_payment_intent_id, 'out_of_band:');

DELETE FROM invoice_charge_attempts
WHERE starts_with(stripe_payment_intent_id, 'out_of_band:');

ALTER TABLE invoices ADD CONSTRAINT invoices_stripe_payment_intent_id_check
    CHECK (NOT starts_with(stripe_payment_intent_id, 'out_of_band:')) NOT VALID;
ALTER TABLE invoices ADD CONSTRAINT invoices_paid_out_of_band_check
    CHECK (NOT paid_out_of_band OR stripe_payment_intent_id IS NULL) NOT VALID;
ALTER TABLE invoice_charge_attempts ADD CONSTRAINT invoice_charge_attempts_stripe_payment_intent_id_check
    CHECK (NOT starts_with(stripe_payment_intent_id, 'out_of_band:')) NOT VALID;

-- +goose Down
-- Rollback runs only with APP_ENV=local. It puts the marker back, in the
-- form the code before this migration wrote and reads, and the schema as it
-- was. The deleted attempt rows are not rebuilt: the code before this
-- migration read the marker from the invoice, not from them.
SET LOCAL app.bypass_rls = 'on';

ALTER TABLE invoice_charge_attempts DROP CONSTRAINT invoice_charge_attempts_stripe_payment_intent_id_check;
ALTER TABLE invoices DROP CONSTRAINT invoices_paid_out_of_band_check;
ALTER TABLE invoices DROP CONSTRAINT invoices_stripe_payment_intent_id_check;

UPDATE invoices
SET stripe_payment_intent_id = 'out_of_band:' || to_char(paid_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
WHERE paid_out_of_band;

ALTER TABLE invoices DROP COLUMN paid_out_of_band;
