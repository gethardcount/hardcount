-- +goose Up
-- Checks the rows already there against the CHECKs 00203 added NOT VALID.
-- Every row passes: 00203 converted the out_of_band: marker before adding
-- them.
ALTER TABLE invoices VALIDATE CONSTRAINT invoices_stripe_payment_intent_id_check;
ALTER TABLE invoices VALIDATE CONSTRAINT invoices_paid_out_of_band_check;
ALTER TABLE invoice_charge_attempts VALIDATE CONSTRAINT invoice_charge_attempts_stripe_payment_intent_id_check;

-- +goose Down
-- Nothing to undo: validating changes no row, and the Down of 00203 drops
-- the constraints.
SELECT 1;
