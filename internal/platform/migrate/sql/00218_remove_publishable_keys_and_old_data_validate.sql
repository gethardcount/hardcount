-- +goose Up
-- Checks the rows already there against the CHECKs 00217 narrowed NOT
-- VALID. On a fresh install every row passes.
ALTER TABLE api_keys VALIDATE CONSTRAINT api_keys_key_type_check;
ALTER TABLE invoice_line_items VALIDATE CONSTRAINT invoice_line_items_line_type_check;
ALTER TABLE invoice_dunning_runs VALIDATE CONSTRAINT invoice_dunning_runs_resolution_check;

-- +goose Down
-- Nothing to undo: validating changes no row, and the Down of 00217 puts
-- the old constraints back.
SELECT 1;
