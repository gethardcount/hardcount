-- +goose Up
-- Checks the rows already in invoices against the all-or-none CHECK that
-- 00185 added NOT VALID. Every row passes: 00185 added the four columns
-- empty.
ALTER TABLE invoices VALIDATE CONSTRAINT invoices_charge_started_all_or_none;

-- +goose Down
-- Nothing to undo: validating changes no row, and the Down of 00185 drops the
-- constraint.
SELECT 1;
