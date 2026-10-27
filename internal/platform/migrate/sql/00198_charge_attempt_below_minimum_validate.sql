-- +goose Up
-- Checks the rows already in invoice_charge_attempts against the CHECK 00197
-- re-added NOT VALID. Every row passes: 00197 only added a value.
ALTER TABLE invoice_charge_attempts VALIDATE CONSTRAINT invoice_charge_attempts_outcome_check;

-- +goose Down
-- Nothing to undo: validating changes no row, and the Down of 00197 puts
-- the old constraint back.
SELECT 1;
