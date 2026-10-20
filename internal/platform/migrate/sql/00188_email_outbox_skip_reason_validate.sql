-- +goose Up
-- Checks the rows already in email_outbox against the skip_reason CHECK
-- that 00187 added NOT VALID. Every row passes: a row written before 00187
-- holds NULL, and the CHECK has held for every row written since.
ALTER TABLE email_outbox VALIDATE CONSTRAINT email_outbox_skip_reason_check;

-- +goose Down
-- Nothing to undo: validating changes no row, and the Down of 00187 drops
-- the constraint.
SELECT 1;
