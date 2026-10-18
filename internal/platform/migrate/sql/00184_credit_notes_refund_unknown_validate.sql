-- +goose Up
-- Checks the rows already in credit_notes against the refund_status CHECK
-- that 00183 added NOT VALID. Every row passes: 00183 only adds a value.
ALTER TABLE credit_notes VALIDATE CONSTRAINT credit_notes_refund_status_check;

-- +goose Down
-- Nothing to undo: validating changes no row, and the Down of 00183
-- replaces the constraint.
SELECT 1;
