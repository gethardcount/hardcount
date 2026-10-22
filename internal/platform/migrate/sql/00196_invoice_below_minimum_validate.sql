-- +goose Up
-- Checks the rows already in invoice_dunning_runs against the CHECK 00195
-- re-added NOT VALID. Every row passes: 00195 only added a value.
ALTER TABLE invoice_dunning_runs VALIDATE CONSTRAINT invoice_dunning_runs_resolution_check;

-- +goose Down
-- Nothing to undo: validating changes no row, and the Down of 00195 puts
-- the old constraint back.
SELECT 1;
