-- +goose Up
-- Checks the rows already in subscriptions against the CHECK 00193 added NOT
-- VALID. Every row passes: 00193 added the column with every row NULL.
ALTER TABLE subscriptions VALIDATE CONSTRAINT subscriptions_pause_collection_invoice_id_check;

-- +goose Down
-- Nothing to undo: validating changes no row, and the Down of 00193 drops
-- the constraint.
SELECT 1;
